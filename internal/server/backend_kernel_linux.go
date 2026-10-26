//go:build linux

package server

// Backend "kernel": usa el módulo usbip-host de Linux. El kernel procesa las
// URBs directamente (máximo rendimiento y compatibilidad); este proceso solo
// gestiona el vínculo del dispositivo, la negociación y la supervisión.

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/brokensaw/usbnetshare/internal/protocol"
)

const (
	sysDevices   = "/sys/bus/usb/devices"
	sysUsbipHost = "/sys/bus/usb/drivers/usbip-host"

	sdevAvailable = 1
	sdevUsed      = 2
	sdevError     = 3
)

func init() { register("kernel", newKernelBackend) }

type kernelBackend struct{ log *slog.Logger }

func newKernelBackend(log *slog.Logger) (Backend, error) {
	if out, err := exec.Command("modprobe", "usbip-host").CombinedOutput(); err != nil {
		log.Warn("modprobe usbip-host falló", "err", err, "salida", strings.TrimSpace(string(out)))
	}
	if _, err := os.Stat(sysUsbipHost); err != nil {
		return nil, fmt.Errorf("módulo usbip-host no disponible (instala linux-tools/usbip y ejecuta como root): %w", err)
	}
	return &kernelBackend{log: log}, nil
}

func (b *kernelBackend) Name() string { return "kernel (usbip-host)" }
func (b *kernelBackend) Close() error { return nil }

func readAttr(dir, name string) string {
	v, _ := os.ReadFile(filepath.Join(dir, name))
	return strings.TrimSpace(string(v))
}

func hexAttr(dir, name string) uint64 {
	v, _ := strconv.ParseUint(readAttr(dir, name), 16, 32)
	return v
}

func decAttr(dir, name string) uint64 {
	v, _ := strconv.ParseUint(readAttr(dir, name), 10, 32)
	return v
}

func speedFromSysfs(s string) uint32 {
	switch s {
	case "1.5":
		return protocol.SpeedLow
	case "12":
		return protocol.SpeedFull
	case "480":
		return protocol.SpeedHigh
	case "5000":
		return protocol.SpeedSuper
	case "10000", "20000":
		return protocol.SpeedSuperPlus
	}
	return protocol.SpeedUnknown
}

func readSysDevice(busid string) (protocol.Device, error) {
	dir := filepath.Join(sysDevices, busid)
	if _, err := os.Stat(filepath.Join(dir, "idVendor")); err != nil {
		return protocol.Device{}, fmt.Errorf("dispositivo %s no presente", busid)
	}
	path, _ := filepath.EvalSymlinks(dir)
	d := protocol.Device{
		Path:               path,
		BusID:              busid,
		BusNum:             uint32(decAttr(dir, "busnum")),
		DevNum:             uint32(decAttr(dir, "devnum")),
		Speed:              speedFromSysfs(readAttr(dir, "speed")),
		IDVendor:           uint16(hexAttr(dir, "idVendor")),
		IDProduct:          uint16(hexAttr(dir, "idProduct")),
		BcdDevice:          uint16(hexAttr(dir, "bcdDevice")),
		DeviceClass:        uint8(hexAttr(dir, "bDeviceClass")),
		DeviceSubClass:     uint8(hexAttr(dir, "bDeviceSubClass")),
		DeviceProtocol:     uint8(hexAttr(dir, "bDeviceProtocol")),
		ConfigurationValue: uint8(decAttr(dir, "bConfigurationValue")),
		NumConfigurations:  uint8(decAttr(dir, "bNumConfigurations")),
		NumInterfaces:      uint8(decAttr(dir, "bNumInterfaces")),
		Product:            readAttr(dir, "product"),
		Manufacturer:       readAttr(dir, "manufacturer"),
	}
	if d.ConfigurationValue != 0 {
		for i := 0; i < int(d.NumInterfaces); i++ {
			idir := filepath.Join(dir, fmt.Sprintf("%s:%d.%d", busid, d.ConfigurationValue, i))
			d.Interfaces = append(d.Interfaces, protocol.Interface{
				Class:    uint8(hexAttr(idir, "bInterfaceClass")),
				SubClass: uint8(hexAttr(idir, "bInterfaceSubClass")),
				Protocol: uint8(hexAttr(idir, "bInterfaceProtocol")),
			})
		}
	}
	return d, nil
}

func (b *kernelBackend) List() ([]protocol.Device, error) {
	ents, err := os.ReadDir(sysDevices)
	if err != nil {
		return nil, err
	}
	var out []protocol.Device
	for _, e := range ents {
		n := e.Name()
		if strings.Contains(n, ":") || strings.HasPrefix(n, "usb") {
			continue // interfaces y root hubs
		}
		if d, err := readSysDevice(n); err == nil {
			out = append(out, d)
		}
	}
	return out, nil
}

