//go:build linux

package netutil

import (
	"net"

	"golang.org/x/sys/unix"

	"github.com/brokensaw/usbnetshare/internal/config"
)

func tuneOS(tc *net.TCPConn, ka config.KeepAlive) {
	rc, err := tc.SyscallConn()
	if err != nil {
		return
	}
	_ = rc.Control(func(fd uintptr) {
		f := int(fd)
		_ = unix.SetsockoptInt(f, unix.IPPROTO_TCP, unix.TCP_KEEPIDLE, int(ka.Idle.Seconds()))
		_ = unix.SetsockoptInt(f, unix.IPPROTO_TCP, unix.TCP_KEEPINTVL, int(ka.Interval.Seconds()))
		_ = unix.SetsockoptInt(f, unix.IPPROTO_TCP, unix.TCP_KEEPCNT, ka.Count)
		// Límite para datos sin confirmar: evita sesiones "zombis" de minutos.
		_ = unix.SetsockoptInt(f, unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT, int(ka.UserTimeout.Milliseconds()))
	})
}
