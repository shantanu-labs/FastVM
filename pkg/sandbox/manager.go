package sandbox

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/shantanuroy/airbox/pkg/api"
	"github.com/shantanuroy/airbox/pkg/network"
	"github.com/shantanuroy/airbox/pkg/storage"
	"github.com/sirupsen/logrus"
)

// SandboxManager coordinates hypervisors, network pools, and overlay storage.
type SandboxManager struct {
	hypervisor Hypervisor
	netPool    network.NetworkPool
	overlayMgr *storage.OverlayManager
	logger     *logrus.Logger
	kernelPath string
	rootfsPath string

	mu              sync.RWMutex
	sandboxes       map[string]*activeEntry
	recentInstances map[string]Instance
	pinnedInstances map[string]Instance
	jobs            map[string]*api.JobInfo
	jobCancels      map[string]context.CancelFunc
}

type activeEntry struct {
	instance  Instance
	tap       *network.TapDevice
	createdAt time.Time
}

// Config holds SandboxManager setup arguments.
type Config struct {
	KernelPath   string
	RootfsPath   string
	OverlayDir   string
	PoolSize     int
	WarmPoolSize int
}

// NewManager initializes a new SandboxManager.
func NewManager(cfg Config, logger *logrus.Logger) (*SandboxManager, error) {
	hyp := NewHypervisor(logger)

	pool, err := network.NewNetlinkPool(cfg.PoolSize, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to init network pool: %w", err)
	}

	overlayMgr, err := storage.NewOverlayManager(cfg.RootfsPath, cfg.OverlayDir)
	if err != nil {
		return nil, fmt.Errorf("failed to init overlay manager: %w", err)
	}

	if prewarmer, ok := hyp.(interface {
		Prewarm(kernelPath, baseRootfs string, netPool network.NetworkPool, count int)
	}); ok {
		warmCount := cfg.WarmPoolSize
		if cfg.PoolSize > 0 && warmCount > cfg.PoolSize/2 {
			warmCount = cfg.PoolSize / 2
		}
		prewarmer.Prewarm(cfg.KernelPath, cfg.RootfsPath, pool, warmCount)
	}

	return &SandboxManager{
		hypervisor:      hyp,
		netPool:         pool,
		overlayMgr:      overlayMgr,
		logger:          logger,
		kernelPath:      cfg.KernelPath,
		rootfsPath:      cfg.RootfsPath,
		sandboxes:       make(map[string]*activeEntry),
		recentInstances: make(map[string]Instance),
		pinnedInstances: make(map[string]Instance),
		jobs:            make(map[string]*api.JobInfo),
		jobCancels:      make(map[string]context.CancelFunc),
	}, nil
}

