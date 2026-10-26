package config

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestExampleConfig(t *testing.T) {
	// Se carga una copia: Load guarda el id generado y no debe tocar el ejemplo.
	b, err := os.ReadFile("../../configs/usbnetshare.yaml")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	s := a.Server
	if s.Name != "taller-pi" || !s.TakeoverEnabled() || !s.Export[0].Matches(0x0908, 0x0004, "1-1") {
		t.Fatalf("%+v", s)
	}
	c := a.Client
	if !c.Enabled || c.Reconnect.MaxBackoff.Seconds() != 30 || c.Reconnect.WaitDevice.Seconds() != 5 {
		t.Fatalf("%+v", c)
	}
	if a.API != DefaultAPIAddr || a.ID == "" || a.Registry.Enabled() {
		t.Fatalf("api %q id %q registro %+v", a.API, a.ID, a.Registry)
	}
	// El ejemplo no debe activar nada que genere tráfico de difusión.
	if s.Discovery.IsEnabled() || c.Discovery.IsEnabled() || len(a.Deprecated()) > 0 {
		t.Fatal("el ejemplo no debe activar el descubrimiento por difusión ni opciones obsoletas")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", FileName)
	a, err := Load(p) // no existe: valores por defecto
	if err != nil || !a.Client.Enabled || a.Server.Enabled {
		t.Fatalf("%v %+v", err, a)
	}
	a.Server.Enabled = true
	a.Server.Export = []Match{{VID: "0908"}}
	a.Client.Devices = []Match{{VID: "0908", BusID: "1-2", Server: "taller-pi"}}
	if err := a.Save(p); err != nil {
		t.Fatal(err)
	}
	b, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !b.Server.Enabled || b.Client.Devices[0].Server != "taller-pi" || b.Server.KeepAlive != a.Server.KeepAlive {
		t.Fatalf("%+v", b)
	}
	if b.ID == "" || b.ID != a.ID {
		t.Fatalf("el ID del equipo debe conservarse: %q -> %q", a.ID, b.ID)
	}
}

// Un USBnetShare.yaml sin id recibe uno al cargarse y se guarda, para que el
// servidor conserve su identidad en el siguiente arranque.
func TestIDIsPersistedOnLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), FileName)
	yml := `server:
  enabled: true
  name: x
`
	if err := os.WriteFile(p, []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := Load(p)
	if err != nil || a.ID == "" {
		t.Fatalf("%v %+v", err, a)
	}
	b, err := Load(p)
	if err != nil || b.ID != a.ID {
		t.Fatalf("el ID cambió entre cargas: %q -> %q (%v)", a.ID, b.ID, err)
	}
}

// Los ficheros antiguos tenían el id en server.id: se migra a la raíz.
func TestIDMigratedFromServer(t *testing.T) {
	p := filepath.Join(t.TempDir(), FileName)
	yml := `server:
  id: viejo123
  name: x
`
	if err := os.WriteFile(p, []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := Load(p)
	if err != nil || a.ID != "viejo123" || a.Server.ID != "" {
		t.Fatalf("%v id=%q server.id=%q", err, a.ID, a.Server.ID)
	}
	raw, _ := os.ReadFile(p)
	var top map[string]any
	if err := yaml.Unmarshal(raw, &top); err != nil || top["id"] != "viejo123" {
		t.Fatalf("el id migrado no se guardó en la raíz (%v):\n%s", err, raw)
	}
	if srv, _ := top["server"].(map[string]any); srv["id"] != nil {
		t.Fatalf("server.id debería desaparecer tras migrar:\n%s", raw)
	}
}

// sweep/scan de versiones anteriores se aceptan pero se ignoran y avisan.
func TestDeprecatedSweepIgnored(t *testing.T) {
	p := filepath.Join(t.TempDir(), FileName)
	yml := `client:
  discovery:
    sweep: true
    scan: ["192.168.102.0/24"]
`
	if err := os.WriteFile(p, []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Deprecated()) == 0 {
		t.Fatal("debería avisar de sweep/scan obsoletos")
	}
	if a.Client.Discovery.IsEnabled() {
		t.Fatal("el descubrimiento por difusión debe estar desactivado por defecto")
	}
}

func TestRegistryValidation(t *testing.T) {
	a := Default()
	a.Registry = Registry{URL: "http://registro:3250/", Token: "t"}
	if err := a.Normalize(); err != nil || a.Registry.URL != "http://registro:3250" {
		t.Fatalf("%v %q", err, a.Registry.URL)
	}
	a.Registry = Registry{URL: "registro:3250", Token: "t"}
	if a.Normalize() == nil {
		t.Fatal("debería exigir http:// o https://")
	}
	a.Registry = Registry{URL: "http://registro:3250"}
	if a.Normalize() == nil {
		t.Fatal("debería exigir el token")
	}
}

func TestAPIMustBeLoopback(t *testing.T) {
	a := Default()
	a.API = "0.0.0.0:3242"
	if a.Normalize() == nil {
		t.Fatal("debería rechazar una API expuesta a la red")
	}
}

// La configuración de usbnet se copia a la ruta nueva de USBnetShare la
// primera vez, sin tocar la antigua ni pisar una nueva que ya exista.
func TestMigrateLegacy(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "usbnet", "usbnet.yaml")
	newPath := filepath.Join(dir, "USBnetShare", "USBnetShare.yaml")
	if migrateLegacy(oldPath, newPath) {
		t.Fatal("no hay configuración antigua: no debería copiar nada")
	}
	if err := os.MkdirAll(filepath.Dir(oldPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldPath, []byte("id: abc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !migrateLegacy(oldPath, newPath) {
		t.Fatal("debería copiar la configuración antigua")
	}
	if b, err := os.ReadFile(newPath); err != nil || string(b) != "id: abc\n" {
		t.Fatalf("copia: %q %v", b, err)
	}
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatal("la configuración antigua debe quedarse como respaldo")
	}
	_ = os.WriteFile(oldPath, []byte("id: otro\n"), 0o644)
	if migrateLegacy(oldPath, newPath) {
		t.Fatal("no debe pisar una configuración nueva que ya existe")
	}
}
