// Package config carga y guarda la configuración YAML de USBnetShare: una sola
// aplicación con un rol de servidor y otro de cliente, cada uno activable.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/brokensaw/usbnetshare/internal/appinfo"
)

// Match selecciona dispositivos USB. Los campos vacíos actúan como comodín,
// pero al menos uno de vid, pid o busid debe estar definido.
type Match struct {
	VID   string `yaml:"vid,omitempty" json:"vid"`     // hex, p.ej. "0908" (Siemens)
	PID   string `yaml:"pid,omitempty" json:"pid"`     // hex
	BusID string `yaml:"busid,omitempty" json:"busid"` // p.ej. "1-1.2"
	// Server (solo en reglas de cliente) limita la regla a un servidor por su
	// nombre lógico. Vacío = el servidor general del cliente, o cualquiera.
	Server string `yaml:"server,omitempty" json:"server"`
	// ServerID (solo cliente) identifica el servidor por su ID en el registro;
	// si está, manda sobre Server (que queda como nombre para mostrar).
	ServerID string `yaml:"server_id,omitempty" json:"server_id"`
}

func parseHex16(s string) (uint16, bool) {
	s = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "0x")
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 16, 16)
	return uint16(v), err == nil
}

func (m Match) Valid() error {
	if m.VID == "" && m.PID == "" && m.BusID == "" {
		return fmt.Errorf("regla vacía: indica vid, pid o busid")
	}
	if m.VID != "" {
		if _, ok := parseHex16(m.VID); !ok {
			return fmt.Errorf("vid inválido %q", m.VID)
		}
	}
	if m.PID != "" {
		if _, ok := parseHex16(m.PID); !ok {
			return fmt.Errorf("pid inválido %q", m.PID)
		}
	}
	return nil
}

func (m Match) Matches(vid, pid uint16, busid string) bool {
	if v, ok := parseHex16(m.VID); ok && v != vid {
		return false
	}
	if p, ok := parseHex16(m.PID); ok && p != pid {
		return false
	}
	if m.BusID != "" && m.BusID != busid {
		return false
	}
	return m.VID != "" || m.PID != "" || m.BusID != ""
}

func (m Match) String() string {
	var parts []string
	if m.Server != "" {
		parts = append(parts, "servidor="+m.Server)
	}
	if m.ServerID != "" {
		parts = append(parts, "id="+m.ServerID)
	}
	if m.VID != "" {
		parts = append(parts, "vid="+m.VID)
	}
	if m.PID != "" {
		parts = append(parts, "pid="+m.PID)
	}
	if m.BusID != "" {
		parts = append(parts, "busid="+m.BusID)
	}
	return strings.Join(parts, ",")
}

type Discovery struct {
	Enabled *bool  `yaml:"enabled" json:"enabled"`
	Port    int    `yaml:"port" json:"port"`
	Group   string `yaml:"group" json:"group"`
	// Clave compartida opcional: el cliente solo acepta respuestas firmadas
	// con HMAC-SHA256 y el servidor solo responde a quien conoce la clave.
	Key string `yaml:"key" json:"key"`
	// Obsoletos: el barrido de subredes se eliminó porque provocaba tormentas
	// de ARP en redes de PLC. Se aceptan para no romper ficheros antiguos,
	// pero se ignoran (ver Deprecated).
	Sweep *bool    `yaml:"sweep,omitempty" json:"-"`
	Scan  []string `yaml:"scan,omitempty" json:"-"`
}

// IsEnabled: el descubrimiento por multicast/broadcast está desactivado
// salvo que se active explícitamente (enabled: true). Lo normal es usar el
// registro (usbnetshare-registry), que no genera tráfico de difusión.
func (d Discovery) IsEnabled() bool { return d.Enabled != nil && *d.Enabled }