// Execute orchestrates the sub-100ms execution flow.
func (m *SandboxManager) Execute(ctx context.Context, req api.ExecuteRequest) (*api.ExecuteResponse, error) {
	sandboxID := req.SandboxID
	isExplicitSandbox := sandboxID != ""
	if sandboxID == "" {
		sandboxID = "sbx-" + uuid.New().String()[:8]
	}

	timeout := 10 * time.Second
	if req.TimeoutMs > 0 {
		timeout = time.Duration(req.TimeoutMs) * time.Millisecond
	}

	// If targeting an explicitly branched or stateful sandbox, execute on its bound instance
	if isExplicitSandbox {
		m.mu.RLock()
		pinnedInst, hasPinned := m.pinnedInstances[sandboxID]
		if !hasPinned {
			pinnedInst, hasPinned = m.recentInstances[sandboxID]
		}
		m.mu.RUnlock()

		if hasPinned && pinnedInst != nil {
			execCtx, execCancel := context.WithTimeout(ctx, timeout)
			defer execCancel()
			resp, err := pinnedInst.Execute(execCtx, req.Code, req.Stdin, req.Env)
			if err != nil {
				if execCtx.Err() == context.DeadlineExceeded {
					return &api.ExecuteResponse{
						SandboxID: sandboxID,
						IsTimeout: true,
						Error:     "execution exceeded timeout threshold",
					}, nil
				}
				return nil, fmt.Errorf("execution failed: %w", err)
			}
			resp.SandboxID = sandboxID
			return resp, nil
		}
	}

	// 1. Acquire pre-warmed TAP device (zero-CLI)
	tap, err := m.netPool.AcquireTap()
	if err != nil {
		return nil, fmt.Errorf("network allocation failed: %w", err)
	}
	defer m.netPool.ReleaseTap(tap)

	// 2. Clone snapshot rootfs to temporary overlay
	overlayPath, err := m.overlayMgr.CreateOverlay(sandboxID)
	if err != nil {
		return nil, fmt.Errorf("storage overlay failed: %w", err)
	}
	defer func() {
		_ = m.overlayMgr.RemoveOverlay(sandboxID)
	}()

	// 3. Configure boot context
	bootCtx, bootCancel := context.WithCancel(ctx)
	defer bootCancel()

	// 4. Assemble MicroVM Options
	opts := SandboxOpts{
		SandboxID:  sandboxID,
		KernelPath: m.kernelPath,
		RootfsPath: overlayPath,
		TapDevice:  tap,
		VcpuCount:  1,
		MemSizeMib: 256,
		Timeout:    timeout,
		Language:   req.Language,
		Code:       req.Code,
		Stdin:      req.Stdin,
		Env:        req.Env,
	}

	// 5. Boot / Restore microVM
	inst, err := m.hypervisor.Start(bootCtx, opts)
	if err != nil {
		return nil, fmt.Errorf("hypervisor boot failed: %w", err)
	}

	m.mu.Lock()
	m.sandboxes[sandboxID] = &activeEntry{
		instance:  inst,
		tap:       tap,
		createdAt: time.Now(),
	}
	m.recentInstances[sandboxID] = inst
	if isExplicitSandbox {
		m.pinnedInstances[sandboxID] = inst
	}
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		delete(m.sandboxes, sandboxID)
		_, isPinned := m.pinnedInstances[sandboxID]
		m.mu.Unlock()
		if !isPinned {
			_ = inst.Stop()
		}
	}()

	// 6. Execute payload inside guest over VSOCK with execution timeout
	execCtx, execCancel := context.WithTimeout(ctx, timeout)
	defer execCancel()
	resp, err := inst.Execute(execCtx, req.Code, req.Stdin, req.Env)
	if err != nil {
		if execCtx.Err() == context.DeadlineExceeded {
			return &api.ExecuteResponse{
				SandboxID: sandboxID,
				IsTimeout: true,
				Error:     "execution exceeded timeout threshold",
			}, nil
		}
		return nil, fmt.Errorf("execution failed: %w", err)
	}

	return resp, nil
}

// Branch snapshots a parent sandbox microVM and creates an isolated copy-on-write branch.
func (m *SandboxManager) Branch(ctx context.Context, parentID string, newID string) (*api.BranchResponse, error) {
	start := time.Now()
	if newID == "" {
		newID = "sbx-branch-" + uuid.New().String()[:8]
	}

	m.mu.RLock()
	var parentInst Instance
	if entry, ok := m.sandboxes[parentID]; ok {
		parentInst = entry.instance
	} else if inst, ok := m.pinnedInstances[parentID]; ok {
		parentInst = inst
	} else if inst, ok := m.recentInstances[parentID]; ok {
		parentInst = inst
	}
	m.mu.RUnlock()

	if parentInst == nil {
		return nil, fmt.Errorf("sandbox %s not found", parentID)
	}

	branchedInst, err := parentInst.Branch(ctx, newID)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.pinnedInstances[parentID] = parentInst
	m.pinnedInstances[newID] = branchedInst
	m.recentInstances[newID] = branchedInst
	m.mu.Unlock()

	dur := time.Since(start).Milliseconds()
	if dur == 0 {
		dur = 1
	}

	snapPath := ""
	memPath := ""
	if m.hypervisor.Name() == "firecracker" {
		snapPath = fmt.Sprintf("/dev/shm/airbox-overlays/snap-%s.vmstate", newID)
		memPath = fmt.Sprintf("/dev/shm/airbox-overlays/snap-%s.mem", newID)
	}

	return &api.BranchResponse{
		ParentSandboxID: parentID,
		SandboxID:       newID,
		Driver:          m.hypervisor.Name(),
		SnapshotPath:    snapPath,
		MemoryPath:      memPath,
		DurationMs:      dur,
	}, nil
}

