// Package client localiza servidores por nombre, importa los dispositivos
// configurados y los vuelve a importar automáticamente si la sesión se cae.
package client

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/brokensaw/usbnetshare/internal/config"
	"github.com/brokensaw/usbnetshare/internal/discovery"
	"github.com/brokensaw/usbnetshare/internal/netutil"
	"github.com/brokensaw/usbnetshare/internal/protocol"
	"github.com/brokensaw/usbnetshare/internal/registry"
	"github.com/brokensaw/usbnetshare/internal/usbids"
)

// Attacher conecta un dispositivo remoto al controlador USB virtual local
// (vhci-hcd en Linux, driver de usbip-win2 en Windows).
type Attacher interface {
	Prepare() error
	Attach(ctx context.Context, host string, port int, dev protocol.Device) (Attachment, error)
}

type Attachment interface {
	Alive() (bool, error)
	Detach() error
	String() string
}

type Target struct {
	Server   string // nombre lógico o "host:puerto" si es estático
	ServerID string // ID en el registro, si se conoce
	Host     string
	Port     int
	Device   protocol.Device
}

// Resolver da la lista de equipos del registro (usbnetshare-registry).
type Resolver interface {
	Peers(ctx context.Context) ([]registry.Peer, error)
}

// Estados de una regla, tal como se muestran en la GUI.
const (
	StateSearching     = "buscando"
	StateConnecting    = "conectando"
	StateConnected     = "conectado"
	StateRetrying      = "reintentando"
	StateWaitingDevice = "esperando dispositivo" // el servidor responde, pero el USB no está enchufado
)

// errDeviceMissing: algún servidor respondió, pero ninguno tiene el
// dispositivo (desenchufado, o aún sin conectar). No es un fallo de red.
var errDeviceMissing = errors.New("el dispositivo no está conectado en el servidor")

// RuleStatus describe en qué punto está una regla.
type RuleStatus struct {
	Rule      config.Match `json:"rule"`
	State     string       `json:"state"`
	Server    string       `json:"server,omitempty"`
	ServerID  string       `json:"server_id,omitempty"`
	Host      string       `json:"host,omitempty"`
	BusID     string       `json:"busid,omitempty"`
	VIDPID    string       `json:"vidpid,omitempty"`
	Product   string       `json:"product,omitempty"`
	Local     string       `json:"local,omitempty"` // puerto del controlador virtual
	Since     time.Time    `json:"since"`
	LastError string       `json:"last_error,omitempty"`
}

type rule struct {
	id     int
	m      config.Match
	cancel context.CancelFunc
	done   chan struct{}
	st     RuleStatus
	known  *candidate // última dirección que funcionó (solo la usa manage)
}

type Client struct {
	cfg *config.Client
	log *slog.Logger
	att Attacher
	// Resolver, si no es nil, localiza los servidores a través del registro.
	Resolver Resolver

	mu      sync.Mutex
	ctx     context.Context // contexto de Run; nil hasta que arranca
	rules   []*rule
	nextID  int
	claimed map[string]int // servidor/busid -> id de regla
}

// AttacherFactory crea el controlador virtual de la plataforma. Los tests lo
// sustituyen por uno simulado.
var AttacherFactory = newAttacher

func New(cfg *config.Client, log *slog.Logger) (*Client, error) {
	att, err := AttacherFactory(cfg, log)
	if err != nil {
		return nil, err
	}
	c := *cfg // copia: SetRules modifica Devices
	return &Client{cfg: &c, log: log, att: att, claimed: map[string]int{}}, nil
}

// Run prepara el controlador virtual y gestiona las reglas hasta que ctx
// termina. Al salir, todos los dispositivos quedan desconectados.
func (c *Client) Run(ctx context.Context) error {
	if err := c.att.Prepare(); err != nil {
		return err
	}
	c.log.Info("cliente iniciado", "servidor", nonEmpty(c.cfg.Server, "(cualquiera)"), "reglas", len(c.cfg.Devices))
	c.mu.Lock()
	c.ctx = ctx
	c.mu.Unlock()
	c.SetRules(c.cfg.Devices)
	<-ctx.Done()

	c.mu.Lock()
	rules := c.rules
	c.rules = nil
	c.mu.Unlock()
	for _, r := range rules {
		<-r.done
	}
	return nil
}

// SetRules sustituye las reglas sin tocar las que no cambian: así añadir o
// quitar un dispositivo desde la GUI no corta los que ya están conectados.
func (c *Client) SetRules(ms []config.Match) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cfg.Devices = append([]config.Match(nil), ms...)
	if c.ctx == nil {
		return // Run las arrancará
	}
	old := c.rules
	used := make([]bool, len(old))
	var next []*rule
	for _, m := range ms {
		var keep *rule
		for i, r := range old {
			if !used[i] && r.m == m {
				used[i], keep = true, r
				break
			}
		}
		if keep == nil {
			keep = c.startLocked(m)
		}
		next = append(next, keep)
	}
	for i, r := range old {
		if !used[i] {
			r.cancel()
		}
	}
	c.rules = next
}

