//go:build linux
// +build linux

package main

import (
	"net"
	"os"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

type vsockConn struct {
	fd int
}

func (c *vsockConn) Read(b []byte) (int, error) {
	n, err := unix.Read(c.fd, b)
	if n < 0 {
		n = 0
	}
	return n, err
}

func (c *vsockConn) Write(b []byte) (int, error) {
	return unix.Write(c.fd, b)
}

func (c *vsockConn) Close() error {
	return unix.Close(c.fd)
}

func (c *vsockConn) LocalAddr() net.Addr                { return nil }
func (c *vsockConn) RemoteAddr() net.Addr               { return nil }
func (c *vsockConn) SetDeadline(t time.Time) error      { return nil }
func (c *vsockConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *vsockConn) SetWriteDeadline(t time.Time) error { return nil }

func mountInitFilesystems(log *logrus.Logger) {
	if os.Getpid() != 1 {
		return
	}
	_ = os.Setenv("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	_ = os.MkdirAll("/proc", 0755)
	_ = os.MkdirAll("/sys", 0755)
	_ = os.MkdirAll("/dev", 0755)
	_ = os.MkdirAll("/tmp", 01777)

	_ = syscall.Mount("proc", "/proc", "proc", 0, "")
	_ = syscall.Mount("sysfs", "/sys", "sysfs", 0, "")
	_ = syscall.Mount("devtmpfs", "/dev", "devtmpfs", 0, "")
	_ = syscall.Mount("tmpfs", "/tmp", "tmpfs", 0, "")

	// Protect PID 1 (agent-init) from guest kernel OOM-killer and cap max threads to contain fork bombs
	_ = os.WriteFile("/proc/1/oom_score_adj", []byte("-1000\n"), 0644)
	_ = os.WriteFile("/proc/sys/kernel/threads-max", []byte("128\n"), 0644)
}

func startVsockServer(port int, log *logrus.Logger) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		log.Debugf("AF_VSOCK socket unavailable: %v", err)
		return
	}

	sa := &unix.SockaddrVM{
		CID:  unix.VMADDR_CID_ANY,
		Port: uint32(port),
	}
	if err := unix.Bind(fd, sa); err != nil {
		_ = unix.Close(fd)
		log.Warnf("AF_VSOCK bind on port %d failed: %v", port, err)
		return
	}

	if err := unix.Listen(fd, 64); err != nil {
		_ = unix.Close(fd)
		log.Warnf("AF_VSOCK listen on port %d failed: %v", port, err)
		return
	}

	log.Infof("Listening on AF_VSOCK port %d", port)
	go func() {
		for {
			nfd, _, err := unix.Accept(fd)
			if err != nil {
				continue
			}
			go handleConnection(&vsockConn{fd: nfd}, log)
		}
	}()
}