// StartJob launches an asynchronous background job inside a microVM and returns immediately (<1ms).
func (m *SandboxManager) StartJob(req api.ExecuteRequest) (*api.JobInfo, error) {
	jobID := "job-" + uuid.New().String()[:8]
	sandboxID := req.SandboxID
	if sandboxID == "" {
		sandboxID = "sbx-" + uuid.New().String()[:8]
	}

	if req.TimeoutMs <= 0 {
		req.TimeoutMs = 60000 // default 60s timeout for background agent jobs
	}
	req.SandboxID = sandboxID
	req.Async = false

	now := time.Now()
	job := &api.JobInfo{
		JobID:     jobID,
		SandboxID: sandboxID,
		Language:  req.Language,
		Status:    "running",
		CreatedAt: now,
	}

	jobCtx, cancel := context.WithCancel(context.Background())

	m.mu.Lock()
	m.jobs[jobID] = job
	m.jobCancels[jobID] = cancel
	m.mu.Unlock()

	go func() {
		defer cancel()
		start := time.Now()
		resp, err := m.Execute(jobCtx, req)
		completedAt := time.Now()
		dur := completedAt.Sub(start).Milliseconds()
		if dur == 0 {
			dur = 1
		}

		m.mu.Lock()
		defer m.mu.Unlock()
		delete(m.jobCancels, jobID)

		if job.Status == "cancelled" {
			job.CompletedAt = &completedAt
			job.DurationMs = dur
			return
		}

		job.CompletedAt = &completedAt
		job.DurationMs = dur
		if err != nil {
			if jobCtx.Err() == context.Canceled {
				job.Status = "cancelled"
				return
			}
			job.Status = "failed"
			job.Error = err.Error()
			return
		}

		job.Stdout = resp.Stdout
		job.Stderr = resp.Stderr
		job.ExitCode = resp.ExitCode
		job.IsTimeout = resp.IsTimeout
		job.Error = resp.Error
		if resp.DurationMs > 0 {
			job.DurationMs = resp.DurationMs
		}

		switch {
		case resp.IsTimeout:
			job.Status = "timeout"
		case resp.ExitCode != 0:
			job.Status = "failed"
		default:
			job.Status = "completed"
		}
	}()

	cp := *job
	return &cp, nil
}

// GetJob retrieves a snapshot of a background job's status and output.
func (m *SandboxManager) GetJob(jobID string) (*api.JobInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	job, ok := m.jobs[jobID]
	if !ok {
		return nil, fmt.Errorf("job %s not found", jobID)
	}
	cp := *job
	return &cp, nil
}

// ListJobs returns all tracked background jobs.
func (m *SandboxManager) ListJobs() []*api.JobInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]*api.JobInfo, 0, len(m.jobs))
	for _, j := range m.jobs {
		cp := *j
		out = append(out, &cp)
	}
	return out
}

// CancelJob terminates a running background job.
func (m *SandboxManager) CancelJob(jobID string) (*api.JobInfo, error) {
	m.mu.Lock()
	job, ok := m.jobs[jobID]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("job %s not found", jobID)
	}
	cancel, hasCancel := m.jobCancels[jobID]
	if job.Status == "running" {
		now := time.Now()
		job.Status = "cancelled"
		job.CompletedAt = &now
	}
	cp := *job
	m.mu.Unlock()

	if hasCancel && cancel != nil {
		cancel()
	}
	return &cp, nil
}

// HypervisorName returns the name of the active hypervisor driver.
func (m *SandboxManager) HypervisorName() string {
	return m.hypervisor.Name()
}

// KvmAvailable checks if KVM is ready.
func (m *SandboxManager) KvmAvailable() bool {
	return m.hypervisor.IsSupported()
}

// AvailableTaps returns the count of free TAP interfaces in the pool.
func (m *SandboxManager) AvailableTaps() int {
	return m.netPool.Available()
}

// ActiveCount returns current running sandboxes.
func (m *SandboxManager) ActiveCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sandboxes)
}

// ListSandboxes returns details on all active sandboxes.
func (m *SandboxManager) ListSandboxes() []api.SandboxInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	list := make([]api.SandboxInfo, 0, len(m.sandboxes))
	for id, entry := range m.sandboxes {
		tapName := ""
		ip := ""
		if entry.tap != nil {
			tapName = entry.tap.Name
			ip = entry.tap.GuestIP
		}
		list = append(list, api.SandboxInfo{
			ID:        id,
			Driver:    m.hypervisor.Name(),
			Status:    "running",
			TapDevice: tapName,
			IPAddress: ip,
			CreatedAt: entry.createdAt,
		})
	}
	return list
}

// Close gracefully releases all resources.
func (m *SandboxManager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for id, entry := range m.sandboxes {
		_ = entry.instance.Stop()
		if entry.tap != nil {
			m.netPool.ReleaseTap(entry.tap)
		}
		_ = m.overlayMgr.RemoveOverlay(id)
	}
	for _, inst := range m.pinnedInstances {
		_ = inst.Stop()
	}
	m.sandboxes = make(map[string]*activeEntry)
	m.pinnedInstances = make(map[string]Instance)
	m.recentInstances = make(map[string]Instance)

	if closer, ok := m.hypervisor.(interface{ Close() error }); ok {
		_ = closer.Close()
	}

	_ = m.netPool.Close()
	m.overlayMgr.CleanupAll()
	return nil
}