func (c *Client) startLocked(m config.Match) *rule {
	ctx, cancel := context.WithCancel(c.ctx)
	r := &rule{id: c.nextID, m: m, cancel: cancel, done: make(chan struct{}),
		st: RuleStatus{Rule: m, State: StateSearching, Since: time.Now()}}
	c.nextID++
	go func() {
		defer close(r.done)
		c.manage(ctx, r)
	}()
	return r
}

// Status devuelve el estado de cada regla, en el orden de la configuración.
func (c *Client) Status() []RuleStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]RuleStatus, 0, len(c.rules))
	for _, r := range c.rules {
		out = append(out, r.st)
	}
	return out
}

func (c *Client) setStatus(r *rule, f func(*RuleStatus)) {
	c.mu.Lock()
	f(&r.st)
	c.mu.Unlock()
}

func nonEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func sleepCtx(ctx context.Context, d time.Duration) {
	// ±20 % de jitter para que varios clientes no se sincronicen.
	j := time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
	select {
	case <-ctx.Done():
	case <-time.After(j):
	}
}

// manage es el bucle de persistencia de una regla: localizar → importar →
// vigilar → (si cae) volver a empezar con backoff exponencial.
func (c *Client) manage(ctx context.Context, r *rule) {
	m := r.m
	log := c.log.With("regla", m.String())
	rc := c.cfg.Reconnect
	backoff := rc.MinBackoff
	fail := func(msg string, err error) {
		log.Warn(msg, "err", err, "reintento_en", backoff)
		c.setStatus(r, func(s *RuleStatus) {
			s.State, s.LastError, s.Since = StateRetrying, msg+": "+err.Error(), time.Now()
		})
		sleepCtx(ctx, backoff)
		backoff *= 2
		if backoff > rc.MaxBackoff {
			backoff = rc.MaxBackoff
		}
	}

	waiting := false
	for ctx.Err() == nil {
		t, err := c.locate(ctx, r)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, errDeviceMissing) {
				// Sondeo rápido y sin backoff: en cuanto se vuelva a enchufar
				// en el servidor, se reconecta.
				if !waiting {
					log.Info("esperando a que el dispositivo se conecte en el servidor", "detalle", err)
				}
				waiting = true
				backoff = rc.MinBackoff
				c.setStatus(r, func(s *RuleStatus) {
					if s.State != StateWaitingDevice {
						s.Since = time.Now()
					}
					s.State, s.LastError, s.Local = StateWaitingDevice, err.Error(), ""
				})
				sleepCtx(ctx, rc.WaitDevice)
				continue
			}
			waiting = false
			fail("dispositivo no localizado", err)
			continue
		}
		waiting = false
		key := nonEmpty(t.ServerID, t.Server) + "/" + t.Device.BusID
		c.setStatus(r, func(s *RuleStatus) {
			s.State, s.Server, s.ServerID, s.Host, s.BusID = StateConnecting, t.Server, t.ServerID, t.Host, t.Device.BusID
			s.VIDPID = fmt.Sprintf("%04x:%04x", t.Device.IDVendor, t.Device.IDProduct)
			s.Product, s.Since = t.Device.Product, time.Now()
		})
		a, err := c.att.Attach(ctx, t.Host, t.Port, t.Device)
		if err != nil {
			c.unclaim(key)
			if ctx.Err() != nil {
				return
			}
			fail("no se pudo importar", err)
			continue
		}
		log.Info("dispositivo conectado", "servidor", t.Server, "host", t.Host, "dispositivo", t.Device.String(), "puerto_local", a.String())
		c.setStatus(r, func(s *RuleStatus) {
			s.State, s.Local, s.LastError, s.Since = StateConnected, a.String(), "", time.Now()
		})
		backoff = rc.MinBackoff
		start := time.Now()

		reason := c.watch(ctx, a)
		_ = a.Detach()
		c.unclaim(key)
		c.setStatus(r, func(s *RuleStatus) { s.Local = "" })
		if ctx.Err() != nil {
			log.Info("dispositivo desconectado (regla eliminada o parada)")
			return
		}
		log.Warn("sesión perdida; reconectando", "motivo", reason, "duró", time.Since(start).Round(time.Second))
		c.setStatus(r, func(s *RuleStatus) {
			s.State, s.LastError, s.Since = StateRetrying, "sesión perdida: "+reason, time.Now()
		})
		sleepCtx(ctx, rc.MinBackoff)
	}
}

