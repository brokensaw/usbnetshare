//go:build !linux

package netutil

import (
	"net"

	"github.com/brokensaw/usbnetshare/internal/config"
)

// En Windows SetKeepAlivePeriod fija idle e intervalo; el número de sondas
// lo decide el sistema (10).
func tuneOS(tc *net.TCPConn, ka config.KeepAlive) {
	_ = tc.SetKeepAlivePeriod(ka.Idle)
}
