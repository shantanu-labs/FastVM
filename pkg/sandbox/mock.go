package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/shantanuroy/airbox/pkg/api"
	"github.com/sirupsen/logrus"
)

// pythonWorkerScript is the in-memory Zygote server pre-loading common modules.
const pythonWorkerScript = `
import sys, json, io, traceback, time, os

# Pre-load standard libraries to eliminate import latency
import math, re, datetime

def handle(req):
    code = req.get("code", "")
    env = req.get("env", {})
    stdin_data = req.get("stdin", "")

    for k, v in env.items():
        os.environ[k] = str(v)

    old_stdout, old_stderr, old_stdin = sys.stdout, sys.stderr, sys.stdin
    stdout_buf = io.StringIO()
    stderr_buf = io.StringIO()
    sys.stdout = stdout_buf
    sys.stderr = stderr_buf
    sys.stdin = io.StringIO(stdin_data)

    start = time.perf_counter()
    exit_code = 0
    err_str = ""

    glob = {
        "__name__": "__main__",
        "__file__": "sandbox.py",
        "math": math,
        "os": os,
        "sys": sys,
        "time": time,
    }

    try:
        compiled = compile(code, "<sandbox>", "exec")
        exec(compiled, glob)
    except SystemExit as e:
        exit_code = e.code if isinstance(e.code, int) else 0
    except Exception as e:
        exit_code = 1
        err_str = traceback.format_exc()
        sys.stderr.write(err_str)
    finally:
        sys.stdout = old_stdout
        sys.stderr = old_stderr
        sys.stdin = old_stdin

    dur_ms = (time.perf_counter() - start) * 1000.0
    return {
        "stdout": stdout_buf.getvalue(),
        "stderr": stderr_buf.getvalue(),
        "exit_code": exit_code,
        "duration_ms": max(1, int(dur_ms)),
        "error": err_str
    }

for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        req = json.loads(line)
        res = handle(req)
        print(json.dumps(res), flush=True)
    except Exception as e:
        print(json.dumps({"error": str(e), "exit_code": 1, "duration_ms": 1}), flush=True)
`

type pyWorker struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	mu     sync.Mutex
	alive  bool
}

func newPyWorker() (*pyWorker, error) {
	cmd := exec.Command("python3", "-u", "-c", pythonWorkerScript)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, err
	}

	return &pyWorker{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReader(stdoutPipe),
		alive:  true,
	}, nil
}

func (w *pyWorker) kill() {
	if w.cmd != nil && w.cmd.Process != nil {
		_ = w.cmd.Process.Kill()
	}
}

func (w *pyWorker) close() {
	w.kill()
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.alive {
		return
	}
	w.alive = false
	_ = w.stdin.Close()
	_ = w.cmd.Wait()
}

// MockHypervisor provides local execution simulation on macOS, non-Linux development machines,
// and unprivileged CI test runners where Firecracker/KVM assets are not present.
type MockHypervisor struct {
	logger     *logrus.Logger
	workerPool chan *pyWorker
	poolSize   int
}

// NewMockHypervisor creates a MockHypervisor with a pre-warmed Python Zygote worker pool.
func NewMockHypervisor(logger *logrus.Logger) *MockHypervisor {
	poolSize := 10
	h := &MockHypervisor{
		logger:     logger,
		workerPool: make(chan *pyWorker, poolSize),
		poolSize:   poolSize,
	}

	// Pre-warm the pool of Python Zygote workers asynchronously
	for i := 0; i < poolSize; i++ {
		go func() {
			w, err := newPyWorker()
			if err == nil {
				h.workerPool <- w
			}
		}()
	}

	return h
}

func (h *MockHypervisor) acquireWorker() *pyWorker {
	select {
	case w := <-h.workerPool:
		return w
	case <-time.After(15 * time.Millisecond):
		// Spawn on-demand if pool empty
		w, err := newPyWorker()
		if err == nil {
			return w
		}
		return nil
	}
}

func (h *MockHypervisor) releaseWorker(w *pyWorker, healthy bool) {
	if !healthy || w == nil {
		if w != nil {
			w.close()
		}
		// Replenish worker in background
		go func() {
			nw, err := newPyWorker()
			if err == nil {
				h.workerPool <- nw
			}
		}()
		return
	}

	select {
	case h.workerPool <- w:
	default:
		w.close()
	}
}

func (h *MockHypervisor) Name() string {
	return "mock-local"
}

func (h *MockHypervisor) IsSupported() bool {
	return true
}

func (h *MockHypervisor) Close() error {
	for {
		select {
		case w := <-h.workerPool:
			w.close()
		default:
			return nil
		}
	}
}

