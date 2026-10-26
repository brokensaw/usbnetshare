//go:build libusb

package server

// Backend "libusb": servidor USB/IP en espacio de usuario. Es el que permite
// ejecutar el servidor en Windows: con UsbDk instalado abre cualquier
// dispositivo (ver usbdk_windows.go); sin él, solo los que tienen el driver
// WinUSB (instalable con Zadig). Soporta transferencias control, bulk e interrupt,
// que es lo que usan los adaptadores tipo Siemens PC Adapter USB.
// Las isócronas (audio/vídeo) no están soportadas.

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/gousb"

	"github.com/brokensaw/usbnetshare/internal/protocol"
	"github.com/brokensaw/usbnetshare/internal/usbdrv"
)

func init() { register("libusb", newLibusbBackend) }

// usbdkReappear es cuánto espera Open a que un dispositivo recién soltado por
// UsbDk vuelva a aparecer.
const usbdkReappear = 8 * time.Second

type libusbBackend struct {
	log   *slog.Logger
	ctx   *gousb.Context
	usbdk bool
	mu    sync.Mutex
	inUse map[string]bool
	last  []protocol.Device // última enumeración correcta (ver List)
}

func newLibusbBackend(log *slog.Logger) (b Backend, err error) {
	// gousb hace panic si libusb no arranca: se convierte en error para que
	// el motor lo muestre en vez de cerrar USBnetShare.
	defer func() {
		if r := recover(); r != nil {
			b, err = nil, fmt.Errorf("libusb no se pudo iniciar: %v", r)
		}
	}()
	ctx := gousb.NewContext()
	usbdk, recreate := switchToUsbDk(ctx, log)
	if recreate { // UsbDk no enumera: contexto nuevo, que usa WinUSB
		ctx.Close()
		ctx = gousb.NewContext()
	}
	return &libusbBackend{log: log, ctx: ctx, usbdk: usbdk, inUse: map[string]bool{}}, nil
}

func (b *libusbBackend) Name() string {
	if b.usbdk {
		return "libusb + UsbDk"
	}
	return "libusb (espacio de usuario)"
}
func (b *libusbBackend) Close() error { return b.ctx.Close() }

func (b *libusbBackend) Maintain(ctx context.Context, _ func(protocol.Device) bool) {
	<-ctx.Done() // los dispositivos se abren bajo demanda; nada que mantener
}

func busIDOf(d *gousb.DeviceDesc) string {
	if len(d.Path) == 0 {
		return fmt.Sprintf("%d-0.%d", d.Bus, d.Address)
	}
	parts := make([]string, len(d.Path))
	for i, p := range d.Path {
		parts[i] = fmt.Sprint(p)
	}
	return fmt.Sprintf("%d-%s", d.Bus, strings.Join(parts, "."))
}

// busIDs asigna el busid de cada dispositivo enumerado. Con UsbDk el bus es
// el FilterID de UsbDk y el puerto cambia según el dispositivo se enumere por
// la línea USB 2 o la USB 3 del mismo conector (el xHCI las numera aparte):
// un busid "bus-puerto" no es estable y el cliente pediría uno que ya no
// existe (ST_NODEV). Por eso con UsbDk se usa "dk-vvvv-pppp", con "-2", "-3"…
// si hay varios dispositivos iguales (ordenados por bus y puerto).
func (b *libusbBackend) busIDs(descs []*gousb.DeviceDesc) []string {
	ids := make([]string, len(descs))
	if !b.usbdk {
		for i, d := range descs {
			ids[i] = busIDOf(d)
		}
		return ids
	}
	order := make([]int, len(descs))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(x, y int) bool {
		a, c := descs[order[x]], descs[order[y]]
		if a.Bus != c.Bus {
			return a.Bus < c.Bus
		}
		return a.Port < c.Port
	})
	total, seen := map[[2]uint16]int{}, map[[2]uint16]int{}
	for _, d := range descs {
		total[[2]uint16{uint16(d.Vendor), uint16(d.Product)}]++
	}
	for _, i := range order {
		d := descs[i]
		k := [2]uint16{uint16(d.Vendor), uint16(d.Product)}
		seen[k]++
		ids[i] = fmt.Sprintf("dk-%04x-%04x", k[0], k[1])
		if total[k] > 1 {
			ids[i] += fmt.Sprintf("-%d", seen[k])
		}
	}
	return ids
}