// KeepAlive controla la detección de enlaces caídos.
// Una caída de red más corta que Idle + Interval*Count no rompe la sesión:
// TCP retransmite y las transferencias pendientes simplemente esperan.
type KeepAlive struct {
	Idle        time.Duration `yaml:"idle" json:"idle"`
	Interval    time.Duration `yaml:"interval" json:"interval"`
	Count       int           `yaml:"count" json:"count"`
	UserTimeout time.Duration `yaml:"user_timeout" json:"user_timeout"` // solo Linux (TCP_USER_TIMEOUT)
}

type Server struct {
	Name string `yaml:"name" json:"name"`
	// ID del servidor. En USBnetShare.yaml el identificador vive en la raíz (App.ID);
	// aquí solo se lee para migrar ficheros antiguos y el motor lo rellena
	// con App.ID al arrancar el rol servidor.
	ID      string `yaml:"id,omitempty" json:"-"`
	Listen  string `yaml:"listen" json:"listen"`
	Backend string `yaml:"backend" json:"backend"` // "kernel" (Linux), "libusb" o "" = automático
	// UsbDk (Windows, backend libusb): "" o "auto" = usarlo si está instalado;
	// "off" = solo dispositivos con WinUSB. Cambiarlo requiere reiniciar USBnetShare.
	UsbDk      string    `yaml:"usbdk,omitempty" json:"usbdk"`
	Export     []Match   `yaml:"export" json:"export"`
	Takeover   *bool     `yaml:"takeover" json:"takeover"`
	AllowCIDRs []string  `yaml:"allow_cidrs" json:"allow_cidrs"`
	Discovery  Discovery `yaml:"discovery" json:"discovery"`
	KeepAlive  KeepAlive `yaml:"keepalive" json:"keepalive"`
}

func (s *Server) TakeoverEnabled() bool { return s.Takeover == nil || *s.Takeover }

func (s *Server) UsbDkEnabled() bool { return !strings.EqualFold(strings.TrimSpace(s.UsbDk), "off") }

type Reconnect struct {
	MinBackoff       time.Duration `yaml:"min_backoff" json:"min_backoff"`
	MaxBackoff       time.Duration `yaml:"max_backoff" json:"max_backoff"`
	DiscoveryTimeout time.Duration `yaml:"discovery_timeout" json:"discovery_timeout"`
	PollInterval     time.Duration `yaml:"poll_interval" json:"poll_interval"`
	// WaitDevice es cada cuánto se comprueba si ha vuelto un dispositivo que
	// se desenchufó en el servidor (sin backoff: se quiere reconectar enseguida).
	WaitDevice time.Duration `yaml:"wait_device" json:"wait_device"`
}

type Client struct {
	// Server es el nombre lógico del servidor (no su IP). Vacío = cualquiera.
	Server        string    `yaml:"server" json:"server"`
	Devices       []Match   `yaml:"devices" json:"devices"`
	StaticServers []string  `yaml:"static_servers" json:"static_servers"` // "host:puerto" (nombres DNS/mDNS)
	Discovery     Discovery `yaml:"discovery" json:"discovery"`
	Reconnect     Reconnect `yaml:"reconnect" json:"reconnect"`
	KeepAlive     KeepAlive `yaml:"keepalive" json:"keepalive"`
}

// ServerRole y ClientRole añaden el interruptor de cada rol.
type ServerRole struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	Server  `yaml:",inline" json:",inline"`
}

type ClientRole struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	Client  `yaml:",inline" json:",inline"`
}

// Registry es el servidor de identidades (usbnetshare-registry): cada equipo se
// registra en él y pregunta por los demás. Sustituye a la búsqueda por red.
type Registry struct {
	URL      string        `yaml:"url" json:"url"`     // p.ej. "http://registro:3250"
	Token    string        `yaml:"token" json:"token"` // el que muestra el contenedor al arrancar
	Interval time.Duration `yaml:"interval,omitempty" json:"interval"`
}

func (r Registry) Enabled() bool { return r.URL != "" }

