//go:build windows

package usbdrv

import (
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/sys/windows"
)

// DEVPKEY_Device_BusReportedDeviceDesc: la cadena de producto del dispositivo.
var busReportedDesc = windows.DEVPROPKEY{
	FmtID: windows.DEVPROPGUID{Data1: 0x540b947e, Data2: 0x8b40, Data3: 0x45bc,
		Data4: [8]byte{0xa8, 0xa2, 0x6a, 0x0b, 0x89, 0x4c, 0xbd, 0xa2}},
	PID: 4,
}

var idRe = regexp.MustCompile(`(?i)^USB\\VID_([0-9A-F]{4})&PID_([0-9A-F]{4})(&MI_[0-9A-F]{2})?\\`)

// Lookup enumera los USB presentes y devuelve su driver por "vvvv:pppp".
// En dispositivos compuestos (usbccgp) cuenta el driver de cada interfaz:
// solo son compartibles si todas tienen un driver compatible con libusb.
func Lookup() map[string]Info {
	set, err := windows.SetupDiGetClassDevsEx(nil, "USB", 0, windows.DIGCF_ALLCLASSES|windows.DIGCF_PRESENT, 0, "")
	if err != nil {
		return nil
	}
	defer set.Close()

	parent := map[string]string{}
	product := map[string]string{}
	ifaces := map[string][]string{}
	for i := 0; ; i++ {
		data, err := set.EnumDeviceInfo(i)
		if err != nil {
			break // ERROR_NO_MORE_ITEMS
		}
		id, err := set.DeviceInstanceID(data)
		if err != nil {
			continue
		}
		m := idRe.FindStringSubmatch(id)
		if m == nil {
			continue
		}
		vid, _ := strconv.ParseUint(m[1], 16, 16)
		pid, _ := strconv.ParseUint(m[2], 16, 16)
		key := Key(uint16(vid), uint16(pid))
		svc := ""
		if v, err := set.DeviceRegistryProperty(data, windows.SPDRP_SERVICE); err == nil {
			svc, _ = v.(string)
		}
		if m[3] != "" {
			ifaces[key] = append(ifaces[key], nonEmpty(svc, "sin driver"))
		} else {
			if prev, ok := parent[key]; !ok || prev == "" {
				parent[key] = svc
			}
			if v, err := windows.SetupDiGetDeviceProperty(set, data, &busReportedDesc); err == nil {
				if p, _ := v.(string); p != "" && product[key] == "" {
					product[key] = strings.TrimSpace(p)
				}
			}
		}
	}

	out := map[string]Info{}
	for key, svc := range parent {
		info := Info{Driver: svc, Shareable: libusbCapable(svc)}
		if strings.EqualFold(svc, "usbccgp") && len(ifaces[key]) > 0 {
			all := true
			for _, s := range ifaces[key] {
				all = all && libusbCapable(s)
			}
			info = Info{Driver: svc + " (" + strings.Join(ifaces[key], ", ") + ")", Shareable: all}
		}
		if svc == "" {
			info.Driver = "sin driver"
		}
		info.Product = product[key]
		out[key] = info
	}
	return out
}

// UsbDkInstalled indica si el driver UsbDk está instalado: su biblioteca
// auxiliar en System32 es lo que carga libusb para usarlo.
func UsbDkInstalled() bool {
	h, err := windows.LoadLibraryEx("UsbDkHelper.dll", 0, windows.LOAD_LIBRARY_SEARCH_SYSTEM32)
	if err != nil {
		return false
	}
	windows.FreeLibrary(h)
	return true
}

func nonEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
