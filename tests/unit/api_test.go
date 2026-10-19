package unit

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shantanuroy/airbox/pkg/api"
	"github.com/sirupsen/logrus"
)

type mockManager struct {
	executeFunc func(ctx context.Context, req api.ExecuteRequest) (*api.ExecuteResponse, error)
}

func (m *mockManager) Execute(ctx context.Context, req api.ExecuteRequest) (*api.ExecuteResponse, error) {
	if m.executeFunc != nil {
		return m.executeFunc(ctx, req)
	}
	return &api.ExecuteResponse{
		SandboxID:  "test-sbx",
		Stdout:     "mock output\n",
		Stderr:     "",
		ExitCode:   0,
		DurationMs: 12,
	}, nil
}

func (m *mockManager) Branch(ctx context.Context, parentID string, newID string) (*api.BranchResponse, error) {
	if newID == "" {
		newID = "sbx-branch-mock"
	}
	return &api.BranchResponse{
		ParentSandboxID: parentID,
		SandboxID:       newID,
		Driver:          "mock-test",
		DurationMs:      4,
	}, nil
}

func (m *mockManager) HypervisorName() string {
	return "mock-test"
}

func (m *mockManager) KvmAvailable() bool {
	return true
}

func (m *mockManager) AvailableTaps() int {
	return 100
}

func (m *mockManager) ActiveCount() int {
	return 0
}

func (m *mockManager) ListSandboxes() []api.SandboxInfo {
	return []api.SandboxInfo{
		{
			ID:        "sbx-12345",
			Driver:    "mock-test",
			Status:    "running",
			TapDevice: "tap0",
			IPAddress: "172.16.0.2",
			CreatedAt: time.Now(),
		},
	}
}

func TestHealthzEndpoint(t *testing.T) {
	log := logrus.New()
	log.SetLevel(logrus.PanicLevel)

	mgr := &mockManager{}
	server := api.NewServer(mgr, log)
	router := server.Routes()

	req, _ := http.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()

	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}

	var health api.HealthResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &health); err != nil {
		t.Fatalf("failed to parse health response: %v", err)
	}

	if health.Status != "healthy" {
		t.Errorf("expected status healthy, got %s", health.Status)
	}

	if health.Hypervisor != "mock-test" {
		t.Errorf("expected hypervisor mock-test, got %s", health.Hypervisor)
	}
}

func TestExecuteEndpoint(t *testing.T) {
	log := logrus.New()
	log.SetLevel(logrus.PanicLevel)

	mgr := &mockManager{
		executeFunc: func(ctx context.Context, req api.ExecuteRequest) (*api.ExecuteResponse, error) {
			return &api.ExecuteResponse{
				SandboxID:  "sbx-run-1",
				Stdout:     "42\n",
				Stderr:     "",
				ExitCode:   0,
				DurationMs: 15,
			}, nil
		},
	}

	server := api.NewServer(mgr, log)
	router := server.Routes()

	payload := api.ExecuteRequest{
		Language:  "python",
		Code:      "print(42)",
		TimeoutMs: 5000,
	}

	data, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPost, "/v1/execute", bytes.NewBuffer(data))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp api.ExecuteResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode execute response: %v", err)
	}

	if resp.Stdout != "42\n" {
		t.Errorf("expected stdout '42\n', got %q", resp.Stdout)
	}
	if resp.ExitCode != 0 {
		t.Errorf("expected exit code 0, got %d", resp.ExitCode)
	}
}

func TestExecuteValidation(t *testing.T) {
	log := logrus.New()
	log.SetLevel(logrus.PanicLevel)

	mgr := &mockManager{}
	server := api.NewServer(mgr, log)
	router := server.Routes()

	// Empty Code
	payload := api.ExecuteRequest{
		Language: "python",
		Code:     "",
	}
	data, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPost, "/v1/execute", bytes.NewBuffer(data))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400 on empty code, got %d", rr.Code)
	}
}

func TestListSandboxesEndpoint(t *testing.T) {
	log := logrus.New()
	log.SetLevel(logrus.PanicLevel)

	mgr := &mockManager{}
	server := api.NewServer(mgr, log)
	router := server.Routes()

	req, _ := http.NewRequest(http.MethodGet, "/v1/sandboxes", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}

	var list []api.SandboxInfo
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("failed to decode list: %v", err)
	}

	if len(list) != 1 {
		t.Fatalf("expected 1 sandbox, got %d", len(list))
	}
}

func TestBranchEndpoint(t *testing.T) {
	log := logrus.New()
	log.SetLevel(logrus.PanicLevel)

	mgr := &mockManager{}
	server := api.NewServer(mgr, log)
	router := server.Routes()

	req, _ := http.NewRequest(http.MethodPost, "/v1/sandboxes/sbx-parent-1/branch", bytes.NewBufferString(`{"new_sandbox_id":"sbx-child-1"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp api.BranchResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode branch response: %v", err)
	}

	if resp.ParentSandboxID != "sbx-parent-1" || resp.SandboxID != "sbx-child-1" {
		t.Errorf("unexpected branch response: %+v", resp)
	}
}

