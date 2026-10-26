//go:build windows

package client

// En Windows el controlador USB virtual lo proporciona el driver de
// usbip-win2 (https://github.com/vadimgrn/usbip-win2), que se instala con el
// instalador de USBnetShare. Aquí se habla con él directamente por DeviceIoControl,
// sin usbip.exe ni libusbip.dll: el driver abre la conexión TCP por sí mismo
// con la IP descubierta en ese momento.
//
// Las estructuras replican include/usbip/vhci.h de usbip-win2 v0.9.8.1
// (alineación de 4 bytes, MSVC x64). Se codifican byte a byte para no
// depender del relleno de los structs de Go.

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/brokensaw/usbnetshare/internal/config"
	"github.com/brokensaw/usbnetshare/internal/protocol"
)

// GUID_DEVINTERFACE_USBIP_VHCI
var vhciGUID = windows.GUID{Data1: 0xB4030C06, Data2: 0xDC5F, Data3: 0x4FCC,
	Data4: [8]byte{0x87, 0xEB, 0xE5, 0x51, 0x5A, 0x09, 0x35, 0xC0}}

// CTL_CODE(FILE_DEVICE_UNKNOWN, fn, METHOD_BUFFERED, FILE_READ_DATA|FILE_WRITE_DATA)
func ctlCode(fn uint32) uint32 { return 0x22<<16 | 3<<14 | fn<<2 }

var (
	ioctlPlugout            = ctlCode(0x801)
	ioctlGetImportedDevices = ctlCode(0x802)
	ioctlStopAttachAttempts = ctlCode(0x805)
	ioctlPluginOnce         = ctlCode(0x806)
)

const (
	busIDSize  = 32
	serviceLen = 32
	hostLen    = 1025

	// imported_device_location
	locPort    = 0
	locBusID   = 8
	locService = locBusID + busIDSize     // 40
	locHost    = locService + serviceLen  // 72
	locSize    = 1100                     // 1097 redondeado a 4
	propsSize  = 32                       // imported_device_properties
	impDevSize = locSize + propsSize      // 1132
	pluginSize = 4 + locSize + 16 + 1 + 3 // base + location + serial + wsk_events + relleno = 1124
	stopSize   = 4 + locSize + 4          // 1108
	getImpHdr  = 4                        // get_imported_devices: base
)

// RequiredUsbipVersion es la versión de usbip-win2 cuyo formato de IOCTL
// replica este fichero (y la que incluye el instalador de USBnetShare).
const RequiredUsbipVersion = "0.9.8.1"

const usbipDownload = "https://github.com/vadimgrn/usbip-win2/releases/tag/v." + RequiredUsbipVersion

