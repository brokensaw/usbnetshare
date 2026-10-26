//go:build libusb && !windows

package server

import (
	"log/slog"

	"github.com/google/gousb"
)

// UsbDk solo existe en Windows.
func switchToUsbDk(*gousb.Context, *slog.Logger) (active, recreate bool) { return false, false }
