// Package usbdrv informa del driver que el sistema tiene asignado a cada
// dispositivo USB. En Windows, libusb solo puede abrir dispositivos con
// WinUSB (o libusbK / libusb0), salvo que use UsbDk: entonces abre cualquiera.
package usbdrv

import (
	"fmt"
	"strings"
)

// Info describe el driver de un dispositivo.
type Info struct {
	Driver    string // p.ej. "WinUSB", "rtucx21x64", "usbccgp (HidUsb, WinUSB)"; "" = desconocido
	Shareable bool   // libusb puede abrirlo
	Product   string // nombre que da el propio dispositivo (p.ej. "DataTraveler 3.0")
}

// Key forma la clave "vvvv:pppp" usada por Lookup.
func Key(vid, pid uint16) string { return fmt.Sprintf("%04x:%04x", vid, pid) }

func libusbCapable(service string) bool {
	switch strings.ToLower(service) {
	case "winusb", "libusbk", "libusb0":
		return true
	}
	return false
}

// Hint devuelve una explicación para el registro cuando un dispositivo no se
// puede abrir, o "" si no hay nada útil que decir.
func Hint(vid, pid uint16) string {
	m := Lookup()
	if m == nil {
		return ""
	}
	i, ok := m[Key(vid, pid)]
	if !ok || i.Shareable || i.Driver == "" {
		return ""
	}
	return fmt.Sprintf("el dispositivo usa el driver %q de Windows, no WinUSB: instala UsbDk desde Ajustes → Drivers (o WinUSB con Zadig) para poder compartirlo", i.Driver)
}
