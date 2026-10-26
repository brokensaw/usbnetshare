//go:build !windows

package usbdrv

// Lookup no aplica fuera de Windows: en Linux el backend kernel o libusb
// desvinculan el driver ellos mismos.
func Lookup() map[string]Info { return nil }

// UsbDkInstalled: UsbDk solo existe en Windows.
func UsbDkInstalled() bool { return false }