// usbipVersion lee la versión de usbip.exe instalado (solo para informar:
// la compatibilidad real se comprueba con probeDriver).
func usbipVersion() string {
	pf := os.Getenv("ProgramFiles")
	if pf == "" {
		pf = `C:\Program Files`
	}
	path := filepath.Join(pf, "USBip", "usbip.exe")
	size, err := windows.GetFileVersionInfoSize(path, nil)
	if err != nil || size == 0 {
		return "desconocida"
	}
	buf := make([]byte, size)
	if windows.GetFileVersionInfo(path, 0, size, unsafe.Pointer(&buf[0])) != nil {
		return "desconocida"
	}
	var fi *windows.VS_FIXEDFILEINFO
	var n uint32
	if windows.VerQueryValue(unsafe.Pointer(&buf[0]), `\`, unsafe.Pointer(&fi), &n) != nil || fi == nil {
		return "desconocida"
	}
	return fmt.Sprintf("%d.%d.%d.%d", fi.FileVersionMS>>16, fi.FileVersionMS&0xffff,
		fi.FileVersionLS>>16, fi.FileVersionLS&0xffff)
}

func incompatible(detail string) error {
	return fmt.Errorf("la versión de usbip-win2 instalada (%s) no es compatible: USBnetShare necesita la %s. "+
		"Instálala desde %s o con el instalador de USBnetShare (%s)", usbipVersion(), RequiredUsbipVersion, usbipDownload, detail)
}

// probeDriver comprueba que el driver entiende nuestras IOCTL con el formato
// de 0.9.8.1. Usa STOP_ATTACH_ATTEMPTS con una ubicación inexistente, que no
// cancela nada real. Un driver anterior a 0.9.7.6 no conoce la IOCTL
// (ERROR_INVALID_FUNCTION); uno con otro formato responde con otro tamaño.
func probeDriver() error {
	b := make([]byte, stopSize)
	binary.LittleEndian.PutUint32(b, stopSize)
	if err := putLocation(b[4:], "USBnetShare-probe.invalid", "0", "0-0"); err != nil {
		return err
	}
	n, err := ioctl(ioctlStopAttachAttempts, b, b)
	switch {
	case errors.Is(err, windows.ERROR_INVALID_FUNCTION):
		return incompatible("el driver es anterior a la 0.9.7.6")
	case err != nil:
		return incompatible("el driver rechazó la consulta: " + err.Error())
	case n != stopSize:
		return incompatible(fmt.Sprintf("formato distinto: %d bytes en lugar de %d", n, stopSize))
	}
	return nil
}

// DriverInfo resume el estado del controlador virtual para la GUI.
func DriverInfo() string {
	h, err := openVHCI()
	if err != nil {
		return err.Error()
	}
	windows.CloseHandle(h)
	if err := probeDriver(); err != nil {
		return err.Error()
	}
	return "usbip-win2 " + usbipVersion() + " · compatible"
}

// usbipErrors son los códigos propios de usbip-win2 (userspace/resources/
// messages.mc): severidad error + bit de cliente (0xE…), facilidad Driver
// (0x100) o Device (0x102, la respuesta del servidor al import) e id.
var usbipErrors = map[uint32]string{
	0xE1000001: "el driver de usbip-win2 no pudo completar la operación",
	0xE1000002: "versión del protocolo USB/IP incompatible con el servidor",
	0xE1000003: "el servidor respondió algo que no cumple el protocolo USB/IP",
	0xE1000004: "no quedan puertos USB virtuales libres en este equipo",
	0xE1000005: "versión del driver de usbip-win2 incompatible con USBnetShare",
	0xE1020001: "el dispositivo no está disponible en el servidor",
	0xE1020002: "el dispositivo está en uso por otro cliente",
	0xE1020003: "el servidor no pudo abrir el dispositivo (revisa su registro)",
	0xE1020004: "el servidor ya no tiene ese dispositivo: se desenchufó o se volvió a conectar en otro puerto",
	0xE1020005: "respuesta inesperada del servidor",
}

// Errores típicos traducidos a algo comprensible.
func explain(err error) error {
	var en windows.Errno
	if errors.As(err, &en) {
		switch en {
		case windows.ERROR_INVALID_FUNCTION:
			return fmt.Errorf("el driver de usbip-win2 no reconoce la operación; versión incompatible, USBnetShare necesita la %s (%w)", RequiredUsbipVersion, err)
		case windows.ERROR_NOT_FOUND, 1169 /* ERROR_NO_MATCH */ : // WskGetAddressInfo
			return fmt.Errorf("servidor no encontrado (%w)", err)
		case windows.ERROR_SEM_TIMEOUT:
			return fmt.Errorf("tiempo de conexión agotado (%w)", err)
		case windows.ERROR_ACCESS_DENIED:
			return fmt.Errorf("acceso denegado al driver de usbip-win2: ejecuta como administrador o instala USBnetShare como servicio (%w)", err)
		}
		if msg, ok := usbipErrors[uint32(en)]; ok {
			return fmt.Errorf("%s (usbip-win2 0x%08X)", msg, uint32(en))
		}
	}
	return err
}

func openVHCI() (windows.Handle, error) {
	paths, err := windows.CM_Get_Device_Interface_List("", &vhciGUID, windows.CM_GET_DEVICE_INTERFACE_LIST_PRESENT)
	if err != nil {
		return 0, fmt.Errorf("buscando el driver de usbip-win2: %w", err)
	}
	if len(paths) == 0 || paths[0] == "" {
		return 0, errors.New("el driver de usbip-win2 no está instalado: ejecuta el instalador de USBnetShare (incluye el driver)")
	}
	p, err := windows.UTF16PtrFromString(paths[0])
	if err != nil {
		return 0, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return 0, explain(fmt.Errorf("abriendo el driver de usbip-win2: %w", err))
	}
	return h, nil
}

func ioctl(code uint32, in []byte, out []byte) (uint32, error) {
	h, err := openVHCI()
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(h)
	var inp, outp *byte
	if len(in) > 0 {
		inp = &in[0]
	}
	if len(out) > 0 {
		outp = &out[0]
	}
	var n uint32
	err = windows.DeviceIoControl(h, code, inp, uint32(len(in)), outp, uint32(len(out)), &n, nil)
	return n, err
}

func putStr(b []byte, s string, max int) error {
	if len(s) >= max {
		return fmt.Errorf("%q demasiado largo (máx. %d)", s, max-1)
	}
	copy(b, s)
	return nil
}

func getStr(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// putLocation escribe imported_device_location en b (que empieza en port).
func putLocation(b []byte, host, service, busid string) error {
	if err := putStr(b[locBusID:], busid, busIDSize); err != nil {
		return err
	}
	if err := putStr(b[locService:], service, serviceLen); err != nil {
		return err
	}
	return putStr(b[locHost:], host, hostLen)
}

type importedDevice struct {
	Port                 int
	BusID, Service, Host string
	DevID                uint32
	Vendor, Product      uint16
}

func getImportedDevices() ([]importedDevice, error) {
	le := binary.LittleEndian
	for cnt := 4; cnt <= 1024; cnt *= 2 {
		buf := make([]byte, getImpHdr+cnt*impDevSize)
		le.PutUint32(buf, getImpHdr+impDevSize) // sizeof(get_imported_devices)
		n, err := ioctl(ioctlGetImportedDevices, buf[:4], buf)
		if errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) || errors.Is(err, windows.ERROR_MORE_DATA) {
			continue
		}
		if err != nil {
			return nil, explain(err)
		}
		if n < getImpHdr || (n-getImpHdr)%impDevSize != 0 {
			return nil, fmt.Errorf("respuesta inesperada del driver (%d bytes); ¿versión de usbip-win2 distinta de la 0.9.8.x?", n)
		}
		var out []importedDevice
		for o := getImpHdr; o+impDevSize <= int(n); o += impDevSize {
			d := buf[o : o+impDevSize]
			p := d[locSize:]
			out = append(out, importedDevice{
				Port:    int(int32(le.Uint32(d[locPort:]))),
				BusID:   getStr(d[locBusID : locBusID+busIDSize]),
				Service: getStr(d[locService : locService+serviceLen]),
				Host:    getStr(d[locHost : locHost+hostLen]),
				DevID:   le.Uint32(p[0:]),
				Vendor:  le.Uint16(p[8:]),
				Product: le.Uint16(p[10:]),
			})
		}
		return out, nil
	}
	return nil, errors.New("demasiados dispositivos importados")
}

func plugout(port int) error {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint32(b, 8)
	binary.LittleEndian.PutUint32(b[4:], uint32(int32(port)))
	_, err := ioctl(ioctlPlugout, b, nil)
	return explain(err)
}

func stopAttachAttempts(host, service, busid string) {
	b := make([]byte, stopSize)
	binary.LittleEndian.PutUint32(b, stopSize)
	if putLocation(b[4:], host, service, busid) == nil {
		_, _ = ioctl(ioctlStopAttachAttempts, b, b)
	}
}

type windowsAttacher struct {
	cfg *config.Client
	log *slog.Logger

	mu    sync.Mutex
	owned map[int]bool // puertos que gestiona este proceso
}

func newAttacher(cfg *config.Client, log *slog.Logger) (Attacher, error) {
	return &windowsAttacher{cfg: cfg, log: log, owned: map[int]bool{}}, nil
}

func (a *windowsAttacher) Prepare() error {
	h, err := openVHCI()
	if err != nil {
		return err
	}
	windows.CloseHandle(h)
	return probeDriver()
}

func (a *windowsAttacher) Attach(ctx context.Context, host string, port int, dev protocol.Device) (Attachment, error) {
	service := strconv.Itoa(port)

	// Limpia un attach anterior del mismo busid que no sea nuestro
	// (p.ej. el que quedó apuntando a la IP antigua del servidor, o el de
	// una ejecución anterior que terminó de forma abrupta).
	if devs, err := getImportedDevices(); err == nil {
		for _, d := range devs {
			a.mu.Lock()
			mine := a.owned[d.Port]
			a.mu.Unlock()
			if d.BusID == dev.BusID && !mine {
				a.log.Info("liberando importación anterior", "puerto", d.Port, "host", d.Host, "busid", d.BusID)
				_ = plugout(d.Port)
			}
		}
	}

	b := make([]byte, pluginSize)
	binary.LittleEndian.PutUint32(b, pluginSize)
	if err := putLocation(b[4:], host, service, dev.BusID); err != nil {
		return nil, err
	}
	// serial vacío y wsk_events=false (modo zero_copy por defecto).

	// El driver conecta y negocia por sí mismo; si cancelan mientras tanto,
	// se le pide que abandone el intento.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			stopAttachAttempts(host, service, dev.BusID)
		case <-done:
		}
	}()

	n, err := ioctl(ioctlPluginOnce, b, b[:8])
	if err != nil {
		return nil, fmt.Errorf("attach (driver usbip-win2): %w", explain(err))
	}
	if n != 8 {
		return nil, fmt.Errorf("respuesta inesperada del driver al conectar (%d bytes)", n)
	}
	vport := int(int32(binary.LittleEndian.Uint32(b[4:])))
	if vport <= 0 {
		return nil, fmt.Errorf("el driver devolvió un puerto no válido (%d)", vport)
	}
	a.mu.Lock()
	a.owned[vport] = true
	a.mu.Unlock()
	return &windowsAttachment{a: a, host: host, busid: dev.BusID, port: vport}, nil
}

type windowsAttachment struct {
	a     *windowsAttacher
	host  string
	busid string
	port  int
}

func (w *windowsAttachment) String() string { return fmt.Sprintf("usbip-win2 puerto %d", w.port) }

func (w *windowsAttachment) Alive() (bool, error) {
	devs, err := getImportedDevices()
	if err != nil {
		return false, err
	}
	for _, d := range devs {
		if d.Port == w.port && d.BusID == w.busid {
			return true, nil
		}
	}
	return false, nil
}

func (w *windowsAttachment) Detach() error {
	defer func() {
		w.a.mu.Lock()
		delete(w.a.owned, w.port)
		w.a.mu.Unlock()
	}()
	alive, err := w.Alive()
	if err != nil || !alive {
		return err
	}
	if err := plugout(w.port); err != nil {
		return fmt.Errorf("detach (driver usbip-win2): %w", err)
	}
	// Da tiempo a que Windows retire el dispositivo antes de reutilizar el puerto.
	time.Sleep(200 * time.Millisecond)
	return nil
}
