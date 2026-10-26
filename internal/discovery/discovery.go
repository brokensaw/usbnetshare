// Package discovery permite al cliente localizar servidores por su nombre
// lógico, sin conocer su IP. El cliente envía una consulta UDP por multicast,
// broadcast general y broadcast dirigido de cada interfaz; los servidores que
// coinciden responden por unicast. La IP de origen de la respuesta es la
// dirección que se usa para conectar, así que un cambio de IP (DHCP, cambio
// de red, etc.) se resuelve solo en el siguiente descubrimiento.
package discovery

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/net/ipv4"
)

const Magic = "usbnet/1"

type DeviceSummary struct {
	BusID        string `json:"busid"`
	VID          string `json:"vid"`
	PID          string `json:"pid"`
	Product      string `json:"product,omitempty"`
	Manufacturer string `json:"manufacturer,omitempty"`
	Busy         bool   `json:"busy"`
}

type query struct {
	Magic string `json:"magic"`
	Type  string `json:"type"`
	Name  string `json:"name,omitempty"`
	Nonce string `json:"nonce"`
}

type Announce struct {
	Magic   string          `json:"magic"`
	Type    string          `json:"type"`
	Name    string          `json:"name"`
	ID      string          `json:"id"`
	Port    int             `json:"port"`
	Devices []DeviceSummary `json:"devices"`
	Nonce   string          `json:"nonce"`
	MAC     string          `json:"mac,omitempty"`
}

func sign(key []byte, a *Announce) string {
	m := hmac.New(sha256.New, key)
	fmt.Fprintf(m, "%s|%s|%s|%d", a.Nonce, a.Name, a.ID, a.Port)
	return hex.EncodeToString(m.Sum(nil))
}

func signQuery(key []byte, nonce string) string {
	m := hmac.New(sha256.New, key)
	fmt.Fprintf(m, "q|%s", nonce)
	return hex.EncodeToString(m.Sum(nil))
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func usableIfaces() []net.Interface {
	ifs, _ := net.Interfaces()
	var out []net.Interface
	for _, ifi := range ifs {
		if ifi.Flags&net.FlagUp == 0 {
			continue
		}
		out = append(out, ifi)
	}
	return out
}

// ---------------------------------------------------------------------------
// Lado servidor
// ---------------------------------------------------------------------------

type Responder struct {
	Name    string
	ID      string
	TCPPort int
	UDPPort int
	Group   string
	Key     []byte
	Devices func() []DeviceSummary
	Log     *slog.Logger
}

// Run atiende consultas hasta que ctx termina. Si el socket falla (p.ej. la
// interfaz desaparece) se vuelve a abrir.
func (r *Responder) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if err := r.serve(ctx); err != nil && ctx.Err() == nil {
			r.Log.Warn("descubrimiento: reiniciando", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(3 * time.Second):
			}
		}
	}
}

func (r *Responder) joinAll(p *ipv4.PacketConn, group net.IP) {
	for _, ifi := range usableIfaces() {
		if ifi.Flags&net.FlagMulticast == 0 {
			continue
		}
		ifi := ifi
		_ = p.JoinGroup(&ifi, &net.UDPAddr{IP: group}) // "ya unido" se ignora
	}
}

