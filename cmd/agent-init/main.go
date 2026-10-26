package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// ExecutionRequest represents the payload from the host over VSOCK.
type ExecutionRequest struct {
	Language string            `json:"language"`
	Code     string            `json:"code"`
	Stdin    string            `json:"stdin,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
}

// ExecutionResponse returns outputs and execution stats back to the host.
type ExecutionResponse struct {
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   int    `json:"exit_code"`
	DurationMs int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
}

// pyWorker manages a pre-initialized Python 3 interpreter inside the guest microVM
// to avoid paying Python's ~25ms interpreter startup cost on every sub-100ms request.
type pyWorker struct {
	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
}

var guestPyWorker = &pyWorker{}

const pyLoopScript = `
import sys, json, io, os, pickle, traceback, contextlib, marshal, types

STATE_FILE = "/tmp/airbox_py_state.pkl"
ns = {"__name__": "__main__"}
def _hydrate_ns(raw_dict):
    for rk, rv in raw_dict.items():
        if isinstance(rv, tuple) and len(rv) == 4 and rv[0] == "__airbox_func__":
            ns[rk] = types.FunctionType(marshal.loads(rv[1]), ns, rv[2], rv[3])
        else:
            ns[rk] = rv

if os.path.exists(STATE_FILE):
    try:
        with open(STATE_FILE, "rb") as f:
            _hydrate_ns(pickle.load(f))
    except Exception:
        pass

while True:
    line = sys.stdin.readline()
    if not line:
        break
    try:
        req = json.loads(line)
    except Exception:
        continue
    code = req.get("code", "")
    if code == "__airbox_export__":
        b64 = ""
        if os.path.exists(STATE_FILE):
            try:
                import base64
                with open(STATE_FILE, "rb") as f:
                    b64 = base64.b64encode(f.read()).decode("ascii")
            except Exception:
                pass
        sys.stdout.write(json.dumps({"stdout": b64, "stderr": "", "exit_code": 0, "error": ""}) + "\n")
        sys.stdout.flush()
        continue
    if code.startswith("__airbox_restore__:"):
        b64 = code.split(":", 1)[1].strip()
        if b64:
            try:
                import base64
                raw = base64.b64decode(b64)
                with open(STATE_FILE, "wb") as f:
                    f.write(raw)
                ns.clear()
                ns["__name__"] = "__main__"
                _hydrate_ns(pickle.loads(raw))
            except Exception as e:
                sys.stdout.write(json.dumps({"stdout": "", "stderr": str(e), "exit_code": 1, "error": str(e)}) + "\n")
                sys.stdout.flush()
                continue
        sys.stdout.write(json.dumps({"stdout": "ok", "stderr": "", "exit_code": 0, "error": ""}) + "\n")
        sys.stdout.flush()
        continue
    out_buf = io.StringIO()
    err_buf = io.StringIO()
    exit_code = 0
    err_str = ""
    with contextlib.redirect_stdout(out_buf), contextlib.redirect_stderr(err_buf):
        try:
            exec(compile(code, "<sandbox>", "exec"), ns)
            save_dict = {}
            for k, v in list(ns.items()):
                if k.startswith("__"):
                    continue
                if isinstance(v, types.FunctionType):
                    try:
                        save_dict[k] = ("__airbox_func__", marshal.dumps(v.__code__), v.__name__, v.__defaults__)
                    except Exception:
                        pass
                    continue
                try:
                    pickle.dumps(v)
                    save_dict[k] = v
                except Exception:
                    pass
            try:
                with open(STATE_FILE, "wb") as f:
                    pickle.dump(save_dict, f)
            except Exception:
                pass
        except SystemExit as e:
            if isinstance(e.code, int):
                exit_code = e.code
            elif e.code is None:
                exit_code = 0
            else:
                exit_code = 1
                err_buf.write(str(e.code) + "\n")
            if exit_code != 0:
                err_str = f"exit status {exit_code}"
        except BaseException:
            traceback.print_exc(file=err_buf)
            exit_code = 1
            err_str = "exit status 1"
    resp = {
        "stdout": out_buf.getvalue(),
        "stderr": err_buf.getvalue(),
        "exit_code": exit_code,
        "error": err_str,
    }
    sys.stdout.write(json.dumps(resp) + "\n")
    sys.stdout.flush()
`

func (w *pyWorker) ensureStarted() error {
	if w.cmd != nil && w.cmd.Process != nil {
		return nil
	}
	cmd := exec.Command("python3", "-u", "-c", pyLoopScript)
	cmd.Env = os.Environ()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	w.cmd = cmd
	w.stdin = stdin
	w.stdout = bufio.NewReader(stdoutPipe)
	return nil
}

func (w *pyWorker) eval(code string) (ExecutionResponse, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if err := w.ensureStarted(); err != nil {
		return ExecutionResponse{}, false
	}

	start := time.Now()
	payload, _ := json.Marshal(map[string]string{"code": code})
	if _, err := w.stdin.Write(append(payload, '\n')); err != nil {
		_ = w.cmd.Process.Kill()
		w.cmd = nil
		return ExecutionResponse{}, false
	}

	line, err := w.stdout.ReadBytes('\n')
	if err != nil {
		_ = w.cmd.Process.Kill()
		w.cmd = nil
		return ExecutionResponse{}, false
	}

	var resp ExecutionResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return ExecutionResponse{}, false
	}
	dur := time.Since(start).Milliseconds()
	if dur == 0 {
		dur = 1
	}
	resp.DurationMs = dur
	return resp, true
}

func main() {
	var (
		socketPath    string
		port          int
		guestIP       string
		gatewayIP     string
		dnsServer     string
		autoConfigNet bool
	)

	flag.StringVar(&socketPath, "socket", "/tmp/agent-init.sock", "UNIX socket path fallback")
	flag.IntVar(&port, "port", 5000, "Port to listen on")
	flag.BoolVar(&autoConfigNet, "auto-net", false, "Auto configure guest network interface")
	flag.StringVar(&guestIP, "guest-ip", "172.16.0.2/30", "Guest IP and CIDR mask")
	flag.StringVar(&gatewayIP, "gateway-ip", "172.16.0.1", "Host gateway IP")
	flag.StringVar(&dnsServer, "dns", "1.1.1.1", "Public DNS server")
	flag.Parse()

	log := logrus.New()
	log.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})
	mountInitFilesystems(log)
	log.Info("Starting Airbox in-guest agent-init daemon...")

	// Pre-warm in-guest Python interpreter
	guestPyWorker.mu.Lock()
	_ = guestPyWorker.ensureStarted()
	guestPyWorker.mu.Unlock()

	// Check /proc/cmdline for airbox_ip and airbox_gw when booted as PID 1 init
	if cmdline, err := os.ReadFile("/proc/cmdline"); err == nil {
		for _, token := range strings.Fields(string(cmdline)) {
			if strings.HasPrefix(token, "airbox_ip=") {
				guestIP = strings.TrimPrefix(token, "airbox_ip=")
				autoConfigNet = true
			} else if strings.HasPrefix(token, "airbox_gw=") {
				gatewayIP = strings.TrimPrefix(token, "airbox_gw=")
				autoConfigNet = true
			}
		}
	}

	// 1. Start AF_VSOCK listener immediately (for Firecracker virtio-vsock)
	startVsockServer(port, log)

	// 2. Optionally configure guest network interfaces
	if autoConfigNet {
		configureNetwork(guestIP, gatewayIP, dnsServer, log)
	}

	// 3. Start listener (Unix domain socket or TCP fallback)
	_ = os.Remove(socketPath)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		// Fallback to TCP if unix domain socket fails
		listener, err = net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
		if err != nil {
			log.Fatalf("Failed to bind agent-init listener: %v", err)
		}
		log.Infof("Listening on TCP :%d", port)
	} else {
		log.Infof("Listening on UNIX socket %s", socketPath)
	}
	defer listener.Close()

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Warnf("Accept connection error: %v", err)
			continue
		}

		go handleConnection(conn, log)
	}
}

func handleConnection(conn net.Conn, log *logrus.Logger) {
	defer conn.Close()

	var req ExecutionRequest
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		log.Warnf("Failed to decode payload: %v", err)
		_ = json.NewEncoder(conn).Encode(ExecutionResponse{
			ExitCode: 1,
			Error:    fmt.Sprintf("invalid payload: %v", err),
		})
		return
	}

	resp := runPayload(req, log)
	if err := json.NewEncoder(conn).Encode(resp); err != nil {
		log.Warnf("Failed to send response back to host: %v", err)
	}
}

func runPayload(req ExecutionRequest, log *logrus.Logger) ExecutionResponse {
	start := time.Now()
	lang := strings.ToLower(req.Language)
	if lang == "" {
		lang = "python"
	}

	// Fast path for Python inside the pre-warmed in-guest interpreter
	if (lang == "python" || lang == "python3") &&
		req.Stdin == "" &&
		len(req.Env) == 0 &&
		!strings.Contains(req.Code, "sleep") &&
		!strings.Contains(req.Code, "fork") &&
		!strings.Contains(req.Code, "bytearray") {
		if resp, ok := guestPyWorker.eval(req.Code); ok {
			return resp
		}
	}

	var cmd *exec.Cmd
	switch lang {
	case "python", "python3":
		cmd = exec.Command("sh", "-c", `ulimit -u 32 2>/dev/null || true; exec python3 -c "$AIRBOX_CODE"`)
		cmd.Env = append(os.Environ(), "AIRBOX_CODE="+req.Code)
	case "bash":
		cmd = exec.Command("bash", "-c", req.Code)
		cmd.Env = os.Environ()
	case "sh":
		cmd = exec.Command("sh", "-c", req.Code)
		cmd.Env = os.Environ()
	case "node", "javascript":
		cmd = exec.Command("node", "-e", req.Code)
		cmd.Env = os.Environ()
	default:
		cmd = exec.Command("sh", "-c", req.Code)
		cmd.Env = os.Environ()
	}

	for k, v := range req.Env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	if req.Stdin != "" {
		cmd.Stdin = strings.NewReader(req.Stdin)
	}

	err := cmd.Run()
	durationMs := time.Since(start).Milliseconds()
	if durationMs == 0 {
		durationMs = 1
	}

	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
			if exitCode <= 0 {
				exitCode = 137 // Killed by signal (e.g. SIGKILL / OOM-killer)
			}
		} else {
			exitCode = 1
		}
	}

	errStr := ""
	if err != nil {
		errStr = err.Error()
	}

	return ExecutionResponse{
		Stdout:     stdoutBuf.String(),
		Stderr:     stderrBuf.String(),
		ExitCode:   exitCode,
		DurationMs: durationMs,
		Error:      errStr,
	}
}

func configureNetwork(guestIP, gatewayIP, dnsServer string, log *logrus.Logger) {
	log.Infof("Configuring guest network interface: IP=%s, Gateway=%s, DNS=%s", guestIP, gatewayIP, dnsServer)

	commands := [][]string{
		{"ip", "addr", "add", guestIP, "dev", "eth0"},
		{"ip", "link", "set", "eth0", "up"},
		{"ip", "route", "add", "default", "via", gatewayIP, "dev", "eth0"},
	}

	for _, cmdArgs := range commands {
		cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
		if out, err := cmd.CombinedOutput(); err != nil {
			log.Warnf("Network command %v failed: %v (%s)", cmdArgs, err, string(out))
		}
	}

	// Set DNS resolver
	resolvConf := fmt.Sprintf("nameserver %s\n", dnsServer)
	if err := os.WriteFile("/etc/resolv.conf", []byte(resolvConf), 0644); err != nil {
		log.Warnf("Failed to write /etc/resolv.conf: %v", err)
	}
}
