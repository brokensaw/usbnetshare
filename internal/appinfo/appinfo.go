// Package appinfo reúne el nombre del programa y los identificadores con los
// que se instala (servicio, carpeta y fichero de configuración, firewall).
package appinfo

import "runtime"

// Name es el nombre visible del programa.
const Name = "USBnetShare"

// LegacyID es el identificador de las versiones anteriores (usbnet): se usa
// para migrar su configuración, su servicio y su regla de firewall.
const LegacyID = "usbnet"

// ID identifica la instalación: servicio, carpeta y fichero de configuración.
// En Windows se escribe como el nombre; en Linux, en minúsculas.
var ID = func() string {
	if runtime.GOOS == "windows" {
		return Name
	}
	return "usbnetshare"
}()
