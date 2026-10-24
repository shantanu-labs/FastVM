//go:build !linux
// +build !linux

package sandbox

import "github.com/sirupsen/logrus"

// NewHypervisor creates the platform-specific hypervisor with a pre-warmed worker pool.
func NewHypervisor(logger *logrus.Logger) Hypervisor {
	return NewMockHypervisor(logger)
}