// App es el contenido completo de USBnetShare.yaml.
type App struct {
	// ID identifica a este equipo de forma estable (no cambia aunque cambie la
	// IP o el nombre). Se genera la primera vez y se guarda.
	ID       string     `yaml:"id" json:"id"`
	Registry Registry   `yaml:"registry" json:"registry"`
	Server   ServerRole `yaml:"server" json:"server"`
	Client   ClientRole `yaml:"client" json:"client"`
	// API es la dirección de control local que usa la interfaz gráfica
	// para hablar con el servicio. Solo se admite loopback.
	API      string `yaml:"api" json:"api"`
	LogFile  string `yaml:"log_file" json:"log_file"`
	LogLevel string `yaml:"log_level" json:"log_level"`
}

const (
	DefaultTCPPort   = 3240
	DefaultDiscPort  = 3241
	DefaultAPIAddr   = "127.0.0.1:3242"
	DefaultDiscGroup = "239.255.32.41"
)

// FileName es el fichero de configuración ("USBnetShare.yaml" en Windows,
// "usbnetshare.yaml" en Linux); legacyFileName, el de las versiones usbnet.
var (
	FileName       = appinfo.ID + ".yaml"
	legacyFileName = appinfo.LegacyID + ".yaml"
)

func defaultsDiscovery(d *Discovery) {
	if d.Port == 0 {
		d.Port = DefaultDiscPort
	}
	if d.Group == "" {
		d.Group = DefaultDiscGroup
	}
}

func defaultsKeepAlive(k *KeepAlive) {
	if k.Idle == 0 {
		k.Idle = 5 * time.Second
	}
	if k.Interval == 0 {
		k.Interval = 3 * time.Second
	}
	if k.Count == 0 {
		k.Count = 5
	}
	if k.UserTimeout == 0 {
		k.UserTimeout = 30 * time.Second
	}
}

// Normalize rellena valores por defecto y valida. No exige reglas: un
// servidor sin "export" no comparte nada y un cliente sin "devices" no
// importa nada, lo que es normal mientras se configura desde la GUI.
func (a *App) Normalize() error {
	s := &a.Server.Server
	if s.Name == "" {
		h, _ := os.Hostname()
		s.Name = h
	}
	if s.Listen == "" {
		s.Listen = fmt.Sprintf(":%d", DefaultTCPPort)
	}
	if a.ID == "" {
		a.ID = s.ID // migración: antes el id estaba en server.id
	}
	if a.ID == "" {
		a.ID = newID()
	}
	s.ID = ""

	if a.Registry.URL != "" {
		u, err := url.Parse(a.Registry.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("registry.url: debe ser http://host:puerto o https://host:puerto, no %q", a.Registry.URL)
		}
		a.Registry.URL = strings.TrimRight(a.Registry.URL, "/")
		if a.Registry.Token == "" {
			return fmt.Errorf("registry.token: falta el token (se muestra en la consola del contenedor al arrancar)")
		}
	}
	if a.Registry.Interval == 0 {
		a.Registry.Interval = 15 * time.Second
	}
	for _, m := range s.Export {
		if err := m.Valid(); err != nil {
			return fmt.Errorf("server.export: %w", err)
		}
	}
	defaultsDiscovery(&s.Discovery)
	defaultsKeepAlive(&s.KeepAlive)

	c := &a.Client.Client
	for _, m := range c.Devices {
		if err := m.Valid(); err != nil {
			return fmt.Errorf("client.devices: %w", err)
		}
	}
	defaultsDiscovery(&c.Discovery)
	defaultsKeepAlive(&c.KeepAlive)
	r := &c.Reconnect
	if r.MinBackoff == 0 {
		r.MinBackoff = time.Second
	}
	if r.MaxBackoff == 0 {
		r.MaxBackoff = 30 * time.Second
	}
	if r.DiscoveryTimeout == 0 {
		r.DiscoveryTimeout = 2 * time.Second
	}
	if r.PollInterval == 0 {
		r.PollInterval = time.Second
	}
	if r.WaitDevice == 0 {
		r.WaitDevice = 5 * time.Second
	}

	if a.API == "" {
		a.API = DefaultAPIAddr
	}
	host, _, err := splitHostPort(a.API)
	if err != nil {
		return fmt.Errorf("api: %w", err)
	}
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		return fmt.Errorf("api: solo se permite una dirección local (127.0.0.1), no %q", host)
	}
	if a.LogLevel == "" {
		a.LogLevel = "info"
	}
	return nil
}