// descs enumera los dispositivos sin abrirlos.
func (b *libusbBackend) descs() ([]*gousb.DeviceDesc, error) {
	var out []*gousb.DeviceDesc
	_, err := b.ctx.OpenDevices(func(d *gousb.DeviceDesc) bool {
		out = append(out, d)
		return false
	})
	return out, err
}

func speedOf(s gousb.Speed) uint32 {
	switch s {
	case gousb.SpeedLow:
		return protocol.SpeedLow
	case gousb.SpeedFull:
		return protocol.SpeedFull
	case gousb.SpeedHigh:
		return protocol.SpeedHigh
	case gousb.SpeedSuper:
		return protocol.SpeedSuper
	}
	return protocol.SpeedUnknown
}

func deviceFromDesc(d *gousb.DeviceDesc, cfgNum int) protocol.Device {
	pd := protocol.Device{
		Path:              fmt.Sprintf("libusb/%d/%d", d.Bus, d.Address),
		BusID:             busIDOf(d),
		BusNum:            uint32(d.Bus),
		DevNum:            uint32(d.Address),
		Speed:             speedOf(d.Speed),
		IDVendor:          uint16(d.Vendor),
		IDProduct:         uint16(d.Product),
		BcdDevice:         uint16(d.Device),
		DeviceClass:       uint8(d.Class),
		DeviceSubClass:    uint8(d.SubClass),
		DeviceProtocol:    uint8(d.Protocol),
		NumConfigurations: uint8(len(d.Configs)),
	}
	if cfgNum == 0 {
		var nums []int
		for n := range d.Configs {
			nums = append(nums, n)
		}
		sort.Ints(nums)
		if len(nums) > 0 {
			cfgNum = nums[0]
		}
	}
	if c, ok := d.Configs[cfgNum]; ok {
		pd.ConfigurationValue = uint8(cfgNum)
		for _, it := range c.Interfaces {
			if len(it.AltSettings) == 0 {
				continue
			}
			a := it.AltSettings[0]
			pd.Interfaces = append(pd.Interfaces, protocol.Interface{
				Class: uint8(a.Class), SubClass: uint8(a.SubClass), Protocol: uint8(a.Protocol)})
		}
		pd.NumInterfaces = uint8(len(pd.Interfaces))
	}
	return pd
}

func (b *libusbBackend) List() ([]protocol.Device, error) {
	descs, err := b.descs()
	if err != nil && b.usbdk {
		// UsbDk_GetDevicesList falla (LIBUSB_ERROR_OTHER) mientras ciertos
		// dispositivos están capturados (p.ej. el adaptador de red Linksys
		// USB3GIGV1). Responder "no existe" a un import o un takeover crearía
		// un bucle de reconexiones: se usa la última lista buena.
		b.mu.Lock()
		last := b.last
		b.mu.Unlock()
		if last != nil {
			return append([]protocol.Device(nil), last...), nil
		}
	}
	ids := b.busIDs(descs)
	var out []protocol.Device
	for i, d := range descs {
		if d.Class != gousb.ClassHub {
			pd := deviceFromDesc(d, 0)
			pd.BusID = ids[i]
			out = append(out, pd)
		}
	}
	if err == nil {
		b.mu.Lock()
		b.last = out
		b.mu.Unlock()
	}
	return out, err
}

