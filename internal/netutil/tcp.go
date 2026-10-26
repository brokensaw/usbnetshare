// Package netutil ajusta los sockets TCP para detectar enlaces caídos sin
// matar sesiones por microcortes.
package netutil

import (
	"net"

	"github.com/brokensaw/usbnetshare/internal/config"
)

// TuneTCP activa TCP_NODELAY (latencia baja, importante para protocolos
// de PLC tipo MPI/PROFIBUS) y keepalive. Los ajustes persisten incluso cuando
// el socket se entrega al kernel (usbip-host / vhci-hcd).
func TuneTCP(c net.Conn, ka config.KeepAlive) {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return
	}
	_ = tc.SetNoDelay(true)
	_ = tc.SetKeepAlive(true)
	_ = tc.SetKeepAlivePeriod(ka.Interval)
	tuneOS(tc, ka)
}
