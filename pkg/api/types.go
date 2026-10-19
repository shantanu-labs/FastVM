package api

import "time"

// ExecuteRequest represents the execution payload sent by an AI agent or client.
type ExecuteRequest struct {
	Language  string            `json:"language"`             // e.g., "python", "bash", "node"
	Code      string            `json:"code"`                 // raw code string or script
	TimeoutMs int               `json:"timeout_ms,omitempty"` // max execution timeout in milliseconds
	Stdin     string            `json:"stdin,omitempty"`      // optional stdin input
	Env       map[string]string `json:"env,omitempty"`        // environment variables to pass
	SandboxID string            `json:"sandbox_id,omitempty"` // optional existing sandbox ID or branch source
	Async     bool              `json:"async,omitempty"`      // if true, runs in background and returns job_id immediately
}

// ExecuteResponse represents the result of code execution sent back to the agent.
type ExecuteResponse struct {
	JobID      string `json:"job_id,omitempty"`
	SandboxID  string `json:"sandbox_id"`
	Status     string `json:"status,omitempty"` // "running", "completed", "failed", "timeout", "cancelled"
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   int    `json:"exit_code"`
	DurationMs int64  `json:"duration_ms"`
	IsTimeout  bool   `json:"is_timeout"`
	Error      string `json:"error,omitempty"`
}

// JobInfo tracks an asynchronous background execution job inside a microVM sandbox.
type JobInfo struct {
	JobID       string     `json:"job_id"`
	SandboxID   string     `json:"sandbox_id"`
	Language    string     `json:"language"`
	Status      string     `json:"status"` // "running", "completed", "failed", "timeout", "cancelled"
	Stdout      string     `json:"stdout"`
	Stderr      string     `json:"stderr"`
	ExitCode    int        `json:"exit_code"`
	DurationMs  int64      `json:"duration_ms"`
	IsTimeout   bool       `json:"is_timeout"`
	Error       string     `json:"error,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

// BranchRequest represents a request to snapshot and branch an existing sandbox microVM.
type BranchRequest struct {
	NewSandboxID string `json:"new_sandbox_id,omitempty"`
}

// BranchResponse returns metadata for a newly branched copy-on-write microVM sandbox.
type BranchResponse struct {
	ParentSandboxID string `json:"parent_sandbox_id"`
	SandboxID       string `json:"sandbox_id"`
	Driver          string `json:"driver"`
	SnapshotPath    string `json:"snapshot_path,omitempty"`
	MemoryPath      string `json:"memory_path,omitempty"`
	DurationMs      int64  `json:"duration_ms"`
}

// SandboxInfo describes an active or pooled sandbox microVM.
type SandboxInfo struct {
	ID        string    `json:"id"`
	Driver    string    `json:"driver"` // e.g. "firecracker", "mock"
	Status    string    `json:"status"` // "running", "paused", "stopped"
	TapDevice string    `json:"tap_device,omitempty"`
	IPAddress string    `json:"ip_address,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// HealthResponse provides the health status and capacity metrics of the node.
type HealthResponse struct {
	Status          string `json:"status"`
	Version         string `json:"version"`
	Hypervisor      string `json:"hypervisor"`
	KvmAvailable    bool   `json:"kvm_available"`
	ActiveSandboxes int    `json:"active_sandboxes"`
	AvailableTaps   int    `json:"available_taps"`
}
