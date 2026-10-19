package network

import (
	"fmt"
	"sync"
)

// TapDevice represents an allocated TAP interface with dedicated IPAM.
type TapDevice struct {
	Name       string // e.g. "tap0"
	HostIP     string // e.g. "172.16.0.1"
	GuestIP    string // e.g. "172.16.0.2"
	SubnetMask string // e.g. "/30"
	MacAddress string // e.g. "AA:FC:00:00:00:01"
	Index      int
}

// NetworkPool manages a pre-warmed pool of TAP devices to bypass CLI execution overhead.
type NetworkPool interface {
	AcquireTap() (*TapDevice, error)
	ReleaseTap(tap *TapDevice)
	Available() int
	Close() error
}

// BasePool provides common thread-safe pooling queue primitives.
type BasePool struct {
	mu        sync.Mutex
	available []*TapDevice
	inUse     map[string]*TapDevice
}

// NewBasePool creates a BasePool.
func NewBasePool() *BasePool {
	return &BasePool{
		available: make([]*TapDevice, 0),
		inUse:     make(map[string]*TapDevice),
	}
}

func (p *BasePool) Add(tap *TapDevice) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.available = append(p.available, tap)
}

func (p *BasePool) Acquire() (*TapDevice, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.available) == 0 {
		return nil, fmt.Errorf("no tap devices available in pre-warmed pool")
	}

	tap := p.available[0]
	p.available = p.available[1:]
	p.inUse[tap.Name] = tap
	return tap, nil
}

func (p *BasePool) Release(tap *TapDevice) {
	p.mu.Lock()
	defer p.mu.Unlock()

	delete(p.inUse, tap.Name)
	p.available = append(p.available, tap)
}

func (p *BasePool) AvailableCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.available)
}
