// Package drivers lleva dentro de USBnetShare.exe los instaladores oficiales de
// los drivers que necesita en Windows y los instala sin ventanas:
//
//   - usbip-win2: controlador USB virtual del rol cliente.
//   - UsbDk: filtro que permite al rol servidor (libusb) abrir cualquier USB
//     sin cambiar su driver (alternativa a WinUSB/Zadig).
//
// Los instaladores no están en git: `make drivers` (go run ./internal/drivers/fetch)
// los descarga en payload/ comprobando su SHA-256, y se incrustan al compilar
// con -tags drivers. Sin esa etiqueta USBnetShare funciona igual, pero sin poder
// instalarlos (Bundled() devuelve false).
package drivers

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Package describe un instalador incrustado.
type Package struct {
	ID      string // "usbip" | "usbdk"
	Name    string
	Version string
	File    string // nombre dentro de payload/
	URL     string // release oficial
	SHA256  string
	License string // licencia del instalador (ver third_party/)
}

var Packages = []Package{
	{
		ID: "usbip", Name: "usbip-win2", Version: "0.9.8.1",
		File:    "USBip-0.9.8.1-x64.exe",
		URL:     "https://github.com/vadimgrn/usbip-win2/releases/download/v.0.9.8.1/USBip-0.9.8.1-x64.exe",
		SHA256:  "38cad6d4432b52d5bb9409d9ad03b72fdffc4ada4cd3a48fbeca1a2752a8518a",
		License: "BSD 2-Clause",
	},
	{
		ID: "usbdk", Name: "UsbDk", Version: "1.0.22",
		File:    "UsbDk_1.0.22_x64.msi",
		URL:     "https://github.com/daynix/UsbDk/releases/download/v1.00-22/UsbDk_1.0.22_x64.msi",
		SHA256:  "91f6f695e1e13c656024e6d3b55620bf08d8835ef05ee0496935ba6bb62466a5",
		License: "Apache 2.0",
	},
}

// Find busca un paquete por su ID.
func Find(id string) (Package, bool) {
	for _, p := range Packages {
		if p.ID == strings.ToLower(strings.TrimSpace(id)) {
			return p, true
		}
	}
	return Package{}, false
}

// Verify comprueba que data es exactamente el instalador publicado.
func (p Package) Verify(data []byte) error {
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != p.SHA256 {
		return fmt.Errorf("%s: SHA-256 %s no coincide con el esperado %s", p.File, got, p.SHA256)
	}
	return nil
}

// RebootExitCode es el código de salida de `USBnetShare -install-drivers` cuando
// hace falta reiniciar (el mismo que usa Windows Installer).
const RebootExitCode = 3010
