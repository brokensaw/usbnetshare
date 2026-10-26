package engine_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brokensaw/usbnetshare/internal/api"
	"github.com/brokensaw/usbnetshare/internal/client"
	"github.com/brokensaw/usbnetshare/internal/config"
	"github.com/brokensaw/usbnetshare/internal/discovery"
	"github.com/brokensaw/usbnetshare/internal/engine"
	"github.com/brokensaw/usbnetshare/internal/logx"
	"github.com/brokensaw/usbnetshare/internal/protocol"
	"github.com/brokensaw/usbnetshare/internal/registry"
	"github.com/brokensaw/usbnetshare/internal/server"
)

// ── backend de servidor simulado: un "PC Adapter" que mantiene la sesión ──

var fakeDev = protocol.Device{BusID: "1-1", BusNum: 1, DevNum: 3, Speed: protocol.SpeedFull,
	IDVendor: 0x0908, IDProduct: 0x0004, ConfigurationValue: 1, NumConfigurations: 1,
	Interfaces: []protocol.Interface{{Class: 0xff}}, Product: "PC Adapter (fake)"}

type fakeBackend struct{}

// unplugged simula desenchufar el dispositivo en el servidor.
var unplugged atomic.Bool

func (fakeBackend) Name() string { return "fake" }
func (fakeBackend) List() ([]protocol.Device, error) {
	if unplugged.Load() {
		return nil, nil
	}
	return []protocol.Device{fakeDev}, nil
}
func (fakeBackend) Maintain(ctx context.Context, _ func(protocol.Device) bool) { <-ctx.Done() }
func (fakeBackend) Close() error                                               { return nil }
func (fakeBackend) Open(string, bool) (server.Session, error)                  { return fakeSess{}, nil }

type fakeSess struct{}

func (fakeSess) Device() protocol.Device { return fakeDev }
func (fakeSess) Abort()                  {}
func (fakeSess) Serve(ctx context.Context, c *net.TCPConn) error {
	defer c.Close()
	go func() { <-ctx.Done(); c.Close() }()
	_, err := c.Read(make([]byte, 1))
	return err
}

// ── controlador virtual simulado: hace el import real y guarda el socket ──

var attaches atomic.Int32

type fakeAttacher struct{ ka config.KeepAlive }

func (fakeAttacher) Prepare() error { return nil }
func (a fakeAttacher) Attach(ctx context.Context, host string, port int, dev protocol.Device) (client.Attachment, error) {
	conn, _, err := client.ImportHandshake(ctx, host, port, dev.BusID, a.ka)
	if err != nil {
		return nil, err
	}
	attaches.Add(1)
	f := &fakeAtt{conn: conn}
	// Como el driver real: si el servidor cierra el TCP, el dispositivo
	// virtual desaparece.
	go func() {
		_, _ = conn.Read(make([]byte, 1))
		f.dead.Store(true)
	}()
	return f, nil
}

type fakeAtt struct {
	mu   sync.Mutex
	conn *net.TCPConn
	dead atomic.Bool
}

func (f *fakeAtt) String() string { return "fake" }
func (f *fakeAtt) Alive() (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.conn != nil && !f.dead.Load(), nil
}
func (f *fakeAtt) Detach() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.conn != nil {
		f.conn.Close()
		f.conn = nil
	}
	return nil
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timeout esperando: %s", what)
}

// startRegistry levanta un usbnetshare-registry de prueba y cuenta las consultas
// de la lista de equipos.
func startRegistry(t *testing.T) (url, token string, peersCalls *atomic.Int32) {
	t.Helper()
	dir := t.TempDir()
	st, err := registry.NewStore(dir, 45*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	token = "token-de-prueba-registro-123"
	rs := registry.NewServer(st, dir, token, false, "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := rs.Handler()
	peersCalls = &atomic.Int32{}
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/api/v1/peers" {
			peersCalls.Add(1)
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(hs.Close)
	return hs.URL, token, peersCalls
}

func runEngine(t *testing.T, cfg *config.App) (*engine.Engine, string) {
	t.Helper()
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), config.FileName)
	log, ring, _ := logx.New("", "debug", "")
	e := engine.New(path, "test", cfg, log, ring)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = e.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return e, path
}

