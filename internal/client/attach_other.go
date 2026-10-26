//go:build !linux && !windows

package client

import (
	"errors"
	"log/slog"

	"github.com/brokensaw/usbnetshare/internal/config"
)

func newAttacher(cfg *config.Client, log *slog.Logger) (Attacher, error) {
	return nil, errors.New("cliente no soportado en este sistema operativo (solo Linux y Windows)")
}

// DriverInfo resume el estado del controlador virtual para la GUI.
func DriverInfo() string { return "cliente no soportado en este sistema operativo" }
