// Package usbids traduce el VID y el PID de un dispositivo USB al nombre de
// su fabricante y de su producto, con la tabla pública usb.ids
// (third_party/usb.ids) embebida en el ejecutable.
package usbids

import (
	"strconv"
	"strings"
	"sync"

	thirdparty "github.com/brokensaw/usbnetshare/third_party"
)

var (
	once     sync.Once
	vendors  map[uint16]string
	products map[uint32]string // vid<<16 | pid
)

// load lee la sección de fabricantes y productos de usb.ids:
//
//	vvvv  fabricante
//	<tab>pppp  producto
//	<tab><tab>ii  interfaz (se ignora)
//
// Las secciones siguientes (clases, HID, idiomas...) empiezan con letras y
// no casan con el formato de 4 cifras hexadecimales.
func load() {
	vendors = make(map[uint16]string, 3500)
	products = make(map[uint32]string, 20000)
	var vendor uint16
	inVendor := false
	for _, l := range strings.Split(thirdparty.USBIDs, "\n") {
		l = strings.TrimRight(l, "\r")
		if l == "" || l[0] == '#' {
			continue
		}
		if l[0] == '\t' {
			if !inVendor || strings.HasPrefix(l, "\t\t") {
				continue
			}
			if id, name, ok := entry(l[1:]); ok {
				products[uint32(vendor)<<16|uint32(id)] = name
			}
			continue
		}
		id, name, ok := entry(l)
		inVendor = ok
		if ok {
			vendor = id
			vendors[id] = name
		}
	}
}

// entry separa "hhhh  nombre".
func entry(l string) (uint16, string, bool) {
	if len(l) < 6 || l[4] != ' ' {
		return 0, "", false
	}
	v, err := strconv.ParseUint(l[:4], 16, 16)
	if err != nil {
		return 0, "", false
	}
	name := strings.TrimSpace(strings.ToValidUTF8(l[4:], ""))
	return uint16(v), name, name != ""
}

// Vendor devuelve el nombre del fabricante o "" si no se conoce.
func Vendor(vid uint16) string {
	once.Do(load)
	return vendors[vid]
}

// Product devuelve el nombre del producto o "" si no se conoce.
func Product(vid, pid uint16) string {
	once.Do(load)
	return products[uint32(vid)<<16|uint32(pid)]
}

// Fill completa con usb.ids el producto y el fabricante que falten; los que
// ya vienen (los que da el propio dispositivo) se respetan.
func Fill(vid, pid uint16, product, manufacturer string) (string, string) {
	if product == "" {
		product = Product(vid, pid)
	}
	if manufacturer == "" {
		manufacturer = Vendor(vid)
	}
	return product, manufacturer
}

// FillHex es Fill con VID y PID en hexadecimal ("0908", "0004"); si no se
// pueden leer, devuelve los nombres tal cual.
func FillHex(vid, pid, product, manufacturer string) (string, string) {
	v, err1 := strconv.ParseUint(strings.TrimPrefix(strings.ToLower(vid), "0x"), 16, 16)
	p, err2 := strconv.ParseUint(strings.TrimPrefix(strings.ToLower(pid), "0x"), 16, 16)
	if err1 != nil {
		return product, manufacturer
	}
	if err2 != nil {
		return product, nonEmpty(manufacturer, Vendor(uint16(v)))
	}
	return Fill(uint16(v), uint16(p), product, manufacturer)
}

func nonEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
