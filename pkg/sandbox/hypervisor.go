package sandbox

import (
	"context"

	"github.com/shantanuroy/airbox/pkg/api"
)

// Instance represents a running microVM sandbox instance.
type Instance interface {
	ID() string
	Execute(ctx context.Context, code string, stdin string, env map[string]string) (*api.ExecuteResponse, error)
	Branch(ctx context.Context, newID string) (Instance, error)
	Stop() error
}

// Hypervisor abstracts microVM life-cycle operations across different engines (Firecracker, Apple Virtualization, Mock).
type Hypervisor interface {
	Name() string
	IsSupported() bool
	Start(ctx context.Context, opts SandboxOpts) (Instance, error)
}
