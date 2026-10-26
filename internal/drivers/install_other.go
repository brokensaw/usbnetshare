//go:build !windows

package drivers

import "errors"

// Fuera de Windows no hay nada que instalar: Linux trae usbip y libusb.
func Bundled() bool { return false }

func Install([]string, string) (bool, error) {
	return false, errors.New("la instalación de drivers solo existe en Windows")
}
