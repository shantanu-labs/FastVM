package sandbox

import (
	"time"

	"github.com/shantanuroy/airbox/pkg/network"
)

// SandboxOpts contains the configuration required to launch a sandboxed microVM.
type SandboxOpts struct {
	SandboxID  string
	KernelPath string
	RootfsPath string
	TapDevice  *network.TapDevice
	VcpuCount  int64
	MemSizeMib int64
	Timeout    time.Duration
	Language   string
	Code       string
	Stdin      string
	Env        map[string]string
}

// DefaultSandboxOpts returns standard sizing (1 vCPU, 256MB RAM, 10s default timeout).
func DefaultSandboxOpts(id string) SandboxOpts {
	return SandboxOpts{
		SandboxID:  id,
		KernelPath: "assets/vmlinux",
		RootfsPath: "assets/rootfs.ext4",
		VcpuCount:  1,
		MemSizeMib: 256,
		Timeout:    10 * time.Second,
		Env:        make(map[string]string),
	}
}
