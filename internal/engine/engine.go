// Package engine reúne los roles de servidor y cliente en un único proceso y
// ofrece las operaciones que usa la interfaz gráfica (directamente o a
// través de la API local cuando USBnetShare corre como servicio).
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/brokensaw/usbnetshare/internal/client"
	"github.com/brokensaw/usbnetshare/internal/config"
	"github.com/brokensaw/usbnetshare/internal/logx"
	"github.com/brokensaw/usbnetshare/internal/server"
	"github.com/brokensaw/usbnetshare/internal/usbdrv"
	"github.com/brokensaw/usbnetshare/internal/usbids"
)

// Controller es lo que necesita la GUI. Lo implementan Engine (en el mismo
// proceso) y api.Remote (contra el servicio).
type Controller interface {
	Status() (Status, error)
	Config() (config.App, error)
	SaveConfig(config.App) error
	LocalDevices() ([]LocalDevice, error)
	Discover() ([]RemoteServer, error)
	Logs(after uint64) ([]logx.Line, error)
	TestRegistry(url, token string) error
}

type ServerStatus struct {
	Enabled  bool                 `json:"enabled"`
	Running  bool                 `json:"running"`
	Error    string               `json:"error,omitempty"`
	Name     string               `json:"name"`
	Listen   string               `json:"listen"`
	Backend  string               `json:"backend,omitempty"`
	Sessions []server.SessionInfo `json:"sessions"`
	// UsbDk (solo Windows): instalado en el equipo / en uso por el backend.
	UsbDkInstalled bool   `json:"usbdk_installed"`
	UsbDkActive    bool   `json:"usbdk_active"`
	UsbDkError     string `json:"usbdk_error,omitempty"` // instalado pero no se usa: motivo
}

type ClientStatus struct {
	Enabled bool                `json:"enabled"`
	Running bool                `json:"running"`
	Error   string              `json:"error,omitempty"`
	Rules   []client.RuleStatus `json:"rules"`
	Driver  string              `json:"driver"` // p.ej. "usbip-win2 0.9.8.1 · compatible"
}

type Status struct {
	Version    string         `json:"version"`
	ConfigPath string         `json:"config_path"`
	Server     ServerStatus   `json:"server"`
	Client     ClientStatus   `json:"client"`
	Registry   RegistryStatus `json:"registry"`
}

// LocalDevice es un USB de este equipo, visto por el rol servidor.
type LocalDevice struct {
	BusID        string `json:"busid"`
	VID          string `json:"vid"`
	PID          string `json:"pid"`
	Product      string `json:"product"`
	Manufacturer string `json:"manufacturer"`
	Exported     bool   `json:"exported"`
	InUseBy      string `json:"in_use_by,omitempty"`
	// Driver y Shareable solo se rellenan en Windows: sin UsbDk, libusb
	// únicamente puede abrir dispositivos con WinUSB (instalado con Zadig).
	Driver    string `json:"driver,omitempty"`
	Shareable bool   `json:"shareable"`
	// HID: teclado, ratón… (compartirlo lo deja sin uso en este equipo).
	HID bool `json:"hid,omitempty"`
	// Missing: regla de exportación cuyo dispositivo no está enchufado ahora.
	Missing bool `json:"missing,omitempty"`
}

// RemoteServer es un servidor encontrado en la red.
type RemoteServer struct {
	Name    string         `json:"name"`
	Host    string         `json:"host"`
	Port    int            `json:"port"`
	Devices []RemoteDevice `json:"devices"`
	ID      string         `json:"id,omitempty"`
	// Static indica que solo respondió por TCP (sin descubrimiento UDP):
	// Name es la entrada de static_servers y no se sabe si está en uso.
	Static bool `json:"static"`
	// StaticEntry es la entrada de static_servers que lo apunta (para poder
	// quitarla desde la GUI), aunque el servidor también respondiera por UDP.
	StaticEntry string `json:"static_entry,omitempty"`
	Error       string `json:"error,omitempty"` // el servidor fijo no respondió
}