func writeSys(path, val string) error {
	return os.WriteFile(path, []byte(val), 0o200)
}

func currentDriver(busid string) string {
	t, err := os.Readlink(filepath.Join(sysDevices, busid, "driver"))
	if err != nil {
		return ""
	}
	return filepath.Base(t)
}

// ensureBound vincula el dispositivo a usbip-host (equivale a "usbip bind").
func (b *kernelBackend) ensureBound(busid string) error {
	if currentDriver(busid) == "usbip-host" {
		return nil
	}
	_ = writeSys(filepath.Join(sysUsbipHost, "match_busid"), "add "+busid)
	if drv := currentDriver(busid); drv != "" {
		if err := writeSys(filepath.Join(sysDevices, busid, "driver", "unbind"), busid); err != nil {
			return fmt.Errorf("desvinculando de %s: %w", drv, err)
		}
	}
	err := writeSys(filepath.Join(sysUsbipHost, "bind"), busid)
	if currentDriver(busid) == "usbip-host" {
		b.log.Info("dispositivo vinculado a usbip-host", "busid", busid)
		return nil
	}
	return fmt.Errorf("bind a usbip-host falló: %v", err)
}

func usbipStatus(busid string) int {
	v, err := strconv.Atoi(readAttr(filepath.Join(sysDevices, busid), "usbip_status"))
	if err != nil {
		return -1
	}
	return v
}

// Maintain re-vincula periódicamente: si el adaptador se desenchufa y se
// vuelve a enchufar en el servidor, queda exportable sin intervención.
func (b *kernelBackend) Maintain(ctx context.Context, exported func(protocol.Device) bool) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	failed := map[string]bool{}
	for {
		devs, _ := b.List()
		for _, d := range devs {
			if !exported(d) {
				continue
			}
			if err := b.ensureBound(d.BusID); err != nil {
				if !failed[d.BusID] {
					b.log.Error("no se pudo vincular", "busid", d.BusID, "err", err)
					failed[d.BusID] = true
				}
			} else {
				delete(failed, d.BusID)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (b *kernelBackend) Open(busid string, force bool) (Session, error) {
	if err := b.ensureBound(busid); err != nil {
		return nil, err
	}
	st := usbipStatus(busid)
	if st == sdevUsed || st == sdevError {
		if !force {
			return nil, ErrBusy
		}
		// Sesión huérfana en el kernel (p.ej. tras reiniciar este servicio).
		b.log.Info("liberando sesión huérfana del kernel", "busid", busid)
		_ = writeSys(filepath.Join(sysDevices, busid, "usbip_sockfd"), "-1")
		deadline := time.Now().Add(10 * time.Second)
		for usbipStatus(busid) != sdevAvailable && time.Now().Before(deadline) {
			time.Sleep(200 * time.Millisecond)
		}
	}
	if s := usbipStatus(busid); s != sdevAvailable {
		return nil, fmt.Errorf("%w (usbip_status=%d)", ErrBusy, s)
	}
	d, err := readSysDevice(busid)
	if err != nil {
		return nil, err
	}
	return &kernelSession{b: b, dev: d}, nil
}

type kernelSession struct {
	b   *kernelBackend
	dev protocol.Device
}

func (s *kernelSession) Device() protocol.Device { return s.dev }
func (s *kernelSession) Abort()                  {}

func (s *kernelSession) Serve(ctx context.Context, conn *net.TCPConn) error {
	busid := s.dev.BusID
	f, err := conn.File() // descriptor duplicado; Fd() lo pone en modo bloqueante
	conn.Close()
	if err != nil {
		return err
	}
	fd := f.Fd()
	err = writeSys(filepath.Join(sysDevices, busid, "usbip_sockfd"), strconv.Itoa(int(fd)))
	f.Close() // el kernel conserva su propia referencia al socket
	if err != nil {
		return fmt.Errorf("entregando socket al kernel: %w", err)
	}

	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = writeSys(filepath.Join(sysDevices, busid, "usbip_sockfd"), "-1")
			return ctx.Err()
		case <-t.C:
		}
		switch st := usbipStatus(busid); st {
		case sdevUsed:
			continue
		case -1:
			return fmt.Errorf("dispositivo desconectado físicamente")
		case sdevError:
			_ = writeSys(filepath.Join(sysDevices, busid, "usbip_sockfd"), "-1")
			return fmt.Errorf("error en la sesión del kernel")
		default:
			return fmt.Errorf("conexión cerrada por el cliente o por la red")
		}
	}
}
