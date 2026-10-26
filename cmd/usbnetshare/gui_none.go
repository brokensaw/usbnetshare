//go:build !desktop

package main

import (
	"errors"
	"log/slog"

	"github.com/brokensaw/usbnetshare/internal/config"
	"github.com/brokensaw/usbnetshare/internal/engine"
)

// Binario sin interfaz gráfica (p.ej. servidor Linux en una Raspberry Pi).
func runGUI(string, *config.App, *engine.Engine, *slog.Logger) error {
	return errors.New("este binario se compiló sin interfaz gráfica: usa -headless o -service install")
}