func (b *libusbBackend) Open(busid string, force bool) (Session, error) {
	b.mu.Lock()
	if b.inUse[busid] {
		b.mu.Unlock()
		return nil, ErrBusy
	}
	b.inUse[busid] = true
	b.mu.Unlock()

	release := func() { b.mu.Lock(); delete(b.inUse, busid); b.mu.Unlock() }

	var vid, pid uint16
	open := func() ([]*gousb.Device, error) {
		descs, err := b.descs()
		var target *gousb.DeviceDesc
		for i, id := range b.busIDs(descs) {
			if id == busid {
				target = descs[i]
			}
		}
		if target == nil {
			return nil, err
		}
		vid, pid = uint16(target.Vendor), uint16(target.Product)
		return b.ctx.OpenDevices(func(d *gousb.DeviceDesc) bool {
			return d.Bus == target.Bus && d.Address == target.Address &&
				d.Vendor == target.Vendor && d.Product == target.Product
		})
	}
	devs, err := open()
	// Con UsbDk, al cerrar una sesión el dispositivo vuelve a Windows y tarda
	// unos segundos en reaparecer: un import inmediato (takeover, reconexión
	// del cliente) lo espera en vez de fallar con "no encontrado".
	for deadline := time.Now().Add(usbdkReappear); b.usbdk && len(devs) == 0 && time.Now().Before(deadline); {
		time.Sleep(250 * time.Millisecond)
		devs, err = open()
	}
	if err != nil && len(devs) == 0 {
		release()
		if b.usbdk {
			return nil, fmt.Errorf("abriendo %s con UsbDk: %w (¿lo tiene abierto otro programa?)", busid, err)
		}
		if h := usbdrv.Hint(vid, pid); h != "" {
			return nil, fmt.Errorf("abriendo %s: %w: %s", busid, err, h)
		}
		return nil, fmt.Errorf("abriendo %s: %w (¿driver WinUSB instalado con Zadig? ¿permisos udev?)", busid, err)
	}
	if len(devs) == 0 {
		release()
		return nil, fmt.Errorf("dispositivo %s no encontrado", busid)
	}
	for _, extra := range devs[1:] {
		extra.Close()
	}
	dev := devs[0]
	_ = dev.SetAutoDetach(true) // no soportado en Windows: se ignora
	dev.ControlTimeout = 10 * time.Second

	cfgNum, err := dev.ActiveConfigNum()
	if err != nil || cfgNum == 0 {
		cfgNum = int(deviceFromDesc(dev.Desc, 0).ConfigurationValue)
	}
	s := &libusbSession{
		b: b, busid: busid, dev: dev,
		intfs: map[int]*gousb.Interface{}, inEP: map[int]*gousb.InEndpoint{},
		outEP: map[int]*gousb.OutEndpoint{}, epLock: map[uint32]*sync.Mutex{},
		pending: map[uint32]context.CancelFunc{}, unlinked: map[uint32]bool{},
		release: release,
	}
	if err := s.setConfig(cfgNum); err != nil {
		s.cleanup()
		return nil, err
	}
	s.info = deviceFromDesc(dev.Desc, cfgNum)
	s.info.BusID = busid
	return s, nil
}

type libusbSession struct {
	b       *libusbBackend
	busid   string
	dev     *gousb.Device
	info    protocol.Device
	release func()

	mu       sync.Mutex
	cfg      *gousb.Config
	intfs    map[int]*gousb.Interface
	inEP     map[int]*gousb.InEndpoint
	outEP    map[int]*gousb.OutEndpoint
	epLock   map[uint32]*sync.Mutex
	pending  map[uint32]context.CancelFunc
	unlinked map[uint32]bool

	wmu  sync.Mutex
	conn net.Conn
}

func (s *libusbSession) Device() protocol.Device { return s.info }
func (s *libusbSession) Abort()                  { s.cleanup() }

// Las tres funciones siguientes requieren s.mu tomado (o uso exclusivo).

func (s *libusbSession) closeInterfacesLocked() {
	for n, it := range s.intfs {
		it.Close()
		delete(s.intfs, n)
	}
	s.inEP = map[int]*gousb.InEndpoint{}
	s.outEP = map[int]*gousb.OutEndpoint{}
}

func (s *libusbSession) openEndpointsLocked(it *gousb.Interface) {
	for _, ep := range it.Setting.Endpoints {
		if ep.TransferType == gousb.TransferTypeIsochronous {
			continue
		}
		if ep.Direction == gousb.EndpointDirectionIn {
			if e, err := it.InEndpoint(ep.Number); err == nil {
				s.inEP[ep.Number] = e
			}
		} else {
			if e, err := it.OutEndpoint(ep.Number); err == nil {
				s.outEP[ep.Number] = e
			}
		}
	}
}