type MockInstance struct {
	id     string
	opts   SandboxOpts
	logger *logrus.Entry
	hyp    *MockHypervisor
}

func (h *MockHypervisor) Start(ctx context.Context, opts SandboxOpts) (Instance, error) {
	log := h.logger.WithField("sandbox_id", opts.SandboxID)
	return &MockInstance{
		id:     opts.SandboxID,
		opts:   opts,
		logger: log,
		hyp:    h,
	}, nil
}

func (m *MockInstance) ID() string {
	return m.id
}

type pyReq struct {
	Code  string            `json:"code"`
	Stdin string            `json:"stdin,omitempty"`
	Env   map[string]string `json:"env,omitempty"`
}

type pyResp struct {
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   int    `json:"exit_code"`
	DurationMs int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
}

func (m *MockInstance) Execute(ctx context.Context, code string, stdin string, env map[string]string) (*api.ExecuteResponse, error) {
	start := time.Now()
	lang := strings.ToLower(m.opts.Language)
	if lang == "" {
		lang = "python"
	}

	// 1. Fast-Path: Use Pre-Warmed Zygote Worker Pool for Python
	if lang == "python" || lang == "python3" {
		worker := m.hyp.acquireWorker()
		if worker != nil {
			type execResult struct {
				resp pyResp
				err  error
			}
			resCh := make(chan execResult, 1)

			go func() {
				worker.mu.Lock()
				defer worker.mu.Unlock()

				reqBytes, err := json.Marshal(pyReq{Code: code, Stdin: stdin, Env: env})
				if err != nil {
					resCh <- execResult{err: err}
					return
				}
				reqBytes = append(reqBytes, '\n')

				if _, err := worker.stdin.Write(reqBytes); err != nil {
					resCh <- execResult{err: err}
					return
				}

				line, err := worker.stdout.ReadBytes('\n')
				if err != nil {
					resCh <- execResult{err: err}
					return
				}

				var pRes pyResp
				if err := json.Unmarshal(line, &pRes); err != nil {
					resCh <- execResult{err: err}
					return
				}

				resCh <- execResult{resp: pRes}
			}()

			select {
			case <-ctx.Done():
				// Timeout exceeded! Immediately kill the running process
				worker.kill()
				go func() {
					worker.close()
					m.hyp.releaseWorker(worker, false)
				}()
				return &api.ExecuteResponse{
					SandboxID:  m.id,
					IsTimeout:  true,
					DurationMs: time.Since(start).Milliseconds(),
					Error:      "execution timeout exceeded",
				}, nil

			case r := <-resCh:
				if r.err == nil {
					m.hyp.releaseWorker(worker, true)
					return &api.ExecuteResponse{
						SandboxID:  m.id,
						Stdout:     r.resp.Stdout,
						Stderr:     r.resp.Stderr,
						ExitCode:   r.resp.ExitCode,
						DurationMs: time.Since(start).Milliseconds(),
						IsTimeout:  false,
						Error:      r.resp.Error,
					}, nil
				}
				// Errored worker: destroy and replace
				m.hyp.releaseWorker(worker, false)
			}
		}
	}

	// 2. Fallback Path: Subprocess invocation for other languages (or if worker pool exhausted)
	var cmd *exec.Cmd
	switch lang {
	case "bash":
		cmd = exec.CommandContext(ctx, "bash", "-c", code)
	case "sh":
		cmd = exec.CommandContext(ctx, "sh", "-c", code)
	case "node", "javascript":
		cmd = exec.CommandContext(ctx, "node", "-e", code)
	default:
		cmd = exec.CommandContext(ctx, "python3", "-c", code)
	}

	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}

	err := cmd.Run()
	durationMs := time.Since(start).Milliseconds()

	isTimeout := false
	if ctx.Err() == context.DeadlineExceeded {
		isTimeout = true
	}

	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	return &api.ExecuteResponse{
		SandboxID:  m.id,
		Stdout:     stdoutBuf.String(),
		Stderr:     stderrBuf.String(),
		ExitCode:   exitCode,
		DurationMs: durationMs,
		IsTimeout:  isTimeout,
		Error: func() string {
			if err != nil {
				return err.Error()
			}
			return ""
		}(),
	}, nil
}

func (m *MockInstance) Branch(ctx context.Context, newID string) (Instance, error) {
	m.logger.Infof("[Mock] Branched sandbox %s -> %s", m.id, newID)
	newOpts := m.opts
	newOpts.SandboxID = newID
	return &MockInstance{
		id:     newID,
		opts:   newOpts,
		logger: m.logger.WithField("sandbox_id", newID),
		hyp:    m.hyp,
	}, nil
}

func (m *MockInstance) Stop() error {
	return nil
}
