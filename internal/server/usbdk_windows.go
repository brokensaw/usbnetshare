//go:build libusb && windows

package server

// UsbDk (https://github.com/daynix/UsbDk) es un driver filtro que libusb sabe
// usar en Windows: al abrir un dispositivo lo aparta de Windows y lo devuelve
// al cerrarlo, sin cambiar su driver. Así se comparte cualquier USB (PC
// Adapter, conversores USB-RS485...) sin instalar WinUSB con Zadig.
//
// LIBUSB_OPTION_USE_USBDK solo funciona sobre un contexto ya inicializado:
// libusb (1.0.27–1.0.29) aplica las opciones de libusb_init_context y las
// predeterminadas antes de iniciar el backend de Windows, que además vuelve a
// fijar WinUSB, así que fallan con LIBUSB_ERROR_NOT_FOUND. gousb no expone su
// contexto, por eso se lee su campo privado con reflect; si en otra versión de
// gousb no existe, se sigue con WinUSB.

/*
#cgo pkg-config: libusb-1.0
#include <libusb.h>

// libusb_set_option es variádica: cgo no puede llamarla directamente.
static int usbnet_use_usbdk(libusb_context *ctx) {
	return libusb_set_option(ctx, LIBUSB_OPTION_USE_USBDK);
}
*/
import "C"

import (
	"log/slog"
	"reflect"
	"sync"

	"github.com/google/gousb"

	"github.com/brokensaw/usbnetshare/internal/usbdrv"
)

var usbdk struct {
	sync.Mutex
	active bool
	err    string
}

// switchToUsbDk pasa un contexto recién creado al backend UsbDk si está
// instalado y la configuración no lo impide, y comprueba que UsbDk consigue
// enumerar. Devuelve si quedó activo; si no, el contexto sigue con WinUSB o
// hay que recrearlo (recreate).
func switchToUsbDk(c *gousb.Context, log *slog.Logger) (active, recreate bool) {
	if !useUsbDk.Load() || !usbdrv.UsbDkInstalled() {
		setUsbDk(false, "", log)
		return false, false
	}
	f := reflect.ValueOf(c).Elem().FieldByName("ctx")
	if !f.IsValid() || f.Kind() != reflect.Pointer || f.IsNil() {
		setUsbDk(false, "esta versión de gousb no permite activarlo", log)
		return false, false
	}
	if rc := C.usbnet_use_usbdk((*C.libusb_context)(f.UnsafePointer())); rc != 0 {
		setUsbDk(false, "libusb no lo acepta: "+C.GoString(C.libusb_error_name(rc)), log)
		return false, false
	}
	// UsbDk_GetDevicesList puede fallar con algunos dispositivos enchufados
	// (libusb devuelve LIBUSB_ERROR_OTHER y no lista nada): mejor WinUSB.
	if _, err := c.OpenDevices(func(*gousb.DeviceDesc) bool { return false }); err != nil {
		setUsbDk(false, "UsbDk no consigue enumerar los USB ("+err.Error()+"); "+
			"suele deberse a un dispositivo concreto: desenchufa los USB que no compartes y reinicia USBnetShare", log)
		return false, true
	}
	setUsbDk(true, "", log)
	return true, false
}

// setUsbDk guarda el estado y registra solo los cambios.
func setUsbDk(active bool, reason string, log *slog.Logger) {
	usbdk.Lock()
	defer usbdk.Unlock()
	if active == usbdk.active && reason == usbdk.err {
		return
	}
	usbdk.active, usbdk.err = active, reason
	switch {
	case active:
		log.Info("UsbDk activo: se puede compartir cualquier USB sin cambiar su driver")
	case reason != "":
		log.Warn("UsbDk instalado pero no se usa; se usa WinUSB", "motivo", reason)
	}
}

// UsbDkActive indica si el backend libusb usa UsbDk.
func UsbDkActive() bool {
	usbdk.Lock()
	defer usbdk.Unlock()
	return usbdk.active
}

// UsbDkError explica por qué UsbDk está instalado pero no se usa ("" si no aplica).
func UsbDkError() string {
	usbdk.Lock()
	defer usbdk.Unlock()
	return usbdk.err
}
