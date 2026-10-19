//go:build !linux
// +build !linux

package network

import (
	"fmt"

	"github.com/sirupsen/logrus"
)

// MockNetworkPool implements NetworkPool for macOS and non-Linux systems for local dev.
type MockNetworkPool struct {
	*BasePool
	log *logrus.Logger
}

// NewNetlinkPool creates a mock network pool on non-Linux environments.
func NewNetlinkPool(size int, logger *logrus.Logger) (NetworkPool, error) {
	pool := &MockNetworkPool{
		BasePool: NewBasePool(),
		log:      logger,
	}

	for i := 0; i < size; i++ {
		tapDev := &TapDevice{
			Name:       fmt.Sprintf("tap%d", i),
			HostIP:     fmt.Sprintf("172.16.0.%d", (i*4)+1),
			GuestIP:    fmt.Sprintf("172.16.0.%d", (i*4)+2),
			SubnetMask: "/30",
			MacAddress: fmt.Sprintf("AA:FC:00:00:00:%02X", i%256),
			Index:      i,
		}
		pool.Add(tapDev)
	}

	logger.Infof("[Mock] Initialized local simulated TAP pool with %d devices", size)
	return pool, nil
}

func (p *MockNetworkPool) AcquireTap() (*TapDevice, error) {
	return p.Acquire()
}

func (p *MockNetworkPool) ReleaseTap(tap *TapDevice) {
	p.Release(tap)
}

func (p *MockNetworkPool) Available() int {
	return p.AvailableCount()
}

func (p *MockNetworkPool) Close() error {
	return nil
}
