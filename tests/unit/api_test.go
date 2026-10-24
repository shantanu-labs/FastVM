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
	"github.com/shantanuroy/airbox/pkg/sandbox"
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

func (m *mockManager) StartJob(req api.ExecuteRequest) (*api.JobInfo, error) {
	return &api.JobInfo{
		JobID:     "job-mock-1",
		SandboxID: "sbx-mock-1",
		Language:  req.Language,
		Status:    "running",
		CreatedAt: time.Now(),
	}, nil
}

func (m *mockManager) GetJob(jobID string) (*api.JobInfo, error) {
	return &api.JobInfo{
		JobID:      jobID,
		SandboxID:  "sbx-mock-1",
		Language:   "python",
		Status:     "completed",
		Stdout:     "async 42\n",
		ExitCode:   0,
		DurationMs: 10,
		CreatedAt:  time.Now(),
	}, nil
}

func (m *mockManager) ListJobs() []*api.JobInfo {
	job, _ := m.GetJob("job-mock-1")
	return []*api.JobInfo{job}
}

func (m *mockManager) CancelJob(jobID string) (*api.JobInfo, error) {
	return &api.JobInfo{
		JobID:     jobID,
		SandboxID: "sbx-mock-1",
		Status:    "cancelled",
		CreatedAt: time.Now(),
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

func TestAsyncExecuteAndJobsLifecycle(t *testing.T) {
	log := logrus.New()
	log.SetLevel(logrus.PanicLevel)

	mgr, err := sandbox.NewManager(sandbox.Config{
		PoolSize:   10,
		OverlayDir: t.TempDir(),
	}, log)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}
	defer mgr.Close()

	server := api.NewServer(mgr, log)
	router := server.Routes()

	// 1. Launch async job via POST /v1/execute with "async": true
	payload := api.ExecuteRequest{
		Language: "python",
		Code:     "print('background agent job complete')",
		Async:    true,
	}
	data, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPost, "/v1/execute", bytes.NewBuffer(data))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("expected status 202 Accepted for async execute, got %d: %s", rr.Code, rr.Body.String())
	}

	var asyncResp api.ExecuteResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &asyncResp); err != nil {
		t.Fatalf("failed to decode async execute response: %v", err)
	}
	if asyncResp.JobID == "" {
		t.Fatalf("expected non-empty job_id in async response")
	}

	// 2. Poll GET /v1/jobs/{id} until completed
	var jobInfo api.JobInfo
	for i := 0; i < 150; i++ {
		getReq, _ := http.NewRequest(http.MethodGet, "/v1/jobs/"+asyncResp.JobID, nil)
		getRR := httptest.NewRecorder()
		router.ServeHTTP(getRR, getReq)
		if getRR.Code != http.StatusOK {
			t.Fatalf("expected status 200 on GET /v1/jobs/%s, got %d", asyncResp.JobID, getRR.Code)
		}
		_ = json.Unmarshal(getRR.Body.Bytes(), &jobInfo)
		if jobInfo.Status == "completed" || jobInfo.Status == "failed" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	if jobInfo.Status != "completed" {
		t.Fatalf("expected job status 'completed', got %q (error: %s)", jobInfo.Status, jobInfo.Error)
	}
	if jobInfo.Stdout != "background agent job complete\n" {
		t.Errorf("expected stdout 'background agent job complete\\n', got %q", jobInfo.Stdout)
	}

	// 3. Test job cancellation via POST /v1/jobs + DELETE /v1/jobs/{id}
	longPayload := api.ExecuteRequest{
		Language: "python",
		Code:     "import time; time.sleep(10)",
	}
	longData, _ := json.Marshal(longPayload)
	startReq, _ := http.NewRequest(http.MethodPost, "/v1/jobs", bytes.NewBuffer(longData))
	startRR := httptest.NewRecorder()
	router.ServeHTTP(startRR, startReq)
	if startRR.Code != http.StatusAccepted {
		t.Fatalf("expected 202 Accepted on POST /v1/jobs, got %d", startRR.Code)
	}
	var startedJob api.JobInfo
	_ = json.Unmarshal(startRR.Body.Bytes(), &startedJob)

	delReq, _ := http.NewRequest(http.MethodDelete, "/v1/jobs/"+startedJob.JobID, nil)
	delRR := httptest.NewRecorder()
	router.ServeHTTP(delRR, delReq)
	if delRR.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on DELETE /v1/jobs/%s, got %d", startedJob.JobID, delRR.Code)
	}
	var cancelledJob api.JobInfo
	_ = json.Unmarshal(delRR.Body.Bytes(), &cancelledJob)
	if cancelledJob.Status != "cancelled" {
		t.Errorf("expected cancelled status, got %q", cancelledJob.Status)
	}
}

