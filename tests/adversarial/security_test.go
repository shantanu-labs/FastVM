package adversarial

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/shantanuroy/airbox/pkg/api"
	"github.com/shantanuroy/airbox/pkg/sandbox"
	"github.com/sirupsen/logrus"
)

func setupTestManager(t *testing.T) *sandbox.SandboxManager {
	log := logrus.New()
	log.SetLevel(logrus.WarnLevel)

	kernelPath := "assets/vmlinux"
	rootfsPath := "assets/rootfs.ext4"
	if _, err := os.Stat(kernelPath); os.IsNotExist(err) {
		if _, err2 := os.Stat("../../assets/vmlinux"); err2 == nil {
			kernelPath = "../../assets/vmlinux"
			rootfsPath = "../../assets/rootfs.ext4"
		}
	}

	cfg := sandbox.Config{
		KernelPath: kernelPath,
		RootfsPath: rootfsPath,
		PoolSize:   4,
	}

	mgr, err := sandbox.NewManager(cfg, log)
	if err != nil {
		t.Fatalf("failed to create test manager: %v", err)
	}
	return mgr
}

// TestAdversarialTimeout verifies that rogue sleep commands are terminated promptly.
func TestAdversarialTimeout(t *testing.T) {
	mgr := setupTestManager(t)
	defer mgr.Close()

	ctx := context.Background()
	req := api.ExecuteRequest{
		Language:  "python",
		Code:      "import time\ntime.sleep(60)\n",
		TimeoutMs: 500, // 500ms max timeout limit
	}

	start := time.Now()
	resp, err := mgr.Execute(ctx, req)
	duration := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected execution error: %v", err)
	}

	if !resp.IsTimeout {
		t.Errorf("expected execution to be marked as timeout, got: %+v", resp)
	}

	if duration > 3*time.Second {
		t.Errorf("execution was not terminated promptly, took %v", duration)
	}
}

// TestAdversarialExitCode verifies that non-zero exit codes are captured cleanly.
func TestAdversarialExitCode(t *testing.T) {
	mgr := setupTestManager(t)
	defer mgr.Close()

	ctx := context.Background()
	req := api.ExecuteRequest{
		Language:  "python",
		Code:      "import sys\nsys.exit(42)\n",
		TimeoutMs: 2000,
	}

	resp, err := mgr.Execute(ctx, req)
	if err != nil {
		t.Fatalf("execution error: %v", err)
	}

	if resp.ExitCode != 42 {
		t.Errorf("expected exit code 42, got %d", resp.ExitCode)
	}
}

// TestAdversarialStderrCapture verifies that error exceptions are returned to caller.
func TestAdversarialStderrCapture(t *testing.T) {
	mgr := setupTestManager(t)
	defer mgr.Close()

	ctx := context.Background()
	req := api.ExecuteRequest{
		Language:  "python",
		Code:      "raise ValueError('malicious payload detected')",
		TimeoutMs: 2000,
	}

	resp, err := mgr.Execute(ctx, req)
	if err != nil {
		t.Fatalf("execution error: %v", err)
	}

	if resp.ExitCode == 0 {
		t.Errorf("expected non-zero exit code on python exception")
	}

	if len(resp.Stderr) == 0 {
		t.Errorf("expected exception in stderr, got empty")
	}
}
