// Package registry implementa el servidor de identidades de USBnetShare
// (usbnetshare-registry) y su cliente. Cada equipo USBnetShare envía un latido con su
// ID, IP, roles y dispositivos; los clientes localizan los servidores por ID
// preguntando al registro, sin generar tráfico de difusión en la red.
package registry

import "time"

// Roles indica qué hace el equipo.
type Roles struct {
	Server bool `json:"server"`
	Client bool `json:"client"`
}

// Device es un USB que un servidor comparte.
type Device struct {
	BusID        string `json:"busid"`
	VID          string `json:"vid"`
	PID          string `json:"pid"`
	Product      string `json:"product,omitempty"`
	Manufacturer string `json:"manufacturer,omitempty"`
	InUseBy      string `json:"in_use_by,omitempty"` // IP:puerto del cliente que lo usa
}

// Conn es una conexión del rol cliente (un USB remoto que usa este equipo).
type Conn struct {
	ServerID   string    `json:"server_id,omitempty"`
	ServerName string    `json:"server_name,omitempty"`
	Device     string    `json:"device"` // "vvvv:pppp" o la regla
	Product    string    `json:"product,omitempty"`
	State      string    `json:"state"`
	Since      time.Time `json:"since"`
}

// Heartbeat es lo que cada USBnetShare envía periódicamente.
type Heartbeat struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Version string   `json:"version"`
	OS      string   `json:"os"`
	Roles   Roles    `json:"roles"`
	Addrs   []string `json:"addrs"` // IP propias del equipo
	Port    int      `json:"port"`  // puerto TCP de USB/IP (si es servidor)
	Devices []Device `json:"devices"`
	Conns   []Conn   `json:"conns"`
}

// HeartbeatReply indica cada cuánto enviar el siguiente latido.
type HeartbeatReply struct {
	IntervalSeconds int `json:"interval_seconds"`
}

// Peer es un equipo conocido por el registro.
type Peer struct {
	Heartbeat
	ObservedIP string    `json:"observed_ip"` // IP desde la que llegó el último latido
	FirstSeen  time.Time `json:"first_seen"`
	LastSeen   time.Time `json:"last_seen"`
	Online     bool      `json:"online"`
}

// Hosts devuelve las direcciones a probar para llegar al equipo: primero la
// observada por el registro y después las que el equipo declara.
func (p Peer) Hosts() []string {
	seen := map[string]bool{}
	var out []string
	for _, h := range append([]string{p.ObservedIP}, p.Addrs...) {
		if h != "" && !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}

// Info es el resumen que muestra la GUI del registro.
type Info struct {
	Version       string `json:"version"`
	TokenFromEnv  bool   `json:"token_from_env"`
	Peers         int    `json:"peers"`
	Online        int    `json:"online"`
	OfflineAfterS int    `json:"offline_after_seconds"`
}