// Dos equipos (uno servidor, otro cliente) se registran en usbnetshare-registry;
// el cliente encuentra el servidor por su ID, sin ninguna búsqueda por la red,
// y se controla a través de la API local como hace la GUI.
func TestEngineThroughAPI(t *testing.T) {
	server.RegisterBackend("fake", func(*slog.Logger) (server.Backend, error) { return fakeBackend{}, nil })
	client.AttacherFactory = func(c *config.Client, _ *slog.Logger) (client.Attacher, error) {
		return fakeAttacher{ka: c.KeepAlive}, nil
	}
	regURL, regToken, peersCalls := startRegistry(t)
	reg := config.Registry{URL: regURL, Token: regToken}
	// Garantía para redes industriales: con la difusión desactivada (valor por
	// defecto) no se hace ni una búsqueda UDP en todo el flujo.
	udpBefore := discovery.Sent.Load()
	defer func() {
		if n := discovery.Sent.Load() - udpBefore; n != 0 {
			t.Errorf("se hicieron %d búsquedas por difusión con la difusión desactivada", n)
		}
	}()

	// Equipo servidor.
	scfg := config.Default()
	scfg.ID = "srv0001"
	scfg.Registry = reg
	scfg.Server = config.ServerRole{Enabled: true, Server: config.Server{
		Name: "t-srv", Listen: "127.0.0.1:43340", Backend: "fake", Export: []config.Match{{VID: "0908"}}}}
	scfg.Client.Enabled = false
	se, _ := runEngine(t, scfg)

	// Equipo cliente.
	ccfg := config.Default()
	ccfg.ID = "cli0001"
	ccfg.Registry = reg
	ccfg.Client.Enabled = true
	ccfg.Client.Reconnect.MinBackoff = 200 * time.Millisecond
	ccfg.Client.Reconnect.PollInterval = 100 * time.Millisecond
	ccfg.Client.Reconnect.WaitDevice = 200 * time.Millisecond
	ce, path := runEngine(t, ccfg)

	hs := httptest.NewServer(api.Handler(ce))
	defer hs.Close()
	r := api.NewRemote(strings.TrimPrefix(hs.URL, "http://"))

	// Sin la cabecera X-USBNet la API se niega (protección frente a webs).
	if resp, err := http.Get(hs.URL + "/api/status"); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("sin cabecera debería dar 403: %v %v", err, resp)
	}
	// Probar conexión con el registro: token bueno y malo.
	if err := r.TestRegistry(regURL, regToken); err != nil {
		t.Fatalf("probar registro: %v", err)
	}
	if err := r.TestRegistry(regURL, "malo"); err == nil {
		t.Fatal("con token erróneo debería fallar")
	}

	waitFor(t, "ambos registrados", func() bool {
		s1, _ := se.Status()
		s2, _ := r.Status()
		return s1.Registry.Connected && s2.Registry.Connected && s1.Server.Running
	})
	// El servidor tarda un latido en publicar sus dispositivos: se fuerza.
	waitFor(t, "servidor visible en el registro con su USB", func() bool {
		found, err := r.Discover()
		return err == nil && len(found) == 1 && found[0].ID == "srv0001" && len(found[0].Devices) == 1
	})
	found, _ := r.Discover()
	if found[0].Name != "t-srv" || found[0].Host != "127.0.0.1" || found[0].Port != 43340 {
		t.Fatalf("discover: %+v", found[0])
	}

	// Conectar = añadir una regla por ID de servidor (lo que hace la GUI).
	c, _ := r.Config()
	c.Client.Devices = []config.Match{{VID: "0908", PID: "0004", Server: "t-srv", ServerID: "srv0001"}}
	if err := r.SaveConfig(c); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "regla conectada", func() bool {
		s, _ := r.Status()
		return len(s.Client.Rules) == 1 && s.Client.Rules[0].State == client.StateConnected
	})
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("la configuración no se guardó: %v", err)
	}
	waitFor(t, "sesión en el servidor", func() bool { s, _ := se.Status(); return len(s.Server.Sessions) == 1 })

	// Desenchufar: «esperando dispositivo». Mientras la IP conocida responde,
	// el cliente NO vuelve a preguntar al registro (solo TCP al servidor).
	server.PresenceInterval = 100 * time.Millisecond
	defer func() { server.PresenceInterval = time.Second; unplugged.Store(false) }()
	unplugged.Store(true)
	waitFor(t, "esperando dispositivo", func() bool {
		s, _ := r.Status()
		return len(s.Client.Rules) == 1 && s.Client.Rules[0].State == client.StateWaitingDevice
	})
	before := peersCalls.Load()
	time.Sleep(6 * time.Second) // más que la caché de 5 s del registro
	if after := peersCalls.Load(); after != before {
		t.Fatalf("en «esperando dispositivo» no debería consultar el registro: %d → %d consultas", before, after)
	}
	unplugged.Store(false)
	waitFor(t, "reconectado", func() bool {
		s, _ := r.Status()
		return len(s.Client.Rules) == 1 && s.Client.Rules[0].State == client.StateConnected
	})

	// Añadir otra regla no debe cortar la conexión existente.
	att := attaches.Load()
	c.Client.Devices = append(c.Client.Devices, config.Match{VID: "ffff"})
	if err := r.SaveConfig(c); err != nil {
		t.Fatal(err)
	}
	time.Sleep(700 * time.Millisecond)
	s, _ := r.Status()
	if len(s.Client.Rules) != 2 || s.Client.Rules[0].State != client.StateConnected || attaches.Load() != att {
		t.Fatalf("la primera regla no debería reconectarse: attaches %d→%d, %+v", att, attaches.Load(), s.Client.Rules)
	}

	// Quitar la regla libera la sesión del servidor.
	c.Client.Devices = c.Client.Devices[1:]
	if err := r.SaveConfig(c); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "sesión liberada", func() bool { s, _ := se.Status(); return len(s.Server.Sessions) == 0 })

	// El registro de log llega por la API.
	if ls, err := r.Logs(0); err != nil || len(ls) == 0 {
		t.Fatalf("logs: %v %d", err, len(ls))
	}
	// Configuración inválida: se rechaza.
	c.Client.Devices = []config.Match{{}}
	if err := r.SaveConfig(c); err == nil {
		t.Fatal("debería rechazar una regla vacía")
	}
}

