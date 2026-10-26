// Package thirdparty incrusta los datos de terceros que usa USBnetShare.
//
// usb.ids es la base pública de identificadores USB (http://www.linux-usb.org/usb.ids,
// mantenida por Stephen J. Gowdy), con doble licencia GPL-2.0-or-later /
// BSD-3-Clause; USBnetShare la usa bajo BSD-3-Clause. Para actualizarla basta con
// sustituir el fichero.
package thirdparty

import _ "embed"

//go:embed usb.ids
var USBIDs string