func (r *Responder) serve(ctx context.Context) error {
	pc, err := net.ListenPacket("udp4", fmt.Sprintf(":%d", r.UDPPort))
	if err != nil {
		return err
	}
	defer pc.Close()
	p := ipv4.NewPacketConn(pc)
	group := net.ParseIP(r.Group)
	r.joinAll(p, group)
	lastJoin := time.Now()
	r.Log.Info("descubrimiento activo", "udp", r.UDPPort, "grupo", r.Group, "nombre", r.Name)

	buf := make([]byte, 2048)
	for ctx.Err() == nil {
		// Las interfaces pueden aparecer/cambiar (Wi-Fi, VPN, cable): re-unirse.
		if time.Since(lastJoin) > 30*time.Second {
			r.joinAll(p, group)
			lastJoin = time.Now()
		}
		_ = pc.SetReadDeadline(time.Now().Add(time.Second))
		n, src, err := pc.ReadFrom(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		var q query
		if json.Unmarshal(buf[:n], &q) != nil || q.Magic != Magic || q.Type != "query" {
			continue
		}
		if q.Name != "" && !strings.EqualFold(q.Name, r.Name) {
			continue
		}
		if len(r.Key) > 0 {
			// El nonce de una consulta autenticada lleva su firma: "nonce.mac".
			parts := strings.SplitN(q.Nonce, ".", 2)
			if len(parts) != 2 || !hmac.Equal([]byte(parts[1]), []byte(signQuery(r.Key, parts[0]))) {
				continue
			}
		}
		a := Announce{Magic: Magic, Type: "announce", Name: r.Name, ID: r.ID,
			Port: r.TCPPort, Nonce: q.Nonce}
		if r.Devices != nil {
			a.Devices = r.Devices()
		}
		if len(r.Key) > 0 {
			a.MAC = sign(r.Key, &a)
		}
		out, _ := json.Marshal(a)
		if _, err := pc.WriteTo(out, src); err != nil {
			r.Log.Debug("descubrimiento: no se pudo responder", "a", src, "err", err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Lado cliente
// ---------------------------------------------------------------------------

type Options struct {
	Port    int
	Group   string
	Key     []byte
	Name    string // filtro por nombre lógico ("" = cualquiera)
	Timeout time.Duration
}

// Sent cuenta las búsquedas por difusión realizadas. Los tests lo usan para
// garantizar que, desactivada la difusión, USBnetShare no envía nada por la red.
var Sent atomic.Int64

// No hay barrido unicast de subredes: enviar un paquete a cada IP provocaba
// tormentas de ARP en redes industriales (PROFINET). La localización normal
// es a través del registro (usbnetshare-registry); esto es solo multicast/broadcast
// y está desactivado por defecto.

type Found struct {
	Announce
	Host string // IP desde la que respondió el servidor
}

func (f Found) Addr() string { return net.JoinHostPort(f.Host, fmt.Sprint(f.Port)) }

func broadcastTargets() []net.IP {
	targets := []net.IP{net.IPv4bcast}
	for _, ifi := range usableIfaces() {
		if ifi.Flags&net.FlagBroadcast == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifi.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil {
				continue
			}
			ip, mask := ipn.IP.To4(), net.IP(ipn.Mask).To4()
			if mask == nil {
				continue
			}
			bc := make(net.IP, 4)
			for i := range bc {
				bc[i] = ip[i] | ^mask[i]
			}
			targets = append(targets, bc)
		}
	}
	return targets
}

// Discover devuelve los servidores que respondieron dentro del timeout.
func Discover(ctx context.Context, o Options) ([]Found, error) {
	Sent.Add(1)
	pc, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		return nil, err
	}
	defer pc.Close()
	p := ipv4.NewPacketConn(pc)
	_ = p.SetMulticastLoopback(true)
	_ = p.SetMulticastTTL(2)

	nonce := randomHex(16)
	if len(o.Key) > 0 {
		nonce = nonce + "." + signQuery(o.Key, nonce)
	}
	msg, _ := json.Marshal(query{Magic: Magic, Type: "query", Name: o.Name, Nonce: nonce})
	group := &net.UDPAddr{IP: net.ParseIP(o.Group), Port: o.Port}

	send := func() {
		for _, ifi := range usableIfaces() {
			if ifi.Flags&net.FlagMulticast == 0 {
				continue
			}
			ifi := ifi
			if p.SetMulticastInterface(&ifi) == nil {
				_, _ = p.WriteTo(msg, nil, group)
			}
		}
		for _, ip := range broadcastTargets() {
			_, _ = pc.WriteTo(msg, &net.UDPAddr{IP: ip, Port: o.Port})
		}
	}

	deadline := time.Now().Add(o.Timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	send()
	resent := false
	seen := map[string]bool{}
	var found []Found
	buf := make([]byte, 64*1024)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		// Reenvío a mitad de plazo por si se perdió el primer paquete.
		if !resent && time.Until(deadline) < o.Timeout/2 {
			send()
			resent = true
		}
		_ = pc.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		n, src, err := pc.ReadFrom(buf)
		if err != nil {
			continue
		}
		var a Announce
		if json.Unmarshal(buf[:n], &a) != nil || a.Magic != Magic || a.Type != "announce" || a.Nonce != nonce {
			continue
		}
		if len(o.Key) > 0 && !hmac.Equal([]byte(a.MAC), []byte(sign(o.Key, &a))) {
			continue
		}
		if o.Name != "" && !strings.EqualFold(a.Name, o.Name) {
			continue
		}
		if seen[a.ID] { // mismo servidor respondiendo por varias interfaces
			continue
		}
		seen[a.ID] = true
		ua, _ := src.(*net.UDPAddr)
		if ua == nil {
			continue
		}
		found = append(found, Found{Announce: a, Host: ua.IP.String()})
	}
	return found, ctx.Err()
}
