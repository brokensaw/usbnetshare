//go:build !(libusb && windows)

package server

// UsbDk solo existe en Windows con el backend libusb (ver usbdk_windows.go).

// UsbDkActive indica si el backend libusb usa UsbDk.
func UsbDkActive() bool { return false }

// UsbDkError explica por qué UsbDk está instalado pero no se usa ("" si no aplica).
func UsbDkError() string { return "" }
