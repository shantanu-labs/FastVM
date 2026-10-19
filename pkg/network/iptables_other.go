//go:build !linux
// +build !linux

package network

import "github.com/sirupsen/logrus"

// SetupFirewallRules is a no-op stub for non-Linux platforms.
func SetupFirewallRules(egressIface string, tapName string, log *logrus.Logger) error {
	log.Debugf("[Mock] Skipping iptables configuration on non-Linux platform for %s", tapName)
	return nil
}
