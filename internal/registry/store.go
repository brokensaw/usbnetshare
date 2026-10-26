package registry

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/brokensaw/usbnetshare/internal/usbids"
)

const (
	tokenFile = "token"
	peersFile = "peers.json"
)

// LoadOrCreateToken decide el token del registro:
//  1. la variable de entorno (envToken), si viene; se guarda también en el
//     volumen para que siga siendo el mismo aunque luego se quite la variable;
//  2. el guardado en dataDir/token (persistente al actualizar o recrear el
//     contenedor, mientras se conserve el volumen);
//  3. uno nuevo aleatorio, que se guarda.
func LoadOrCreateToken(dataDir, envToken string) (token string, fromEnv, created bool, err error) {
	path := filepath.Join(dataDir, tokenFile)
	if t := strings.TrimSpace(envToken); t != "" {
		if len(t) < 16 {
			return "", false, false, errors.New("USBNETSHARE_REGISTRY_TOKEN es demasiado corto (mínimo 16 caracteres)")
		}
		return t, true, false, writeAtomic(path, []byte(t+"\n"), 0o600)
	}
	if b, err := os.ReadFile(path); err == nil {
		if t := strings.TrimSpace(string(b)); t != "" {
			return t, false, false, nil
		}
	} else if !os.IsNotExist(err) {
		return "", false, false, err
	}
	t, err := NewToken()
	if err != nil {
		return "", false, false, err
	}
	return t, false, true, writeAtomic(path, []byte(t+"\n"), 0o600)
}

// NewToken genera un token aleatorio de 32 bytes.
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// SaveToken guarda un token (al regenerarlo desde la GUI).
func SaveToken(dataDir, token string) error {
	return writeAtomic(filepath.Join(dataDir, tokenFile), []byte(token+"\n"), 0o600)
}

func writeAtomic(path string, b []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Store guarda los equipos conocidos, en memoria y en dataDir/peers.json.
type Store struct {
	dataDir      string
	offlineAfter time.Duration
	now          func() time.Time

	mu    sync.Mutex
	peers map[string]*Peer
	dirty bool
}

func NewStore(dataDir string, offlineAfter time.Duration) (*Store, error) {
	s := &Store{dataDir: dataDir, offlineAfter: offlineAfter, now: time.Now, peers: map[string]*Peer{}}
	b, err := os.ReadFile(filepath.Join(dataDir, peersFile))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err == nil {
		var list []*Peer
		if err := json.Unmarshal(b, &list); err != nil {
			return nil, fmt.Errorf("%s: %w", peersFile, err)
		}
		for _, p := range list {
			s.peers[p.ID] = p
		}
	}
	return s, nil
}

// validID: lo genera USBnetShare (hex), pero se acepta cualquier identificador
// razonable para no depender del formato.
func validID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// Update registra un latido.
func (s *Store) Update(hb Heartbeat, observedIP string) error {
	if !validID(hb.ID) {
		return fmt.Errorf("id no válido %q", hb.ID)
	}
	if len(hb.Name) > 128 || len(hb.Addrs) > 32 || len(hb.Devices) > 256 || len(hb.Conns) > 256 {
		return errors.New("latido demasiado grande")
	}
	fillNames(&hb)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	p, ok := s.peers[hb.ID]
	if !ok {
		p = &Peer{FirstSeen: now}
		s.peers[hb.ID] = p
	}
	p.Heartbeat = hb
	p.ObservedIP = observedIP
	p.LastSeen = now
	s.dirty = true
	return nil
}

// fillNames completa con usb.ids los nombres que no trae el latido (equipos
// con una versión anterior de usbnet, o dispositivos sin nombre propio).
func fillNames(hb *Heartbeat) {
	for i := range hb.Devices {
		d := &hb.Devices[i]
		d.Product, d.Manufacturer = usbids.FillHex(d.VID, d.PID, d.Product, d.Manufacturer)
	}
	for i := range hb.Conns {
		c := &hb.Conns[i]
		if vid, pid, ok := strings.Cut(c.Device, ":"); ok && c.Product == "" {
			c.Product, _ = usbids.FillHex(vid, pid, "", "")
		}
	}
}

// Peers devuelve los equipos, en línea primero y por nombre.
func (s *Store) Peers() []Peer {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	out := make([]Peer, 0, len(s.peers))
	for _, p := range s.peers {
		c := *p
		c.Online = now.Sub(p.LastSeen) <= s.offlineAfter
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Online != out[j].Online {
			return out[i].Online
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

// Delete olvida un equipo. Devuelve false si no existía.
func (s *Store) Delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.peers[id]; !ok {
		return false
	}
	delete(s.peers, id)
	s.dirty = true
	return true
}

// Flush escribe peers.json si hubo cambios.
func (s *Store) Flush() error {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	list := make([]*Peer, 0, len(s.peers))
	for _, p := range s.peers {
		c := *p
		list = append(list, &c)
	}
	s.dirty = false
	s.mu.Unlock()
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(s.dataDir, peersFile), b, 0o600); err != nil {
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
		return err
	}
	return nil
}