// Un servidor fijo (static_servers) aparece en Discover aunque el
// descubrimiento UDP esté desactivado: es el caso de redes que filtran el
// broadcast o de un firewall que bloquea las respuestas.
func TestDiscoverStaticServer(t *testing.T) {
	server.RegisterBackend("fake", func(*slog.Logger) (server.Backend, error) { return fakeBackend{}, nil })
	off := false
	cfg := config.Default()
	cfg.Server = config.ServerRole{Enabled: true, Server: config.Server{
		Name: "fijo", Listen: "127.0.0.1:43350", Backend: "fake",
		Export: []config.Match{{VID: "0908"}}, Discovery: config.Discovery{Enabled: &off}}}
	cfg.Client.Enabled = false
	cfg.Client.Discovery.Enabled = &off
	cfg.Client.StaticServers = []string{"127.0.0.1:43350", "127.0.0.1:43359"} // el segundo no existe
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	log, ring, _ := logx.New("", "error", "")
	e := engine.New(filepath.Join(t.TempDir(), config.FileName), "test", cfg, log, ring)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = e.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	waitFor(t, "servidor en marcha", func() bool { s, _ := e.Status(); return s.Server.Running })

	found, err := e.Discover()
	if err != nil || len(found) != 2 {
		t.Fatalf("%v %+v", err, found)
	}
	ok, bad := found[0], found[1]
	if !ok.Static || ok.Name != "127.0.0.1:43350" || ok.Error != "" || len(ok.Devices) != 1 || ok.Devices[0].VID != "0908" {
		t.Fatalf("servidor fijo: %+v", ok)
	}
	if !bad.Static || bad.Error == "" {
		t.Fatalf("el servidor inexistente debería tener error: %+v", bad)
	}
}

// Desenchufar el dispositivo en el servidor cierra la sesión; el cliente
// pasa a "esperando dispositivo" y, al volver a enchufarlo, reconecta solo.
func TestUnplugAndReconnect(t *testing.T) {
	server.RegisterBackend("fake", func(*slog.Logger) (server.Backend, error) { return fakeBackend{}, nil })
	client.AttacherFactory = func(c *config.Client, _ *slog.Logger) (client.Attacher, error) {
		return fakeAttacher{ka: c.KeepAlive}, nil
	}
	server.PresenceInterval = 100 * time.Millisecond
	defer func() { server.PresenceInterval = time.Second; unplugged.Store(false) }()

	off := false
	cfg := config.Default()
	cfg.Server = config.ServerRole{Enabled: true, Server: config.Server{
		Name: "t-unplug", Listen: "127.0.0.1:43360", Backend: "fake",
		Export: []config.Match{{VID: "0908"}}, Discovery: config.Discovery{Enabled: &off}}}
	cfg.Client.Enabled = true
	cfg.Client.Discovery.Enabled = &off
	cfg.Client.StaticServers = []string{"127.0.0.1:43360"}
	cfg.Client.Devices = []config.Match{{VID: "0908", Server: "127.0.0.1:43360"}}
	cfg.Client.Reconnect.MinBackoff = 100 * time.Millisecond
	cfg.Client.Reconnect.PollInterval = 100 * time.Millisecond
	cfg.Client.Reconnect.WaitDevice = 200 * time.Millisecond
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	log, ring, _ := logx.New("", "debug", "")
	e := engine.New(filepath.Join(t.TempDir(), config.FileName), "test", cfg, log, ring)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = e.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	state := func() (string, int) {
		s, _ := e.Status()
		if len(s.Client.Rules) == 0 {
			return "", len(s.Server.Sessions)
		}
		return s.Client.Rules[0].State, len(s.Server.Sessions)
	}
	waitFor(t, "conectado", func() bool { st, n := state(); return st == client.StateConnected && n == 1 })

	unplugged.Store(true)
	waitFor(t, "esperando dispositivo y sesión cerrada", func() bool {
		st, n := state()
		return st == client.StateWaitingDevice && n == 0
	})

	unplugged.Store(false)
	waitFor(t, "reconectado solo", func() bool { st, n := state(); return st == client.StateConnected && n == 1 })
}