func (s *libusbSession) setConfig(n int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeInterfacesLocked()
	if s.cfg != nil {
		s.cfg.Close()
		s.cfg = nil
	}
	cfg, err := s.dev.Config(n)
	if err != nil {
		return fmt.Errorf("configuración %d: %w", n, err)
	}
	s.cfg = cfg
	for _, id := range cfg.Desc.Interfaces {
		it, err := cfg.Interface(id.Number, 0)
		if err != nil {
			s.b.log.Warn("no se pudo reclamar la interfaz", "busid", s.busid, "if", id.Number, "err", err)
			continue
		}
		s.intfs[id.Number] = it
		s.openEndpointsLocked(it)
	}
	return nil
}

func (s *libusbSession) setInterface(num, alt int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg == nil {
		return errors.New("sin configuración activa")
	}
	if old, ok := s.intfs[num]; ok {
		for _, ep := range old.Setting.Endpoints {
			if ep.Direction == gousb.EndpointDirectionIn {
				delete(s.inEP, ep.Number)
			} else {
				delete(s.outEP, ep.Number)
			}
		}
		old.Close()
		delete(s.intfs, num)
	}
	it, err := s.cfg.Interface(num, alt)
	if err != nil {
		return err
	}
	s.intfs[num] = it
	s.openEndpointsLocked(it)
	return nil
}

func (s *libusbSession) cleanup() {
	s.mu.Lock()
	s.closeInterfacesLocked()
	if s.cfg != nil {
		s.cfg.Close()
		s.cfg = nil
	}
	s.mu.Unlock()
	// Reset: el siguiente cliente encontrará el dispositivo en estado limpio.
	// Con UsbDk no: al cerrar, UsbDk devuelve el dispositivo a Windows, que ya
	// lo reinicia, y un reset previo hace que los USB 3 se re-enumeren en otro
	// puerto (cambia su busid y el siguiente import no lo encuentra).
	if !s.b.usbdk {
		_ = s.dev.Reset()
	}
	s.dev.Close()
	s.release()
}

func (s *libusbSession) send(b []byte) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_ = s.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := s.conn.Write(b); err != nil {
		s.conn.Close() // provoca la salida del bucle de lectura
	}
}

func (s *libusbSession) lockFor(dir, ep uint32) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := dir<<8 | ep
	l, ok := s.epLock[k]
	if !ok {
		l = &sync.Mutex{}
		s.epLock[k] = l
	}
	return l
}

func (s *libusbSession) Serve(ctx context.Context, conn *net.TCPConn) error {
	s.conn = conn
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() {
		cancel()
		conn.Close()
		wg.Wait()
		s.cleanup()
	}()
	go func() { <-ctx.Done(); conn.Close() }()

	r := bufio.NewReaderSize(conn, 64*1024)
	for {
		u, err := protocol.ReadURB(r)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		seq := u.Header.SeqNum
		switch u.Header.Command {
		case protocol.CmdSubmit:
			uctx, ucancel := context.WithCancel(ctx)
			s.mu.Lock()
			s.pending[seq] = ucancel
			s.mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer ucancel()
				s.handleSubmit(uctx, u)
			}()
		case protocol.CmdUnlink:
			target := u.Unlink.UnlinkSeqNum
			s.mu.Lock()
			c, ok := s.pending[target]
			if ok {
				s.unlinked[target] = true
				delete(s.pending, target)
			}
			s.mu.Unlock()
			status := int32(0) // ya había terminado
			if ok {
				c()
				status = protocol.ECONNRESET
			}
			s.send(protocol.EncodeRetUnlink(seq, status))
		}
	}
}

func errnoOf(err error) int32 {
	var ts gousb.TransferStatus
	if errors.As(err, &ts) {
		switch ts {
		case gousb.TransferStall:
			return protocol.EPIPE
		case gousb.TransferCancelled:
			return protocol.ECONNRESET
		case gousb.TransferNoDevice:
			return protocol.ENODEV
		case gousb.TransferTimedOut:
			return protocol.ETIMEDOUT
		case gousb.TransferOverflow:
			return protocol.EOVERFLOW
		}
		return protocol.EPROTO
	}
	var ue gousb.Error
	if errors.As(err, &ue) {
		switch ue {
		case gousb.ErrorPipe:
			return protocol.EPIPE
		case gousb.ErrorNoDevice:
			return protocol.ENODEV
		case gousb.ErrorTimeout:
			return protocol.ETIMEDOUT
		case gousb.ErrorOverflow:
			return protocol.EOVERFLOW
		case gousb.ErrorInterrupted:
			return protocol.ECONNRESET
		}
	}
	if errors.Is(err, context.Canceled) {
		return protocol.ECONNRESET
	}
	return protocol.EPROTO
}