type RemoteDevice struct {
	BusID        string `json:"busid"`
	VID          string `json:"vid"`
	PID          string `json:"pid"`
	Product      string `json:"product"`
	Manufacturer string `json:"manufacturer"`
	Busy         bool   `json:"busy"`
}

// role es un bucle que mantiene vivo un rol y lo reinicia si falla
// (p.ej. puerto ocupado o driver aún no cargado).
type role struct {
	cancel  context.CancelFunc
	done    chan struct{}
	mu      sync.Mutex
	running bool
	err     string
}

func (r *role) set(running bool, err error) {
	r.mu.Lock()
	r.running = running
	r.err = ""
	if err != nil {
		r.err = err.Error()
	}
	r.mu.Unlock()
}

func (r *role) get() (bool, string) {
	if r == nil {
		return false, ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running, r.err
}

func (r *role) stop() {
	if r != nil {
		r.cancel()
		<-r.done
	}
}

type Engine struct {
	path    string
	version string
	log     *slog.Logger
	ring    *logx.Ring

	applyMu   sync.Mutex // serializa arranque, parada y cambios de configuración
	mu        sync.Mutex // protege los campos de abajo
	cfg       *config.App
	ctx       context.Context
	srvRole   *role
	cliRole   *role
	srv       *server.Server // instancia viva del rol servidor (o nil)
	drvInfo   string         // caché de client.DriverInfo
	usbdkInst bool           // caché de usbdrv.UsbDkInstalled
	drvAt     time.Time
	cli       *client.Client // instancia viva del rol cliente (o nil)
	retryWait time.Duration

	reg regState // conexión con usbnetshare-registry (tiene su propio mutex)
}

func New(path, version string, cfg *config.App, log *slog.Logger, ring *logx.Ring) *Engine {
	e := &Engine{path: path, version: version, cfg: clone(cfg), log: log, ring: ring, retryWait: 5 * time.Second}
	e.reg.kick = make(chan struct{}, 1)
	return e
}

func clone(a *config.App) *config.App {
	b, _ := json.Marshal(a)
	var c config.App
	_ = json.Unmarshal(b, &c)
	return &c
}

// Run arranca los roles activados y bloquea hasta que ctx termina.
func (e *Engine) Run(ctx context.Context) error {
	e.applyMu.Lock()
	e.mu.Lock()
	e.ctx = ctx
	cfg := e.cfg
	e.mu.Unlock()
	e.apply(nil, cfg)
	e.applyMu.Unlock()
	for _, w := range cfg.Deprecated() {
		e.log.Warn(w)
	}

	agentDone := make(chan struct{})
	go func() { defer close(agentDone); e.runAgent(ctx) }()

	<-ctx.Done()
	<-agentDone

	e.applyMu.Lock()
	defer e.applyMu.Unlock()
	e.mu.Lock()
	s, c := e.srvRole, e.cliRole
	e.srvRole, e.cliRole = nil, nil
	e.mu.Unlock()
	s.stop()
	c.stop()
	return nil
}

// apply arranca, para o reconfigura los roles pasando de old a cfg
// (old nil = arranque). Se llama con applyMu tomado y e.mu libre: parar un
// rol espera a su goroutine, que también toma e.mu.
func (e *Engine) apply(old, cfg *config.App) {
	// Servidor: cualquier cambio en su sección lo reinicia.
	if old == nil || !reflect.DeepEqual(old.Server, cfg.Server) {
		e.mu.Lock()
		r := e.srvRole
		e.srvRole = nil
		e.mu.Unlock()
		r.stop()
		if cfg.Server.Enabled {
			nr := e.startServer(cfg.Server.Server)
			e.mu.Lock()
			e.srvRole = nr
			e.mu.Unlock()
		}
	}
	// Cliente: si solo cambian las reglas, se aplican en caliente para no
	// cortar los dispositivos que siguen conectados.
	if old == nil || !reflect.DeepEqual(old.Client, cfg.Client) {
		e.mu.Lock()
		cli := e.cli
		e.mu.Unlock()
		if old != nil && old.Client.Enabled && cfg.Client.Enabled && cli != nil {
			a, b := old.Client.Client, cfg.Client.Client
			a.Devices, b.Devices = nil, nil
			if reflect.DeepEqual(a, b) {
				cli.SetRules(cfg.Client.Devices)
				return
			}
		}
		e.mu.Lock()
		r := e.cliRole
		e.cliRole = nil
		e.mu.Unlock()
		r.stop()
		if cfg.Client.Enabled {
			nr := e.startClient(cfg.Client.Client)
			e.mu.Lock()
			e.cliRole = nr
			e.mu.Unlock()
		}
	}
}

func (e *Engine) loop(name string, run func(ctx context.Context, r *role) error) *role {
	e.mu.Lock()
	parent := e.ctx
	e.mu.Unlock()
	ctx, cancel := context.WithCancel(parent)
	r := &role{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(r.done)
		for ctx.Err() == nil {
			err := run(ctx, r)
			if ctx.Err() != nil {
				break
			}
			if err == nil {
				err = fmt.Errorf("terminó sin motivo")
			}
			r.set(false, err)
			e.log.Error("el rol "+name+" se detuvo; reintento en breve", "err", err, "espera", e.retryWait)
			select {
			case <-ctx.Done():
			case <-time.After(e.retryWait):
			}
		}
		r.set(false, nil)
	}()
	return r
}

func (e *Engine) startServer(cfg config.Server) *role {
	return e.loop("servidor", func(ctx context.Context, r *role) error {
		e.mu.Lock()
		cfg.ID = e.cfg.ID // el ID del equipo identifica también al servidor
		e.mu.Unlock()
		s, err := server.New(&cfg, e.log)
		if err != nil {
			return err
		}
		defer s.Close()
		e.mu.Lock()
		e.srv = s
		e.mu.Unlock()
		defer func() { e.mu.Lock(); e.srv = nil; e.mu.Unlock() }()
		r.set(true, nil)
		e.kickAgent() // el registro ve el rol nuevo sin esperar al siguiente latido
		return s.Run(ctx)
	})
}

func (e *Engine) startClient(cfg config.Client) *role {
	return e.loop("cliente", func(ctx context.Context, r *role) error {
		e.mu.Lock()
		cfg.Devices = e.cfg.Client.Devices // reglas cambiadas en caliente
		e.mu.Unlock()
		c, err := client.New(&cfg, e.log)
		if err != nil {
			return err
		}
		c.Resolver = regResolver{e} // sin registro configurado devuelve una lista vacía
		e.mu.Lock()
		e.cli = c
		e.mu.Unlock()
		defer func() { e.mu.Lock(); e.cli = nil; e.mu.Unlock() }()
		r.set(true, nil)
		e.kickAgent() // el registro ve el rol nuevo sin esperar al siguiente latido
		return c.Run(ctx)
	})
}

func (e *Engine) Status() (Status, error) {
	reg := e.registryStatus()
	e.mu.Lock()
	defer e.mu.Unlock()
	st := Status{Version: e.version, ConfigPath: e.path, Registry: reg}
	st.Server = ServerStatus{Enabled: e.cfg.Server.Enabled, Name: e.cfg.Server.Name,
		Listen: e.cfg.Server.Listen, Sessions: []server.SessionInfo{}}
	st.Server.Running, st.Server.Error = e.srvRole.get()
	if e.srv != nil {
		st.Server.Backend = e.srv.BackendName()
		st.Server.Sessions = e.srv.Sessions()
	}
	if time.Since(e.drvAt) > 10*time.Second {
		e.drvInfo, e.usbdkInst, e.drvAt = client.DriverInfo(), usbdrv.UsbDkInstalled(), time.Now()
	}
	st.Server.UsbDkInstalled, st.Server.UsbDkActive, st.Server.UsbDkError = e.usbdkInst, server.UsbDkActive(), server.UsbDkError()
	st.Client = ClientStatus{Enabled: e.cfg.Client.Enabled, Rules: []client.RuleStatus{}, Driver: e.drvInfo}
	st.Client.Running, st.Client.Error = e.cliRole.get()
	if e.cli != nil {
		st.Client.Rules = e.cli.Status()
	}
	return st, nil
}

func (e *Engine) Config() (config.App, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return *clone(e.cfg), nil
}

// SaveConfig valida, guarda en disco y aplica la configuración.
func (e *Engine) SaveConfig(a config.App) error {
	n := clone(&a)
	if err := n.Normalize(); err != nil {
		return err
	}
	e.applyMu.Lock()
	defer e.applyMu.Unlock()
	if err := n.Save(e.path); err != nil {
		return fmt.Errorf("no se pudo guardar %s: %w", e.path, err)
	}
	e.mu.Lock()
	old := e.cfg
	e.cfg = n
	ctx := e.ctx
	e.mu.Unlock()
	e.log.Info("configuración guardada", "fichero", e.path)
	if ctx != nil && ctx.Err() == nil {
		e.apply(old, n)
	}
	e.kickAgent()
	return nil
}

// Reload vuelve a leer la configuración del disco. Solo se usa con el motor
// parado (p.ej. tras desinstalar el servicio, que pudo modificarla).
func (e *Engine) Reload() error {
	a, err := config.Load(e.path)
	if err != nil {
		return err
	}
	e.mu.Lock()
	e.cfg = a
	e.mu.Unlock()
	return nil
}

// LocalDevices lista los USB de este equipo. Si el rol servidor no está en
// marcha, abre el backend solo para listar.
func (e *Engine) LocalDevices() ([]LocalDevice, error) {
	e.mu.Lock()
	s := e.srv
	cfg := e.cfg.Server.Server
	e.mu.Unlock()
	if s == nil {
		tmp, err := server.New(&cfg, e.log)
		if err != nil {
			return nil, err
		}
		defer tmp.Close()
		s = tmp
	}
	devs, err := s.AllDevices()
	if err != nil {
		return nil, err
	}
	busy := map[string]string{}
	for _, si := range s.Sessions() {
		busy[si.BusID] = si.Remote
	}
	drivers := usbdrv.Lookup() // nil fuera de Windows
	usbdk := server.UsbDkActive()
	out := []LocalDevice{}
	for _, d := range devs {
		if d.DeviceClass == 0x09 { // hubs: no se pueden compartir
			continue
		}
		ld := LocalDevice{
			BusID: d.BusID, VID: fmt.Sprintf("%04x", d.IDVendor), PID: fmt.Sprintf("%04x", d.IDProduct),
			Product: d.Product, Manufacturer: d.Manufacturer, Exported: s.Exported(d), InUseBy: busy[d.BusID], Shareable: true,
			HID: d.DeviceClass == 0x03,
		}
		for _, it := range d.Interfaces {
			ld.HID = ld.HID || it.Class == 0x03
		}
		if drivers != nil {
			if i, ok := drivers[usbdrv.Key(d.IDVendor, d.IDProduct)]; ok {
				// Con UsbDk el driver de Windows no importa: se muestra como información.
				ld.Driver, ld.Shareable = i.Driver, i.Shareable || usbdk
			}
		}
		out = append(out, ld)
	}
	// Reglas de exportación sin dispositivo presente (desenchufado): se
	// muestran para que se vea que está compartido y pendiente de volver.
	e.mu.Lock()
	export := e.cfg.Server.Export
	e.mu.Unlock()
	for _, m := range export {
		if m.VID == "" || m.PID == "" {
			continue // reglas genéricas (p.ej. todo Siemens): no son un dispositivo concreto
		}
		matched := false
		for _, d := range devs {
			if m.Matches(d.IDVendor, d.IDProduct, d.BusID) {
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		ld := LocalDevice{VID: m.VID, PID: m.PID, BusID: m.BusID, Exported: true, Missing: true, Shareable: true}
		ld.Product, ld.Manufacturer = usbids.FillHex(m.VID, m.PID, "", "")
		out = append(out, ld)
	}
	return out, nil
}

// Discover lista los servidores disponibles para «Buscar servidores»:
//  1. los registrados y en línea en usbnetshare-registry (con sus USB);
//  2. los servidores fijos (static_servers), consultados por TCP;
//  3. los que respondan por multicast/broadcast, solo si está activado.
//
// No hay barrido de red. Si el registro falla y no hay nada más, devuelve
// el error para que la GUI lo muestre.
func (e *Engine) Discover() ([]RemoteServer, error) {
	e.mu.Lock()
	cfg := e.cfg.Client.Client
	selfID := e.cfg.ID
	e.mu.Unlock()
	if cfg.Reconnect.DiscoveryTimeout < 2*time.Second {
		cfg.Reconnect.DiscoveryTimeout = 2 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	statics := make([]RemoteServer, len(cfg.StaticServers))
	var wg sync.WaitGroup
	for i, s := range cfg.StaticServers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statics[i] = e.queryStatic(ctx, s, cfg.KeepAlive)
		}()
	}

	out := []RemoteServer{}
	var regErr error
	if e.regClient() != nil {
		e.reg.mu.Lock()
		e.reg.peersAt = time.Time{} // búsqueda manual: siempre datos frescos
		e.reg.mu.Unlock()
		peers, err := e.peers(ctx)
		regErr = err
		out = append(out, remoteFromPeers(peers, selfID)...)
	}

	for _, f := range client.Discover(ctx, &cfg, e.log) {
		if f.ID == selfID {
			continue
		}
		rs := RemoteServer{Name: f.Name, ID: f.ID, Host: f.Host, Port: f.Port, Devices: []RemoteDevice{}}
		for _, d := range f.Devices {
			rd := RemoteDevice{BusID: d.BusID, VID: d.VID, PID: d.PID, Product: d.Product, Manufacturer: d.Manufacturer, Busy: d.Busy}
			// servidor de una versión anterior o dispositivo sin nombre propio
			rd.Product, rd.Manufacturer = usbids.FillHex(d.VID, d.PID, rd.Product, rd.Manufacturer)
			rs.Devices = append(rs.Devices, rd)
		}
		dup := false
		for _, o := range out {
			dup = dup || (o.ID != "" && o.ID == rs.ID)
		}
		if !dup {
			out = append(out, rs)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	wg.Wait()
	for _, st := range statics {
		if i, ok := st.foundAt(ctx, out); ok {
			out[i].StaticEntry = st.Name // ya está en la lista: se marca como añadido a mano
			continue
		}
		st.StaticEntry = st.Name
		out = append(out, st)
	}
	if len(out) == 0 && regErr != nil {
		return out, fmt.Errorf("no se pudo consultar el registro: %w", regErr)
	}
	return out, nil
}

func (e *Engine) queryStatic(ctx context.Context, s string, ka config.KeepAlive) RemoteServer {
	host, ps, err := net.SplitHostPort(s)
	if err != nil {
		host, ps = s, fmt.Sprint(config.DefaultTCPPort)
	}
	port, _ := strconv.Atoi(ps)
	rs := RemoteServer{Name: s, Host: host, Port: port, Static: true, Devices: []RemoteDevice{}}
	devs, err := client.Devlist(ctx, host, port, ka)
	if err != nil {
		rs.Error = err.Error()
		return rs
	}
	for _, d := range devs {
		// OP_DEVLIST no lleva nombres: salen de la tabla usb.ids.
		prod, mfr := usbids.Fill(d.IDVendor, d.IDProduct, d.Product, "")
		rs.Devices = append(rs.Devices, RemoteDevice{BusID: d.BusID,
			VID: fmt.Sprintf("%04x", d.IDVendor), PID: fmt.Sprintf("%04x", d.IDProduct),
			Product: prod, Manufacturer: mfr})
	}
	return rs
}

// foundAt indica si un servidor fijo ya está en la lista porque respondió
// al descubrimiento (comparando sus IP resueltas y el puerto).
func (rs RemoteServer) foundAt(ctx context.Context, out []RemoteServer) (int, bool) {
	ips, err := net.DefaultResolver.LookupHost(ctx, rs.Host)
	if err != nil {
		return 0, false
	}
	for i, f := range out {
		if f.Static || f.Port != rs.Port {
			continue
		}
		for _, ip := range ips {
			if ip == f.Host {
				return i, true
			}
		}
	}
	return 0, false
}

func (e *Engine) Logs(after uint64) ([]logx.Line, error) {
	if e.ring == nil {
		return []logx.Line{}, nil
	}
	return e.ring.Since(after), nil
}