func splitHostPort(s string) (string, string, error) {
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return "", "", fmt.Errorf("falta el puerto en %q", s)
	}
	return strings.Trim(s[:i], "[]"), s[i+1:], nil
}

// Default devuelve una configuración nueva: cliente activado, servidor
// desactivado, sin reglas.
func Default() *App {
	a := &App{Client: ClientRole{Enabled: true}}
	_ = a.Normalize()
	return a
}

// Load lee path. Si no existe devuelve Default() sin error, para que la
// primera ejecución abra la GUI vacía en lugar de fallar.
func Load(path string) (*App, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Default(), nil
	}
	if err != nil {
		return nil, err
	}
	a := &App{}
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(a); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	hadID := a.ID != "" && a.Server.ID == ""
	if err := a.Normalize(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if !hadID {
		// Primera vez con identificador (o migración desde server.id): se
		// guarda para que no cambie. Sin permiso de escritura se reintentará.
		_ = a.Save(path)
	}
	return a, nil
}

// Deprecated devuelve avisos sobre opciones obsoletas que se ignoran.
func (a *App) Deprecated() []string {
	var out []string
	for _, d := range []struct {
		where string
		disc  Discovery
	}{{"client.discovery", a.Client.Discovery}, {"server.discovery", a.Server.Discovery}} {
		if d.disc.Sweep != nil || len(d.disc.Scan) > 0 {
			out = append(out, d.where+".sweep/scan ya no existen y se ignoran: el barrido de red se eliminó; usa el registro (registry.url)")
		}
	}
	return out
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Save escribe la configuración de forma atómica (fichero temporal + rename).
func (a *App) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := yaml.Marshal(a)
	if err != nil {
		return err
	}
	b = append([]byte("# Configuración de USBnetShare. Se puede editar a mano o desde la interfaz gráfica.\n"), b...)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// DefaultPath elige dónde vive la configuración:
//  1. junto al ejecutable, si ya existe allí (instalación portátil; también
//     el usbnet.yaml de versiones anteriores);
//  2. %ProgramData%\USBnetShare en Windows (compartido entre el servicio y la GUI);
//  3. /etc/usbnetshare en Linux.
//
// Si aún no existe y sí la de USBnetShare (%ProgramData%\USBnetShare\USBnetShare.yaml o
// /etc/usbnet/USBnetShare.yaml), se copia; el fichero antiguo se deja como respaldo.
func DefaultPath() string {
	if exe, err := os.Executable(); err == nil {
		for _, name := range []string{FileName, legacyFileName} {
			p := filepath.Join(filepath.Dir(exe), name)
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	p := filepath.Join(systemDir(appinfo.ID), FileName)
	migrateLegacy(filepath.Join(systemDir(appinfo.LegacyID), legacyFileName), p)
	return p
}

func systemDir(id string) string {
	if runtime.GOOS == "windows" {
		pd := os.Getenv("ProgramData")
		if pd == "" {
			pd = `C:\ProgramData`
		}
		return filepath.Join(pd, id)
	}
	return filepath.Join("/etc", id)
}

// migrateLegacy copia la configuración antigua a la ruta nueva si esta aún no
// existe. Devuelve true si ha copiado.
func migrateLegacy(oldPath, newPath string) bool {
	if _, err := os.Stat(newPath); err == nil {
		return false
	}
	b, err := os.ReadFile(oldPath)
	if err != nil {
		return false
	}
	if err := os.MkdirAll(filepath.Dir(newPath), 0o755); err != nil {
		return false
	}
	return os.WriteFile(newPath, b, 0o644) == nil
}
