//go:build linux
// +build linux

package network

import (
	"os/exec"
	"strings"
	"sync"

	"github.com/sirupsen/logrus"
)

var configuredTaps sync.Map

// detectDefaultIface finds the host's default route interface (e.g. ens4 on GCP, eth0 on AWS).
func detectDefaultIface() string {
	out, err := exec.Command("sh", "-c", "ip route show default | awk '/default/ {print $5; exit}'").Output()
	if err == nil {
		iface := strings.TrimSpace(string(out))
		if iface != "" {
			return iface
		}
	}
	return "eth0"
}

// ensureRule checks if an iptables rule exists (-C) before inserting/appending it.
func ensureRule(table string, action string, chain string, ruleArgs []string, log *logrus.Logger) {
	checkArgs := []string{}
	if table != "" {
		checkArgs = append(checkArgs, "-t", table)
	}
	checkArgs = append(checkArgs, "-C", chain)
	checkArgs = append(checkArgs, ruleArgs...)
	if err := exec.Command("iptables", checkArgs...).Run(); err == nil {
		return
	}

	applyArgs := []string{}
	if table != "" {
		applyArgs = append(applyArgs, "-t", table)
	}
	applyArgs = append(applyArgs, action, chain)
	applyArgs = append(applyArgs, ruleArgs...)
	if out, err := exec.Command("iptables", applyArgs...).CombinedOutput(); err != nil {
		log.Warnf("iptables %v warning: %v (%s)", applyArgs, err, string(out))
	}
}

// SetupFirewallRules applies the strict iptables security sandbox on Linux.
func SetupFirewallRules(egressIface string, tapName string, log *logrus.Logger) error {
	if _, loaded := configuredTaps.LoadOrStore(tapName, true); loaded {
		return nil
	}
	if egressIface == "" {
		egressIface = detectDefaultIface()
	}

	// 1. Enable IPv4 forwarding
	_ = exec.Command("sysctl", "-w", "net.ipv4.ip_forward=1").Run()

	// 2. Configure NAT masquerading
	ensureRule("nat", "-A", "POSTROUTING", []string{"-o", egressIface, "-j", "MASQUERADE"}, log)

	// 3. Protect Host (Drop direct traffic targeting the host gateway from the VM TAP)
	ensureRule("", "-I", "INPUT", []string{"-i", tapName, "-j", "DROP"}, log)

	// 4. Default DROP and outbound internet allow at bottom of FORWARD chain for tapName
	ensureRule("", "-A", "FORWARD", []string{"-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"}, log)
	ensureRule("", "-A", "FORWARD", []string{"-i", tapName, "-o", egressIface, "-j", "ACCEPT"}, log)
	ensureRule("", "-A", "FORWARD", []string{"-i", tapName, "-j", "DROP"}, log)

	// 5. Insert strict SSRF & RFC1918 DROP rules at the TOP of FORWARD so they take precedence
	ensureRule("", "-I", "FORWARD", []string{"-i", tapName, "-d", "192.168.0.0/16", "-j", "DROP"}, log)
	ensureRule("", "-I", "FORWARD", []string{"-i", tapName, "-d", "172.16.0.0/12", "-j", "DROP"}, log)
	ensureRule("", "-I", "FORWARD", []string{"-i", tapName, "-d", "10.0.0.0/8", "-j", "DROP"}, log)
	ensureRule("", "-I", "FORWARD", []string{"-i", tapName, "-d", "169.254.169.254/32", "-j", "DROP"}, log)

	log.Infof("Configured strict firewall and SSRF egress lockdown for %s (egress: %s)", tapName, egressIface)
	return nil
}
