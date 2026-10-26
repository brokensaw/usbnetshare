package registry

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/dop251/goja/parser"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestTokenGeneratedOnceAndPersisted(t *testing.T) {
	dir := t.TempDir()
	t1, fromEnv, created, err := LoadOrCreateToken(dir, "")
	if err != nil || fromEnv || !created || len(t1) < 40 {
		t.Fatalf("%q %v %v %v", t1, fromEnv, created, err)
	}
	// Al recrear el contenedor (mismo volumen) se reutiliza.
	t2, _, created, err := LoadOrCreateToken(dir, "")
	if err != nil || created || t2 != t1 {
		t.Fatalf("debería reutilizar el token: %q -> %q (%v)", t1, t2, err)
	}
}

func TestTokenFromEnvWinsAndIsPersisted(t *testing.T) {
	dir := t.TempDir()
	_, _, _, _ = LoadOrCreateToken(dir, "")
	tok, fromEnv, _, err := LoadOrCreateToken(dir, "token-de-la-variable-1234")
	if err != nil || !fromEnv || tok != "token-de-la-variable-1234" {
		t.Fatalf("%q %v %v", tok, fromEnv, err)
	}
	// Si luego se quita la variable, sigue el mismo (guardado en el volumen).
	tok2, fromEnv, _, _ := LoadOrCreateToken(dir, "")
	if tok2 != tok || fromEnv {
		t.Fatalf("sin variable debería seguir %q, es %q", tok, tok2)
	}
	if _, _, _, err := LoadOrCreateToken(dir, "corto"); err == nil {
		t.Fatal("debería rechazar un token corto")
	}
}

func newTestServer(t *testing.T, fromEnv bool) (*Server, *httptest.Server, *Store, func(time.Duration)) {
	t.Helper()
	dir := t.TempDir()
	st, err := NewStore(dir, 45*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC)
	st.now = func() time.Time { return now }
	advance := func(d time.Duration) { now = now.Add(d) }
	s := NewServer(st, dir, "secreto-de-prueba-123456", fromEnv, "test", quiet)
	hs := httptest.NewServer(s.Handler())
	t.Cleanup(hs.Close)
	return s, hs, st, advance
}

func TestAuthRequired(t *testing.T) {
	_, hs, _, _ := newTestServer(t, false)
	for _, auth := range []string{"", "Bearer otro", "secreto-de-prueba-123456"} {
		req, _ := http.NewRequest("GET", hs.URL+"/api/v1/peers", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("auth %q: %v %v", auth, err, resp.StatusCode)
		}
	}
	// healthz y la GUI no necesitan token.
	for _, p := range []string{"/healthz", "/"} {
		resp, err := http.Get(hs.URL + p)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %v %v", p, err, resp)
		}
	}
	// Cliente con token erróneo: error claro.
	if _, err := NewClient(hs.URL, "malo").Peers(context.Background()); err != ErrUnauthorized {
		t.Fatalf("esperaba ErrUnauthorized, %v", err)
	}
}

func TestHeartbeatPeersAndOffline(t *testing.T) {
	_, hs, st, advance := newTestServer(t, false)
	c := NewClient(hs.URL, "secreto-de-prueba-123456")
	ctx := context.Background()
	hb := Heartbeat{ID: "abc123", Name: "PT-MEJORAC", Version: "dev", Roles: Roles{Server: true},
		Addrs: []string{"192.168.5.56"}, Port: 3240,
		Devices: []Device{{BusID: "1-13", VID: "0951", PID: "1666", Product: "DataTraveler 3.0"}}}
	iv, err := c.Heartbeat(ctx, hb)
	if err != nil || iv != 15*time.Second {
		t.Fatalf("%v %v", iv, err)
	}
	if _, err := c.Heartbeat(ctx, Heartbeat{ID: "cli1", Name: "PC-TALLER", Roles: Roles{Client: true}}); err != nil {
		t.Fatal(err)
	}
	peers, err := c.Peers(ctx)
	if err != nil || len(peers) != 2 {
		t.Fatalf("%v %+v", err, peers)
	}
	var srv Peer
	for _, p := range peers {
		if p.ID == "abc123" {
			srv = p
		}
	}
	if !srv.Online || srv.ObservedIP != "127.0.0.1" || len(srv.Devices) != 1 || !srv.Roles.Server {
		t.Fatalf("%+v", srv)
	}
	if h := srv.Hosts(); len(h) != 2 || h[0] != "127.0.0.1" || h[1] != "192.168.5.56" {
		t.Fatalf("hosts %v", h)
	}

	advance(46 * time.Second)
	for _, p := range st.Peers() {
		if p.Online {
			t.Fatalf("%s debería estar desconectado", p.ID)
		}
	}

	// ID no válido.
	if _, err := c.Heartbeat(ctx, Heartbeat{ID: "../x"}); err == nil {
		t.Fatal("debería rechazar un id no válido")
	}
}

func TestPersistenceAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	st, _ := NewStore(dir, 45*time.Second)
	if err := st.Update(Heartbeat{ID: "abc", Name: "uno"}, "10.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}
	st2, err := NewStore(dir, 45*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if p := st2.Peers(); len(p) != 1 || p[0].Name != "uno" || p[0].ObservedIP != "10.0.0.1" {
		t.Fatalf("%+v", p)
	}
	if !st2.Delete("abc") || st2.Delete("abc") {
		t.Fatal("delete")
	}
}

func TestRegenerateToken(t *testing.T) {
	s, hs, _, _ := newTestServer(t, false)
	req, _ := http.NewRequest("POST", hs.URL+"/api/v1/token/regenerate", nil)
	req.Header.Set("Authorization", "Bearer secreto-de-prueba-123456")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("%v %v", err, resp.StatusCode)
	}
	if s.currentToken() == "secreto-de-prueba-123456" {
		t.Fatal("el token no cambió")
	}
	saved, _ := os.ReadFile(filepath.Join(s.DataDir, tokenFile))
	if strings.TrimSpace(string(saved)) != s.currentToken() {
		t.Fatal("el token nuevo no se guardó en el volumen")
	}
	// El token antiguo ya no vale.
	if _, err := NewClient(hs.URL, "secreto-de-prueba-123456").Peers(context.Background()); err != ErrUnauthorized {
		t.Fatalf("el token antiguo debería fallar: %v", err)
	}

	// Con token fijado por variable de entorno no se permite.
	_, hs2, _, _ := newTestServer(t, true)
	req, _ = http.NewRequest("POST", hs2.URL+"/api/v1/token/regenerate", nil)
	req.Header.Set("Authorization", "Bearer secreto-de-prueba-123456")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("con token de entorno debería dar 409, da %d", resp.StatusCode)
	}
}

func TestWebScriptParses(t *testing.T) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	for i, m := range regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindAllStringSubmatch(string(b), -1) {
		if _, err := parser.ParseFile(nil, "index.html", m[1], 0); err != nil {
			t.Errorf("script %d: %v", i, err)
		}
	}
}

// La web incluye el logo (cabecera e icono de la pestaña) sin pedir token.
func TestLogoServed(t *testing.T) {
	_, hs, _, _ := newTestServer(t, false)
	resp, err := http.Get(hs.URL + "/logo.png")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("logo: %s %q", resp.Status, resp.Header.Get("Content-Type"))
	}
}
