package engine

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/brokensaw/usbnetshare/internal/client"
	"github.com/brokensaw/usbnetshare/internal/config"
	"github.com/brokensaw/usbnetshare/internal/registry"
)

// RegistryStatus es el estado de la conexión con usbnetshare-registry.
type RegistryStatus struct {
	Enabled   bool      `json:"enabled"`
	URL       string    `json:"url,omitempty"`
	Connected bool      `json:"connected"`
	Error     string    `json:"error,omitempty"`
	LastOK    time.Time `json:"last_ok"`
	ID        string    `json:"id"` // ID de este equipo
}

// peersCacheTTL limita las consultas al registro: varias reglas que buscan a
// la vez comparten la misma respuesta.
const peersCacheTTL = 5 * time.Second

type regState struct {
	mu        sync.Mutex
	url       string
	token     string
	cli       *registry.Client
	connected bool
	lastErr   string
	lastOK    time.Time
	peers     []registry.Peer
	peersErr  error
	peersAt   time.Time
	kick      chan struct{}
}

// regClient devuelve el cliente del registro para la configuración actual
// (nil si no hay registro configurado).
func (e *Engine) regClient() *registry.Client {
	e.mu.Lock()
	r := e.cfg.Registry
	e.mu.Unlock()
	e.reg.mu.Lock()
	defer e.reg.mu.Unlock()
	if !r.Enabled() {
		e.reg.cli, e.reg.url, e.reg.token = nil, "", ""
		return nil
	}
	if e.reg.cli == nil || e.reg.url != r.URL || e.reg.token != r.Token {
		e.reg.cli, e.reg.url, e.reg.token = registry.NewClient(r.URL, r.Token), r.URL, r.Token
		e.reg.peers, e.reg.peersAt = nil, time.Time{}
	}
	return e.reg.cli
}

// regResolver adapta el motor a client.Resolver.
type regResolver struct{ e *Engine }

func (r regResolver) Peers(ctx context.Context) ([]registry.Peer, error) { return r.e.peers(ctx) }

// peers consulta el registro con una caché corta.
func (e *Engine) peers(ctx context.Context) ([]registry.Peer, error) {
	c := e.regClient()
	if c == nil {
		return nil, nil
	}
	e.reg.mu.Lock()
	if time.Since(e.reg.peersAt) < peersCacheTTL {
		p, err := e.reg.peers, e.reg.peersErr
		e.reg.mu.Unlock()
		return p, err
	}
	e.reg.mu.Unlock()
	p, err := c.Peers(ctx)
	e.reg.mu.Lock()
	e.reg.peers, e.reg.peersErr, e.reg.peersAt = p, err, time.Now()
	e.reg.mu.Unlock()
	return p, err
}

func (e *Engine) registryStatus() RegistryStatus {
	e.mu.Lock()
	r, id := e.cfg.Registry, e.cfg.ID
	e.mu.Unlock()
	e.reg.mu.Lock()
	defer e.reg.mu.Unlock()
	return RegistryStatus{Enabled: r.Enabled(), URL: r.URL, Connected: r.Enabled() && e.reg.connected,
		Error: e.reg.lastErr, LastOK: e.reg.lastOK, ID: id}
}

// kickAgent hace que el agente envíe un latido ya (tras cambiar la configuración).
func (e *Engine) kickAgent() {
	select {
	case e.reg.kick <- struct{}{}:
	default:
	}
}

// runAgent envía el latido al registro mientras el motor corre. Un fallo del
// registro no afecta a las conexiones USB: solo se refleja en el estado.
func (e *Engine) runAgent(ctx context.Context) {
	backoff := 5 * time.Second
	for ctx.Err() == nil {
		wait := 5 * time.Second // sin registro configurado: mirar de vez en cuando
		if c := e.regClient(); c != nil {
			hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			iv, err := c.Heartbeat(hctx, e.heartbeat())
			cancel()
			e.reg.mu.Lock()
			if err != nil {
				if e.reg.connected || e.reg.lastErr != err.Error() {
					e.log.Warn("no se pudo enviar el latido al registro", "err", err)
				}
				e.reg.connected, e.reg.lastErr = false, err.Error()
				wait = backoff
				if backoff < 60*time.Second {
					backoff *= 2
				}
			} else {
				if !e.reg.connected {
					e.log.Info("conectado al registro", "url", e.reg.url)
				}
				e.reg.connected, e.reg.lastErr, e.reg.lastOK = true, "", time.Now()
				backoff = 5 * time.Second
				e.mu.Lock()
				wait = e.cfg.Registry.Interval
				e.mu.Unlock()
				if iv > 0 {
					wait = iv
				}
			}
			e.reg.mu.Unlock()
		} else {
			e.reg.mu.Lock()
			e.reg.connected, e.reg.lastErr = false, ""
			e.reg.mu.Unlock()
		}
		select {
		case <-ctx.Done():
		case <-e.reg.kick:
		case <-time.After(wait):
		}
	}
}

