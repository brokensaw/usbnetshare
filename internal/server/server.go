// Package server implementa un servidor USB/IP con descubrimiento automático,
// re-vinculación de dispositivos y recuperación de sesiones colgadas.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/brokensaw/usbnetshare/internal/config"
	"github.com/brokensaw/usbnetshare/internal/discovery"
	"github.com/brokensaw/usbnetshare/internal/netutil"
	"github.com/brokensaw/usbnetshare/internal/protocol"
	"github.com/brokensaw/usbnetshare/internal/usbdrv"
	"github.com/brokensaw/usbnetshare/internal/usbids"
)

var ErrBusy = errors.New("dispositivo ocupado")

// ErrUnplugged es el motivo de cierre de una sesión cuyo dispositivo se
// desenchufó físicamente del servidor.
var ErrUnplugged = errors.New("dispositivo desenchufado en el servidor")

// PresenceInterval es cada cuánto se comprueba que el dispositivo de una
// sesión sigue enchufado, y cada cuánto se registran los cambios de la lista.
var PresenceInterval = time.Second

// Backend abstrae cómo se accede físicamente al USB.
type Backend interface {
	Name() string
	List() ([]protocol.Device, error)
	// Open reserva el dispositivo para una sesión. Si force es true y el
	// dispositivo quedó ocupado por una sesión huérfana, la libera.
	Open(busid string, force bool) (Session, error)
	// Maintain corre en segundo plano (re-vinculación tras reconexión física).
	Maintain(ctx context.Context, exported func(protocol.Device) bool)
	Close() error
}

type Session interface {
	Device() protocol.Device
	// Serve toma posesión de conn y bloquea hasta que la sesión termina.
	Serve(ctx context.Context, conn *net.TCPConn) error
	// Abort libera la reserva si la sesión no llegó a iniciarse.
	Abort()
}

type backendFactory func(log *slog.Logger) (Backend, error)

var backends = map[string]backendFactory{}

func register(name string, f backendFactory) { backends[name] = f }

// RegisterBackend permite añadir backends desde fuera del paquete (tests).
func RegisterBackend(name string, f func(log *slog.Logger) (Backend, error)) { register(name, f) }

func newBackend(name string, log *slog.Logger) (Backend, error) {
	if name == "" {
		name = "libusb"
		if runtime.GOOS == "linux" {
			name = "kernel"
		}
	}
	f, ok := backends[name]
	if !ok {
		var avail []string
		for k := range backends {
			avail = append(avail, k)
		}
		sort.Strings(avail)
		return nil, fmt.Errorf("backend %q no disponible en este binario (disponibles: %v; libusb requiere compilar con -tags libusb)", name, avail)
	}
	return f(log)
}

type active struct {
	cancel context.CancelFunc
	done   chan struct{}
	remote string
	since  time.Time
}

type Server struct {
	cfg     *config.Server
	log     *slog.Logger
	backend Backend
	allow   []*net.IPNet
	id      string

	mu     sync.Mutex
	active map[string]*active
}

// useUsbDk: el backend libusb puede usar UsbDk en Windows (server.usbdk ≠ "off").
var useUsbDk atomic.Bool

func New(cfg *config.Server, log *slog.Logger) (*Server, error) {
	useUsbDk.Store(cfg.UsbDkEnabled())
	b, err := newBackend(cfg.Backend, log)
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, log: log, backend: b, active: map[string]*active{}}
	for _, c := range cfg.AllowCIDRs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			return nil, fmt.Errorf("allow_cidrs: %w", err)
		}
		s.allow = append(s.allow, n)
	}
	s.id = cfg.ID
	if s.id == "" { // configuración sin normalizar (tests)
		idb := make([]byte, 8)
		_, _ = rand.Read(idb)
		s.id = hex.EncodeToString(idb)
	}
	return s, nil
}

