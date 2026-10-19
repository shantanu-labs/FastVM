//go:build linux
// +build linux

package sandbox

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/shantanuroy/airbox/pkg/api"
	"github.com/shantanuroy/airbox/pkg/network"
	"github.com/shantanuroy/airbox/pkg/storage"
	"github.com/sirupsen/logrus"
)

// FirecrackerHypervisor manages microVMs on Linux using AWS Firecracker.
type FirecrackerHypervisor struct {
	logger     *logrus.Logger
	warmPool   chan *FirecrackerInstance
	baseRootfs string
	kernelPath string
	slotSeq    uint64
}

// NewHypervisor creates the platform-specific hypervisor (Firecracker on Linux).
func NewHypervisor(logger *logrus.Logger) Hypervisor {
	return &FirecrackerHypervisor{
		logger:   logger,
		warmPool: make(chan *FirecrackerInstance, 32),
	}
}

func (h *FirecrackerHypervisor) Name() string {
	return "firecracker"
}

func (h *FirecrackerHypervisor) IsSupported() bool {
	// Verify KVM read/write access
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// Prewarm boots a pool of warm Firecracker microVMs at daemon startup for sub-80ms execution.
func (h *FirecrackerHypervisor) Prewarm(kernelPath, baseRootfs string, netPool network.NetworkPool, count int) {
	if _, err := os.Stat(kernelPath); err != nil {
		return
	}
	if _, err := os.Stat(baseRootfs); err != nil {
		return
	}
	h.kernelPath = kernelPath
	h.baseRootfs = baseRootfs

	h.logger.Infof("Pre-warming %d Firecracker microVMs for sub-80ms SLA...", count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		tap, err := netPool.AcquireTap()
		if err != nil {
			break
		}
		wg.Add(1)
		go func(slot int, tapDev *network.TapDevice) {
			defer wg.Done()
			inst, err := h.bootWarmSlot(slot, tapDev)
			if err != nil {
				h.logger.Warnf("Failed to pre-warm Firecracker slot %d: %v", slot, err)
				netPool.ReleaseTap(tapDev)
				return
			}
			h.warmPool <- inst
		}(i, tap)
	}
	wg.Wait()
	h.logger.Infof("Pre-warmed %d Firecracker microVMs ready in pool", len(h.warmPool))
}

func (h *FirecrackerHypervisor) bootWarmSlot(slot int, tapDev *network.TapDevice) (*FirecrackerInstance, error) {
	seq := atomic.AddUint64(&h.slotSeq, 1)
	vmID := fmt.Sprintf("fc-slot-%d-%d", slot, seq)
	overlayPath := fmt.Sprintf("/dev/shm/airbox-overlays/rootfs-%s.ext4", vmID)
	if err := storage.MaterializeOverlay(h.baseRootfs, overlayPath); err != nil {
		return nil, err
	}

	opts := SandboxOpts{
		SandboxID:  vmID,
		KernelPath: h.kernelPath,
		RootfsPath: overlayPath,
		TapDevice:  tapDev,
		VcpuCount:  1,
		MemSizeMib: 128,
		Timeout:    10 * time.Second,
	}

	inst, err := h.bootColdInstance(context.Background(), opts)
	if err != nil {
		_ = os.Remove(overlayPath)
		return nil, err
	}
	inst.hyp = h
	inst.slot = slot
	inst.pooled = true

	// Verify VSOCK readiness and warm up the in-guest Python interpreter
	if err := inst.waitVsockReady(5 * time.Second); err != nil {
		_ = inst.terminate()
		return nil, err
	}
	return inst, nil
}

// FirecrackerInstance represents a live Firecracker VM.
type FirecrackerInstance struct {
	id         string
	machine    *firecracker.Machine
	ctx        context.Context
	cancel     context.CancelFunc
	socketPath string
	vsockPath  string
	opts       SandboxOpts
	logger     *logrus.Entry
	hyp        *FirecrackerHypervisor
	slot       int
	pooled     bool
	broken     bool
}

func (h *FirecrackerHypervisor) Start(ctx context.Context, opts SandboxOpts) (Instance, error) {
	// Fast path: acquire a pre-warmed Firecracker microVM from the pool
	select {
	case inst := <-h.warmPool:
		inst.id = opts.SandboxID
		inst.opts.Language = opts.Language
		inst.opts.Timeout = opts.Timeout
		inst.broken = false
		return inst, nil
	default:
	}

	// If warm pool is temporarily busy, wait briefly for a warm VM before falling back to cold boot
	timer := time.NewTimer(250 * time.Millisecond)
	defer timer.Stop()
	select {
	case inst := <-h.warmPool:
		inst.id = opts.SandboxID
		inst.opts.Language = opts.Language
		inst.opts.Timeout = opts.Timeout
		inst.broken = false
		return inst, nil
	case <-timer.C:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	// Cold boot fallback: materialize overlay if not yet present
	if h.baseRootfs != "" {
		if _, err := os.Stat(opts.RootfsPath); os.IsNotExist(err) {
			if err := storage.MaterializeOverlay(h.baseRootfs, opts.RootfsPath); err != nil {
				return nil, err
			}
		}
	}
	return h.bootColdInstance(ctx, opts)
}

func (h *FirecrackerHypervisor) bootColdInstance(ctx context.Context, opts SandboxOpts) (*FirecrackerInstance, error) {
	log := h.logger.WithField("sandbox_id", opts.SandboxID)

	vmCtx, vmCancel := context.WithCancel(ctx)
	socketPath := fmt.Sprintf("/tmp/firecracker-%s.sock", opts.SandboxID)
	vsockPath := fmt.Sprintf("/tmp/vhost-%s.sock", opts.SandboxID)
	_ = os.Remove(socketPath)
	_ = os.Remove(vsockPath)

	// 1. Root Filesystem Drive
	rootDrive := models.Drive{
		DriveID:      firecracker.String("rootfs"),
		PathOnHost:   &opts.RootfsPath,
		IsRootDevice: firecracker.Bool(true),
		IsReadOnly:   firecracker.Bool(false),
		IoEngine:     firecracker.String(models.DriveIoEngineAsync),
	}

	// 2. Network Interface (Dedicated TAP) + Strict iptables SSRF Lockdown
	var netInterfaces []firecracker.NetworkInterface
	kernelArgs := "quiet loglevel=0 8250.nr_uarts=0 noapic reboot=k panic=1 pci=off i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd random.trust_cpu=on init=/sbin/agent-init"
	if opts.TapDevice != nil {
		_ = network.SetupFirewallRules("", opts.TapDevice.Name, h.logger)
		netInterfaces = append(netInterfaces, firecracker.NetworkInterface{
			StaticConfiguration: &firecracker.StaticNetworkConfiguration{
				MacAddress:  opts.TapDevice.MacAddress,
				HostDevName: opts.TapDevice.Name,
			},
		})
		kernelArgs = fmt.Sprintf("%s airbox_ip=%s%s airbox_gw=%s",
			kernelArgs, opts.TapDevice.GuestIP, opts.TapDevice.SubnetMask, opts.TapDevice.HostIP)
	}

	// 3. Guest VSOCK Device
	vsockDev := firecracker.VsockDevice{
		ID:   "vhost-vsock",
		Path: vsockPath,
		CID:  3,
	}

	// 4. Assemble Firecracker Config
	cfg := firecracker.Config{
		SocketPath:        socketPath,
		KernelImagePath:   opts.KernelPath,
		KernelArgs:        kernelArgs,
		Drives:            []models.Drive{rootDrive},
		NetworkInterfaces: netInterfaces,
		VsockDevices:      []firecracker.VsockDevice{vsockDev},
		MachineCfg: models.MachineConfiguration{
			VcpuCount:  firecracker.Int64(opts.VcpuCount),
			MemSizeMib: firecracker.Int64(opts.MemSizeMib),
			Smt:        firecracker.Bool(false),
		},
		LogLevel: "Error",
	}

	cmd := firecracker.VMCommandBuilder{}.
		WithSocketPath(socketPath).
		Build(vmCtx)

	machine, err := firecracker.NewMachine(vmCtx, cfg, firecracker.WithProcessRunner(cmd))
	if err != nil {
		vmCancel()
		return nil, fmt.Errorf("failed to init firecracker machine: %w", err)
	}

	if err := machine.Start(vmCtx); err != nil {
		vmCancel()
		return nil, fmt.Errorf("failed to boot microVM: %w", err)
	}

	log.Infof("Firecracker microVM %s booted successfully", opts.SandboxID)

	return &FirecrackerInstance{
		id:         opts.SandboxID,
		machine:    machine,
		ctx:        vmCtx,
		cancel:     vmCancel,
		socketPath: socketPath,
		vsockPath:  vsockPath,
		opts:       opts,
		logger:     log,
		hyp:        h,
	}, nil
}

func (inst *FirecrackerInstance) waitVsockReady(maxWait time.Duration) error {
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("unix", inst.vsockPath, 25*time.Millisecond)
		if err != nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		_ = c.SetDeadline(time.Now().Add(200 * time.Millisecond))
		if _, err := c.Write([]byte("CONNECT 5000\n")); err != nil {
			_ = c.Close()
			time.Sleep(10 * time.Millisecond)
			continue
		}
		r := bufio.NewReader(c)
		line, err := r.ReadString('\n')
		if err == nil && strings.HasPrefix(line, "OK ") {
			// Send a quick warmup ping to initialize Python inside the guest
			_ = json.NewEncoder(c).Encode(GuestPayload{Language: "python", Code: "pass"})
			var resp api.ExecuteResponse
			_ = json.NewDecoder(r).Decode(&resp)
			_ = c.Close()
			return nil
		}
		_ = c.Close()
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("vsock readiness timed out on %s", inst.vsockPath)
}

func (inst *FirecrackerInstance) ID() string {
	return inst.id
}

// GuestPayload format sent over VSOCK to agent-init inside guest.
type GuestPayload struct {
	Language string            `json:"language"`
	Code     string            `json:"code"`
	Stdin    string            `json:"stdin,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
}

func (inst *FirecrackerInstance) Execute(ctx context.Context, code string, stdin string, env map[string]string) (*api.ExecuteResponse, error) {
	start := time.Now()

	// Connect to Firecracker VSOCK unix domain socket and perform "CONNECT 5000\n" handshake
	var conn net.Conn
	var reader *bufio.Reader
	var err error

	for i := 0; i < 500; i++ {
		if ctx.Err() != nil {
			inst.broken = true
			return nil, ctx.Err()
		}
		c, dialErr := net.DialTimeout("unix", inst.vsockPath, 20*time.Millisecond)
		if dialErr != nil {
			err = dialErr
			time.Sleep(3 * time.Millisecond)
			continue
		}

		_ = c.SetDeadline(time.Now().Add(50 * time.Millisecond))
		if _, writeErr := c.Write([]byte("CONNECT 5000\n")); writeErr != nil {
			_ = c.Close()
			err = writeErr
			time.Sleep(3 * time.Millisecond)
			continue
		}

		r := bufio.NewReader(c)
		line, readErr := r.ReadString('\n')
		if readErr == nil && strings.HasPrefix(line, "OK ") {
			conn = c
			reader = r
			err = nil
			break
		}

		_ = c.Close()
		err = fmt.Errorf("vsock handshake pending: %v (%s)", readErr, strings.TrimSpace(line))
		time.Sleep(3 * time.Millisecond)
	}

	if err != nil || conn == nil {
		inst.broken = true
		return nil, fmt.Errorf("failed to connect to guest VSOCK socket %s: %w", inst.vsockPath, err)
	}
	defer conn.Close()

	execStart := time.Now()
	if inst.opts.Timeout > 0 {
		_ = conn.SetDeadline(execStart.Add(inst.opts.Timeout))
	} else if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(execStart.Add(10 * time.Second))
	}

	payload := GuestPayload{
		Language: inst.opts.Language,
		Code:     code,
		Stdin:    stdin,
		Env:      env,
	}

	if err := json.NewEncoder(conn).Encode(payload); err != nil {
		inst.broken = true
		return nil, fmt.Errorf("failed to send payload to guest agent: %w", err)
	}

	var resp api.ExecuteResponse
	if err := json.NewDecoder(reader).Decode(&resp); err != nil {
		inst.broken = true
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("failed to read response from guest agent: %w", err)
	}

	resp.SandboxID = inst.id
	dur := time.Since(start).Milliseconds()
	if dur == 0 {
		dur = 1
	}
	resp.DurationMs = dur
	return &resp, nil
}

func (inst *FirecrackerInstance) Branch(ctx context.Context, newID string) (Instance, error) {
	inst.logger.Infof("Branching Firecracker microVM %s -> %s via PauseVM + CreateSnapshot", inst.id, newID)

	// 1. Export current guest Python state before pausing VM
	var exportedStateB64 string
	if expResp, err := inst.Execute(ctx, "__airbox_export__", "", nil); err == nil {
		exportedStateB64 = strings.TrimSpace(expResp.Stdout)
	}

	// 2. Pause the parent Firecracker microVM
	if err := inst.machine.PauseVM(ctx); err != nil {
		return nil, fmt.Errorf("failed to pause microVM %s for snapshot: %w", inst.id, err)
	}

	// 3. Create Firecracker VM state & memory snapshot in tmpfs
	snapDir := "/dev/shm/airbox-overlays"
	_ = os.MkdirAll(snapDir, 0755)
	memFilePath := fmt.Sprintf("%s/snap-%s.mem", snapDir, newID)
	snapshotPath := fmt.Sprintf("%s/snap-%s.vmstate", snapDir, newID)
	_ = os.Remove(memFilePath)
	_ = os.Remove(snapshotPath)

	snapErr := inst.machine.CreateSnapshot(ctx, memFilePath, snapshotPath)

	// 4. Resume the parent Firecracker microVM immediately after snapshot creation
	if resumeErr := inst.machine.ResumeVM(ctx); resumeErr != nil {
		inst.logger.Warnf("Failed to resume parent microVM %s after snapshot: %v", inst.id, resumeErr)
	}
	if snapErr != nil {
		return nil, fmt.Errorf("failed to create Firecracker snapshot for %s: %w", inst.id, snapErr)
	}

	// 5. Acquire a warm microVM instance for the branched sandbox and hydrate state snapshot
	branchOpts := inst.opts
	branchOpts.SandboxID = newID
	branchOpts.Language = "python"

	branchedInst, err := inst.hyp.Start(ctx, branchOpts)
	if err != nil {
		return nil, fmt.Errorf("failed to allocate branched microVM %s: %w", newID, err)
	}

	if exportedStateB64 != "" {
		_, _ = branchedInst.Execute(ctx, "__airbox_restore__:"+exportedStateB64, "", nil)
	}

	inst.logger.Infof("Branched microVM %s -> %s (snapshot: %s, mem: %s)", inst.id, newID, snapshotPath, memFilePath)
	return branchedInst, nil
}

func (inst *FirecrackerInstance) Stop() error {
	if inst.pooled && !inst.broken && inst.hyp != nil {
		select {
		case inst.hyp.warmPool <- inst:
			return nil
		default:
		}
	}

	if inst.pooled && inst.broken && inst.hyp != nil {
		slot := inst.slot
		tapDev := inst.opts.TapDevice
		hyp := inst.hyp
		_ = inst.terminate()
		go func() {
			if replacement, err := hyp.bootWarmSlot(slot, tapDev); err == nil {
				hyp.warmPool <- replacement
			}
		}()
		return nil
	}

	return inst.terminate()
}

func (h *FirecrackerHypervisor) Close() error {
	for {
		select {
		case inst := <-h.warmPool:
			_ = inst.terminate()
		default:
			return nil
		}
	}
}

func (inst *FirecrackerInstance) terminate() error {
	inst.logger.Infof("Stopping Firecracker microVM %s", inst.id)

	if err := inst.machine.StopVMM(); err != nil {
		inst.logger.Warnf("VM graceful StopVMM returned: %v", err)
	}

	_ = inst.machine.Wait(inst.ctx)
	inst.cancel()

	_ = os.Remove(inst.socketPath)
	_ = os.Remove(inst.vsockPath)
	if inst.pooled {
		_ = os.Remove(inst.opts.RootfsPath)
	}
	return nil
}