// heartbeat reúne el estado real de este equipo.
func (e *Engine) heartbeat() registry.Heartbeat {
	e.mu.Lock()
	cfg := e.cfg
	srv, cli := e.srv, e.cli
	srvRunning, _ := e.srvRole.get()
	cliRunning, _ := e.cliRole.get()
	e.mu.Unlock()

	hb := registry.Heartbeat{ID: cfg.ID, Name: cfg.Server.Name, Version: e.version, OS: runtime.GOOS,
		Roles: registry.Roles{Server: cfg.Server.Enabled && srvRunning, Client: cfg.Client.Enabled && cliRunning},
		Addrs: localAddrs(), Devices: []registry.Device{}, Conns: []registry.Conn{}}
	if _, p, err := net.SplitHostPort(cfg.Server.Listen); err == nil {
		hb.Port, _ = strconv.Atoi(p)
	}
	if srv != nil {
		busy := map[string]string{}
		for _, s := range srv.Sessions() {
			busy[s.BusID] = s.Remote
		}
		if devs, err := srv.ExportedDevices(); err == nil {
			for _, d := range devs {
				hb.Devices = append(hb.Devices, registry.Device{BusID: d.BusID,
					VID: fmt.Sprintf("%04x", d.IDVendor), PID: fmt.Sprintf("%04x", d.IDProduct),
					Product: d.Product, Manufacturer: d.Manufacturer, InUseBy: busy[d.BusID]})
			}
		}
	}
	if cli != nil {
		for _, st := range cli.Status() {
			dev := st.VIDPID
			if dev == "" {
				dev = st.Rule.String()
			}
			hb.Conns = append(hb.Conns, registry.Conn{
				ServerID:   nonEmpty(st.ServerID, st.Rule.ServerID),
				ServerName: nonEmpty(st.Server, st.Rule.Server),
				Device:     dev, Product: st.Product, State: st.State, Since: st.Since})
		}
	}
	return hb
}

// localAddrs: IPv4 propias (sin loopback ni link-local). Solo se informan al
// registro; USBnetShare no envía nada a esas redes.
func localAddrs() []string {
	var out []string
	ifs, _ := net.Interfaces()
	for _, ifi := range ifs {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifi.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLinkLocalUnicast() {
				out = append(out, ipn.IP.String())
			}
		}
	}
	return out
}

func nonEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// TestRegistry comprueba una URL y un token sin guardarlos (botón «Probar
// conexión» de la GUI).
func (e *Engine) TestRegistry(url, token string) error {
	a := config.Default()
	a.Registry = config.Registry{URL: url, Token: token}
	if err := a.Normalize(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	_, err := registry.NewClient(a.Registry.URL, token).Peers(ctx)
	return err
}

// remoteFromPeers convierte los servidores en línea del registro al formato
// de «Buscar servidores».
func remoteFromPeers(peers []registry.Peer, selfID string) []RemoteServer {
	var out []RemoteServer
	for _, p := range peers {
		if !p.Online || !p.Roles.Server || p.ID == selfID {
			continue
		}
		rs := RemoteServer{Name: p.Name, ID: p.ID, Host: p.ObservedIP, Port: p.Port, Devices: []RemoteDevice{}}
		for _, d := range p.Devices {
			rs.Devices = append(rs.Devices, RemoteDevice{BusID: d.BusID, VID: d.VID, PID: d.PID,
				Product: d.Product, Manufacturer: d.Manufacturer, Busy: d.InUseBy != ""})
		}
		out = append(out, rs)
	}
	return out
}

var _ client.Resolver = regResolver{}