func (s *Server) exported(d protocol.Device) bool {
	if d.DeviceClass == 0x09 { // nunca exportar hubs
		return false
	}
	for _, m := range s.cfg.Export {
		if m.Matches(d.IDVendor, d.IDProduct, d.BusID) {
			return true
		}
	}
	return false
}

// knownNames recuerda el nombre propio de cada VID:PID: mientras UsbDk tiene
// capturado un dispositivo, Windows deja de verlo y no lo puede dar.
var knownNames sync.Map

// list pide los dispositivos al backend y completa los nombres que el
// backend no conoce: en Windows (libusb) el producto se toma de lo que el
// dispositivo informó a Windows; lo que siga faltando sale de usb.ids.
func (s *Server) list() ([]protocol.Device, error) {
	devs, err := s.backend.List()
	if err != nil {
		return nil, err
	}
	var drv map[string]usbdrv.Info
	for i := range devs {
		d := &devs[i]
		if d.Product == "" {
			if drv == nil {
				drv = usbdrv.Lookup()
			}
			d.Product = drv[usbdrv.Key(d.IDVendor, d.IDProduct)].Product
		}
		key := usbdrv.Key(d.IDVendor, d.IDProduct)
		if d.Product != "" {
			knownNames.Store(key, d.Product)
		} else if v, ok := knownNames.Load(key); ok {
			d.Product = v.(string)
		}
		d.Product, d.Manufacturer = usbids.Fill(d.IDVendor, d.IDProduct, d.Product, d.Manufacturer)
	}
	return devs, nil
}

// ExportedDevices lista los dispositivos presentes que cumplen las reglas.
func (s *Server) ExportedDevices() ([]protocol.Device, error) {
	all, err := s.list()
	if err != nil {
		return nil, err
	}
	var out []protocol.Device
	for _, d := range all {
		if s.exported(d) {
			out = append(out, d)
		}
	}
	return out, nil
}

// AllDevices lista todo lo que ve el backend (para el comando -list).
func (s *Server) AllDevices() ([]protocol.Device, error) { return s.list() }

func (s *Server) summaries() []discovery.DeviceSummary {
	devs, err := s.ExportedDevices()
	if err != nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]discovery.DeviceSummary, 0, len(devs))
	for _, d := range devs {
		_, busy := s.active[d.BusID]
		out = append(out, discovery.DeviceSummary{
			BusID: d.BusID, VID: fmt.Sprintf("%04x", d.IDVendor),
			PID: fmt.Sprintf("%04x", d.IDProduct), Product: d.Product, Manufacturer: d.Manufacturer, Busy: busy,
		})
	}
	return out
}

// SessionInfo describe un dispositivo exportado que está en uso.
type SessionInfo struct {
	BusID  string    `json:"busid"`
	Remote string    `json:"remote"`
	Since  time.Time `json:"since"`
}

// Sessions lista las sesiones activas, ordenadas por busid.
func (s *Server) Sessions() []SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SessionInfo, 0, len(s.active))
	for b, a := range s.active {
		out = append(out, SessionInfo{BusID: b, Remote: a.remote, Since: a.since})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BusID < out[j].BusID })
	return out
}

// Exported indica si un dispositivo cumple las reglas de exportación.
func (s *Server) Exported(d protocol.Device) bool { return s.exported(d) }

// BackendName devuelve el backend en uso ("libusb", "kernel (usbip-host)"…).
func (s *Server) BackendName() string { return s.backend.Name() }

func (s *Server) Close() error { return s.backend.Close() }