func (c *Client) watch(ctx context.Context, a Attachment) string {
	t := time.NewTicker(c.cfg.Reconnect.PollInterval)
	defer t.Stop()
	errCount := 0
	for {
		select {
		case <-ctx.Done():
			return "parada"
		case <-t.C:
		}
		alive, err := a.Alive()
		if err != nil {
			errCount++
			if errCount >= 5 {
				return "no se puede consultar el estado: " + err.Error()
			}
			continue
		}
		errCount = 0
		if !alive {
			return "el controlador virtual informa de desconexión"
		}
	}
}

func (c *Client) claim(key string, id int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if owner, ok := c.claimed[key]; ok && owner != id {
		return false
	}
	c.claimed[key] = id
	return true
}

func (c *Client) unclaim(key string) {
	c.mu.Lock()
	delete(c.claimed, key)
	c.mu.Unlock()
}

type candidate struct {
	name string
	id   string // ID en el registro ("" si es estático o por difusión)
	host string
	port int
}

// candidates devuelve las direcciones donde buscar el servidor de una regla:
//  1. el registro (por ID, o por nombre si la regla no tiene ID);
//  2. los servidores fijos (static_servers);
//  3. multicast/broadcast, solo si se activó explícitamente.
//
// No hay barrido de subredes (provocaba tormentas de ARP en redes de PLC).
func (c *Client) candidates(ctx context.Context, m config.Match) ([]candidate, error) {
	name := nonEmpty(m.Server, c.cfg.Server)
	var out []candidate
	var regErr error
	if c.Resolver != nil {
		peers, err := c.Resolver.Peers(ctx)
		regErr = err
		for _, p := range peers {
			if !p.Online || !p.Roles.Server {
				continue
			}
			if m.ServerID != "" {
				if p.ID != m.ServerID {
					continue
				}
			} else if name != "" && !strings.EqualFold(p.Name, name) {
				continue
			}
			for _, h := range p.Hosts() {
				out = append(out, candidate{name: p.Name, id: p.ID, host: h, port: p.Port})
			}
		}
	}
	for _, s := range c.cfg.StaticServers {
		if m.ServerID != "" || (name != "" && name != s && name != c.cfg.Server) {
			continue
		}
		host, ps, err := net.SplitHostPort(s)
		if err != nil {
			host, ps = s, fmt.Sprint(config.DefaultTCPPort)
		}
		var port int
		fmt.Sscan(ps, &port)
		out = append(out, candidate{name: s, host: host, port: port})
	}
	if c.cfg.Discovery.IsEnabled() {
		filter := name
		if m.ServerID != "" {
			filter = "" // se filtra por ID abajo
		}
		f, err := discovery.Discover(ctx, discovery.Options{
			Port: c.cfg.Discovery.Port, Group: c.cfg.Discovery.Group,
			Key: []byte(c.cfg.Discovery.Key), Name: filter,
			Timeout: c.cfg.Reconnect.DiscoveryTimeout,
		})
		if err != nil && ctx.Err() == nil {
			c.log.Debug("descubrimiento con errores", "err", err)
		}
		for _, s := range f {
			if m.ServerID != "" && s.ID != m.ServerID {
				continue
			}
			out = append(out, candidate{name: s.Name, id: s.ID, host: s.Host, port: s.Port})
		}
	}
	return out, regErr
}

// tryCandidate pide la lista de dispositivos a una dirección y elige el que
// cumple la regla. errDeviceMissing = el servidor responde pero no lo tiene.
func (c *Client) tryCandidate(ctx context.Context, id int, m config.Match, cd candidate) (*Target, error) {
	devs, err := Devlist(ctx, cd.host, cd.port, c.cfg.KeepAlive)
	if err != nil {
		return nil, err
	}
	for _, d := range devs {
		if !m.Matches(d.IDVendor, d.IDProduct, d.BusID) {
			continue
		}
		if !c.claim(nonEmpty(cd.id, cd.name)+"/"+d.BusID, id) {
			continue // ya lo gestiona otra regla
		}
		// OP_DEVLIST no lleva nombres: para el estado y el registro salen de usb.ids.
		d.Product, d.Manufacturer = usbids.Fill(d.IDVendor, d.IDProduct, d.Product, d.Manufacturer)
		return &Target{Server: cd.name, ServerID: cd.id, Host: cd.host, Port: cd.port, Device: d}, nil
	}
	return nil, fmt.Errorf("%w (%s)", errDeviceMissing, cd.name)
}