func (s *libusbSession) handleSubmit(ctx context.Context, u *protocol.URB) {
	h := u.Header
	lk := s.lockFor(h.Direction, h.EP)
	lk.Lock() // mantiene el orden de las URBs por endpoint
	var (
		status int32
		data   []byte
		actual int
	)
	switch {
	case u.IsISO:
		status = protocol.EOPNOTSUPP
	case h.EP == 0:
		data, actual, status = s.control(u)
	case h.Direction == protocol.DirIn:
		s.mu.Lock()
		ep := s.inEP[int(h.EP)]
		s.mu.Unlock()
		if ep == nil {
			status = protocol.EPIPE
			break
		}
		buf := make([]byte, u.Submit.BufferLength)
		n, err := ep.ReadContext(ctx, buf)
		if err != nil && n == 0 {
			status = errnoOf(err)
		}
		data, actual = buf[:n], n
	default:
		s.mu.Lock()
		ep := s.outEP[int(h.EP)]
		s.mu.Unlock()
		if ep == nil {
			status = protocol.EPIPE
			break
		}
		n, err := ep.WriteContext(ctx, u.Data)
		if err != nil {
			status = errnoOf(err)
		}
		actual = n
	}
	lk.Unlock()

	s.mu.Lock()
	if s.unlinked[h.SeqNum] { // el cliente lo canceló: no se responde
		delete(s.unlinked, h.SeqNum)
		s.mu.Unlock()
		return
	}
	delete(s.pending, h.SeqNum)
	s.mu.Unlock()

	if h.Direction == protocol.DirIn {
		s.send(protocol.EncodeRetSubmit(h.SeqNum, status, data[:actual]))
	} else {
		s.send(protocol.EncodeRetSubmitOut(h.SeqNum, status, actual))
	}
}

// control ejecuta una transferencia por el endpoint 0. Las peticiones
// estándar que cambian el estado del dispositivo se traducen a llamadas
// libusb para mantener coherente el estado local (igual que hace el kernel).
func (s *libusbSession) control(u *protocol.URB) ([]byte, int, int32) {
	st := u.Submit.Setup
	rType, req := st[0], st[1]
	val := binary.LittleEndian.Uint16(st[2:4])
	idx := binary.LittleEndian.Uint16(st[4:6])
	length := binary.LittleEndian.Uint16(st[6:8])

	switch {
	case rType == 0x00 && req == 0x09: // SET_CONFIGURATION
		if err := s.setConfig(int(val)); err != nil {
			s.b.log.Warn("SET_CONFIGURATION falló", "err", err)
			return nil, 0, protocol.EPIPE
		}
		return nil, 0, 0
	case rType == 0x01 && req == 0x0B: // SET_INTERFACE
		if err := s.setInterface(int(idx), int(val)); err != nil {
			s.b.log.Warn("SET_INTERFACE falló", "err", err)
			return nil, 0, protocol.EPIPE
		}
		return nil, 0, 0
	}

	if rType&0x80 != 0 { // IN
		buf := make([]byte, length)
		n, err := s.dev.Control(rType, req, val, idx, buf)
		if err != nil {
			return nil, 0, errnoOf(err)
		}
		return buf[:n], n, 0
	}
	n, err := s.dev.Control(rType, req, val, idx, u.Data)
	if err != nil {
		// CLEAR_FEATURE(ENDPOINT_HALT) puede ser rechazado por WinUSB; el
		// estado del endpoint se reinicia igualmente en la siguiente transferencia.
		if rType == 0x02 && req == 0x01 && val == 0 {
			return nil, 0, 0
		}
		return nil, 0, errnoOf(err)
	}
	return nil, n, 0
}