// Run escucha conexiones hasta que ctx termina.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return err
	}
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	var port int
	fmt.Sscan(portStr, &port)

	s.log.Info("servidor USB/IP iniciado", "nombre", s.cfg.Name, "escucha", ln.Addr().String(),
		"backend", s.backend.Name(), "takeover", s.cfg.TakeoverEnabled())

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); s.backend.Maintain(ctx, s.exported) }()

	if s.cfg.Discovery.IsEnabled() {
		r := &discovery.Responder{
			Name: s.cfg.Name, ID: s.id, TCPPort: port, UDPPort: s.cfg.Discovery.Port,
			Group: s.cfg.Discovery.Group, Key: []byte(s.cfg.Discovery.Key),
			Devices: s.summaries, Log: s.log,
		}
		wg.Add(1)
		go func() { defer wg.Done(); r.Run(ctx) }()
	}

	wg.Add(1)
	go func() { defer wg.Done(); s.logExportChanges(ctx) }()

	go func() { <-ctx.Done(); ln.Close() }()

	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			s.log.Warn("accept falló; reintentando", "err", err)
			time.Sleep(500 * time.Millisecond)
			continue
		}
		wg.Add(1)
		go func() { defer wg.Done(); s.handle(ctx, c.(*net.TCPConn)) }()
	}

	// Terminar sesiones activas.
	s.mu.Lock()
	for _, a := range s.active {
		a.cancel()
	}
	s.mu.Unlock()
	wg.Wait()
	return nil
}

func (s *Server) allowed(addr net.Addr) bool {
	if len(s.allow) == 0 {
		return true
	}
	ta, ok := addr.(*net.TCPAddr)
	if !ok {
		return false
	}
	for _, n := range s.allow {
		if n.Contains(ta.IP) {
			return true
		}
	}
	return false
}

func (s *Server) handle(ctx context.Context, c *net.TCPConn) {
	remote := c.RemoteAddr().String()
	log := s.log.With("cliente", remote)
	handedOff := false
	defer func() {
		if !handedOff {
			c.Close()
		}
	}()

	if !s.allowed(c.RemoteAddr()) {
		log.Warn("conexión rechazada (allow_cidrs)")
		return
	}
	netutil.TuneTCP(c, s.cfg.KeepAlive)
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))

	h, err := protocol.ReadOpHeader(c)
	if err != nil {
		log.Debug("cabecera ilegible", "err", err)
		return
	}
	if h.Version != protocol.Version {
		log.Debug("versión de protocolo distinta", "version", fmt.Sprintf("%#04x", h.Version))
	}

	switch h.Code {
	case protocol.OpReqDevlist:
		devs, err := s.ExportedDevices()
		if err != nil {
			log.Error("no se pudo listar dispositivos", "err", err)
		}
		_ = protocol.WriteDevlistReply(c, devs)

	case protocol.OpReqImport:
		busid, err := protocol.ReadImportRequest(c)
		if err != nil {
			return
		}
		handedOff = s.importDevice(ctx, c, busid, log.With("busid", busid))

	default:
		log.Warn("operación desconocida", "code", fmt.Sprintf("%#04x", h.Code))
	}
}

func (s *Server) findExported(busid string) (*protocol.Device, error) {
	devs, err := s.ExportedDevices()
	if err != nil {
		return nil, err
	}
	for i := range devs {
		if devs[i].BusID == busid {
			return &devs[i], nil
		}
	}
	return nil, nil
}