// locate busca el dispositivo de una regla. Si ya hay una dirección conocida
// se usa solo esa (TCP directo, sin registro ni difusión) mientras responda;
// únicamente si deja de responder se vuelve a preguntar al registro.
func (c *Client) locate(ctx context.Context, r *rule) (*Target, error) {
	m := r.m
	if r.known != nil {
		t, err := c.tryCandidate(ctx, r.id, m, *r.known)
		if err == nil || errors.Is(err, errDeviceMissing) {
			return t, err
		}
		c.log.Info("el servidor no responde en su última dirección; se consulta de nuevo",
			"servidor", r.known.name, "host", r.known.host, "err", err)
		r.known = nil
	}
	cands, regErr := c.candidates(ctx, m)
	if len(cands) == 0 {
		switch {
		case regErr != nil:
			return nil, fmt.Errorf("no se pudo consultar el registro: %w", regErr)
		case c.Resolver == nil && len(c.cfg.StaticServers) == 0 && !c.cfg.Discovery.IsEnabled():
			return nil, errors.New("no hay forma de localizar servidores: configura el servidor de identidades (Ajustes → Servidor de identidades) o añade el servidor por IP")
		case m.ServerID != "":
			return nil, fmt.Errorf("el servidor %s no está en línea en el registro", nonEmpty(m.Server, m.ServerID))
		}
		return nil, errors.New("ningún servidor responde con ese nombre")
	}
	var errs, missing []string
	for _, cd := range cands {
		t, err := c.tryCandidate(ctx, r.id, m, cd)
		if err == nil {
			known := cd
			r.known = &known
			return t, nil
		}
		if errors.Is(err, errDeviceMissing) {
			known := cd // el servidor responde: se recuerda para sondear solo a él
			r.known = &known
			missing = append(missing, cd.name)
			continue
		}
		errs = append(errs, fmt.Sprintf("%s (%s): %v", cd.name, cd.host, err))
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w (%s)", errDeviceMissing, strings.Join(missing, ", "))
	}
	return nil, errors.New(strings.Join(errs, "; "))
}

// Devlist ejecuta OP_REQ_DEVLIST contra un servidor.
func Devlist(ctx context.Context, host string, port int, ka config.KeepAlive) ([]protocol.Device, error) {
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, fmt.Sprint(port)))
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	netutil.TuneTCP(conn, ka)
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := protocol.WriteOpHeader(conn, protocol.OpReqDevlist, 0); err != nil {
		return nil, err
	}
	h, err := protocol.ReadOpHeader(conn)
	if err != nil {
		return nil, err
	}
	if h.Code != protocol.OpRepDevlist || h.Status != protocol.StOK {
		return nil, fmt.Errorf("respuesta inesperada (code=%#x status=%s)", h.Code, protocol.StatusText(h.Status))
	}
	return protocol.ReadDevlistReply(conn)
}

// Discover busca servidores por multicast/broadcast. Solo actúa si el
// descubrimiento por difusión está activado (por defecto no lo está).
func Discover(ctx context.Context, cfg *config.Client, log *slog.Logger) []discovery.Found {
	if !cfg.Discovery.IsEnabled() {
		return nil
	}
	f, err := discovery.Discover(ctx, discovery.Options{
		Port: cfg.Discovery.Port, Group: cfg.Discovery.Group,
		Key: []byte(cfg.Discovery.Key), Timeout: cfg.Reconnect.DiscoveryTimeout,
	})
	if err != nil && ctx.Err() == nil {
		log.Debug("descubrimiento con errores", "err", err)
	}
	return f
}

// ImportHandshake hace OP_REQ_IMPORT sobre una conexión nueva y devuelve
// el socket listo para la fase de URBs (usado por el attacher de Linux).
func ImportHandshake(ctx context.Context, host string, port int, busid string, ka config.KeepAlive) (*net.TCPConn, protocol.Device, error) {
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, fmt.Sprint(port)))
	if err != nil {
		return nil, protocol.Device{}, err
	}
	tc := conn.(*net.TCPConn)
	netutil.TuneTCP(tc, ka)
	_ = tc.SetDeadline(time.Now().Add(15 * time.Second))
	fail := func(e error) (*net.TCPConn, protocol.Device, error) { tc.Close(); return nil, protocol.Device{}, e }

	if err := protocol.WriteImportRequest(tc, busid); err != nil {
		return fail(err)
	}
	h, err := protocol.ReadOpHeader(tc)
	if err != nil {
		return fail(err)
	}
	if h.Code != protocol.OpRepImport {
		return fail(fmt.Errorf("respuesta inesperada %#x", h.Code))
	}
	if h.Status != protocol.StOK {
		return fail(fmt.Errorf("el servidor rechazó el import: %s", protocol.StatusText(h.Status)))
	}
	dev, err := protocol.ReadDevice(tc)
	if err != nil {
		return fail(err)
	}
	if dev.BusID != busid {
		return fail(fmt.Errorf("busid devuelto %q no coincide con %q", dev.BusID, busid))
	}
	_ = tc.SetDeadline(time.Time{})
	return tc, dev, nil
}
