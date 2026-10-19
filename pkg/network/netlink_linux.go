//go:build linux
// +build linux

package network

import (
	"fmt"

	"github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
)

// NetlinkPool implements NetworkPool using direct Linux Netlink sockets.
type NetlinkPool struct {
	*BasePool
	log *logrus.Logger
}

// NewNetlinkPool initializes a pool of pre-warmed TAP devices on Linux.
func NewNetlinkPool(size int, logger *logrus.Logger) (*NetlinkPool, error) {
	pool := &NetlinkPool{
		BasePool: NewBasePool(),
		log:      logger,
	}

	for i := 0; i < size; i++ {
		tapName := fmt.Sprintf("tap%d", i)
		macAddr := fmt.Sprintf("AA:FC:00:00:%02X:%02X", (i>>8)&0xFF, i&0xFF)

		// Each TAP gets a dedicated /30 block
		// e.g. i=0 -> 172.16.0.1 (host), 172.16.0.2 (guest)
		// block offset = i * 4
		fourthOctetHost := (i * 4) + 1
		thirdOctet := (fourthOctetHost / 256) % 256
		fourthOctetHost = fourthOctetHost % 256

		fourthOctetGuest := fourthOctetHost + 1

		hostIPStr := fmt.Sprintf("172.16.%d.%d/30", thirdOctet, fourthOctetHost)
		guestIPStr := fmt.Sprintf("172.16.%d.%d", thirdOctet, fourthOctetGuest)

		// Create or reuse tuntap link via netlink
		linkAttrs := netlink.NewLinkAttrs()
		linkAttrs.Name = tapName

		tuntap := &netlink.Tuntap{
			LinkAttrs: linkAttrs,
			Mode:      netlink.TUNTAP_MODE_TAP,
		}

		if err := netlink.LinkAdd(tuntap); err != nil {
			// If link already exists, try to reuse it
			existing, errGet := netlink.LinkByName(tapName)
			if errGet != nil {
				return nil, fmt.Errorf("failed to create TAP %s: %w", tapName, err)
			}
			tuntap.LinkAttrs = *existing.Attrs()
		}

		// Configure Host IP
		hostAddr, err := netlink.ParseAddr(hostIPStr)
		if err == nil {
			_ = netlink.AddrAdd(tuntap, hostAddr)
		}

		// Set interface up
		if err := netlink.LinkSetUp(tuntap); err != nil {
			logger.Warnf("failed to set tap %s UP: %v", tapName, err)
		}

		tapDev := &TapDevice{
			Name:       tapName,
			HostIP:     fmt.Sprintf("172.16.%d.%d", thirdOctet, fourthOctetHost),
			GuestIP:    guestIPStr,
			SubnetMask: "/30",
			MacAddress: macAddr,
			Index:      i,
		}

		pool.Add(tapDev)
	}

	logger.Infof("Initialized Netlink TAP pool with %d pre-warmed devices", size)
	return pool, nil
}

func (p *NetlinkPool) AcquireTap() (*TapDevice, error) {
	return p.Acquire()
}

func (p *NetlinkPool) ReleaseTap(tap *TapDevice) {
	p.Release(tap)
}

func (p *NetlinkPool) Available() int {
	return p.AvailableCount()
}

func (p *NetlinkPool) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, tap := range append(p.available, func() []*TapDevice {
		var inUseList []*TapDevice
		for _, v := range p.inUse {
			inUseList = append(inUseList, v)
		}
		return inUseList
	}()...) {
		link, err := netlink.LinkByName(tap.Name)
		if err == nil {
			_ = netlink.LinkDel(link)
		}
	}
	return nil
}