func (s *Server) importDevice(ctx context.Context, c *net.TCPConn, busid string, log *slog.Logger) bool {
	d, err := s.findExported(busid)
	if err != nil || d == nil {
		var avail []string
		if devs, err := s.ExportedDevices(); err == nil {
			for _, x := range devs {
				avail = append(avail, x.BusID)
			}
		}
		log.Warn("import de dispositivo no exportado o inexistente", "disponibles", avail)
		_ = protocol.WriteImportReply(c, protocol.StNoDev, nil)
		return false
	}

	// Takeover: si hay una sesión previa (típicamente la del mismo cliente que
	// perdió la red y aún no lo sabemos), se cierra para atender la nueva.
	s.mu.Lock()
	prev := s.active[busid]
	s.mu.Unlock()
	if prev != nil {
		if !s.cfg.TakeoverEnabled() {
			log.Info("rechazado: dispositivo en uso", "por", prev.remote)
			_ = protocol.WriteImportReply(c, protocol.StDevBusy, nil)
			return false
		}
		log.Info("takeover: cerrando sesión anterior", "anterior", prev.remote)
		prev.cancel()
		select {
		case <-prev.done:
		case <-time.After(10 * time.Second):
			log.Warn("la sesión anterior no terminó a tiempo")
			_ = protocol.WriteImportReply(c, protocol.StDevBusy, nil)
			return false
		}
	}

	sess, err := s.backend.Open(busid, s.cfg.TakeoverEnabled())
	if err != nil {
		st := protocol.StDevErr
		if errors.Is(err, ErrBusy) {
			st = protocol.StDevBusy
		}
		log.Error("no se pudo abrir el dispositivo", "err", err)
		_ = protocol.WriteImportReply(c, st, nil)
		return false
	}
	dev := sess.Device()
	if err := protocol.WriteImportReply(c, protocol.StOK, &dev); err != nil {
		log.Warn("fallo enviando respuesta de import", "err", err)
		sess.Abort()
		return false
	}
	_ = c.SetDeadline(time.Time{})

	sctx, cancel := context.WithCancel(ctx)
	a := &active{cancel: cancel, done: make(chan struct{}), remote: c.RemoteAddr().String(), since: time.Now()}
	s.mu.Lock()
	s.active[busid] = a
	s.mu.Unlock()

	log.Info("dispositivo exportado", "dispositivo", dev.String())
	var unplugged atomic.Bool
	go s.watchPresence(sctx, cancel, dev, &unplugged)
	err = sess.Serve(sctx, c)
	cancel()
	if unplugged.Load() {
		err = ErrUnplugged
	}

	s.mu.Lock()
	if s.active[busid] == a {
		delete(s.active, busid)
	}
	s.mu.Unlock()
	close(a.done)
	log.Info("sesión finalizada", "duración", time.Since(a.since).Round(time.Second), "motivo", errString(err))
	return true
}

// watchPresence cierra la sesión si el dispositivo deja de estar enchufado.
// Sin esto, con el backend libusb la sesión seguiría abierta respondiendo
// ENODEV y el cliente nunca sabría que tiene que reconectar.
func (s *Server) watchPresence(ctx context.Context, cancel context.CancelFunc, dev protocol.Device, unplugged *atomic.Bool) {
	t := time.NewTicker(PresenceInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		devs, err := s.backend.List()
		if err != nil {
			continue // fallo puntual al enumerar: no es un desenchufado
		}
		present := false
		for _, d := range devs {
			if d.BusID == dev.BusID && d.IDVendor == dev.IDVendor && d.IDProduct == dev.IDProduct {
				present = true
				break
			}
		}
		if !present {
			s.log.Warn("dispositivo desenchufado en el servidor: se cierra la sesión", "busid", dev.BusID,
				"dispositivo", dev.String())
			unplugged.Store(true)
			cancel()
			return
		}
	}
}

// logExportChanges registra cuándo aparece o desaparece un dispositivo
// compartible (enchufar/desenchufar en el servidor).
func (s *Server) logExportChanges(ctx context.Context) {
	known := map[string]string{}
	first := true
	t := time.NewTicker(2 * PresenceInterval)
	defer t.Stop()
	for {
		if devs, err := s.ExportedDevices(); err == nil {
			now := map[string]string{}
			for _, d := range devs {
				now[d.BusID] = d.String()
				if _, ok := known[d.BusID]; !ok && !first {
					s.log.Info("dispositivo compartible conectado", "dispositivo", d.String())
				}
			}
			for b, desc := range known {
				if _, ok := now[b]; !ok {
					s.log.Warn("dispositivo compartible desconectado", "dispositivo", desc)
				}
			}
			known, first = now, false
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func errString(err error) string {
	if err == nil {
		return "fin normal"
	}
	return err.Error()
}
