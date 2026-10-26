//go:build windows

package client

import (
	"fmt"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// El error que veía el cliente: "winapi error #3775004676" (0xE1020004, ST_NODEV).
func TestExplainUsbipStatus(t *testing.T) {
	err := explain(fmt.Errorf("attach: %w", windows.Errno(3775004676)))
	if !strings.Contains(err.Error(), "ya no tiene ese dispositivo") || !strings.Contains(err.Error(), "0xE1020004") {
		t.Fatalf("mensaje: %v", err)
	}
}
