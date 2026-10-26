package registry

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

//go:embed web
var webFS embed.FS

// Server atiende la API y la GUI web del registro.
type Server struct {
	Store        *Store
	DataDir      string
	Version      string
	TokenFromEnv bool
	Interval     time.Duration // intervalo de latido que se pide a los equipos
	Log          *slog.Logger
	Out          io.Writer // consola donde se imprime el token (stdout)

	mu    sync.RWMutex
	token string
}

func NewServer(store *Store, dataDir, token string, fromEnv bool, version string, log *slog.Logger) *Server {
	return &Server{Store: store, DataDir: dataDir, token: token, TokenFromEnv: fromEnv,
		Version: version, Interval: 15 * time.Second, Log: log, Out: io.Discard}
}

func (s *Server) currentToken() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.token
}

// authorized compara el token en tiempo constante.
func (s *Server) authorized(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return false
	}
	got := []byte(strings.TrimSpace(h[len(prefix):]))
	want := []byte(s.currentToken())
	return len(want) > 0 && subtle.ConstantTimeCompare(got, want) == 1
}

func (s *Server) Handler() http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("POST /api/v1/heartbeat", s.heartbeat)
	api.HandleFunc("GET /api/v1/peers", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.Store.Peers())
	})
	api.HandleFunc("DELETE /api/v1/peers/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !s.Store.Delete(r.PathValue("id")) {
			writeError(w, http.StatusNotFound, "equipo no encontrado")
			return
		}
		s.Log.Info("equipo eliminado del registro", "id", r.PathValue("id"))
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	api.HandleFunc("GET /api/v1/info", func(w http.ResponseWriter, r *http.Request) {
		peers := s.Store.Peers()
		online := 0
		for _, p := range peers {
			if p.Online {
				online++
			}
		}
		writeJSON(w, http.StatusOK, Info{Version: s.Version, TokenFromEnv: s.TokenFromEnv,
			Peers: len(peers), Online: online, OfflineAfterS: int(s.Store.offlineAfter / time.Second)})
	})
	api.HandleFunc("POST /api/v1/token/regenerate", s.regenerate)

	web, _ := fs.Sub(webFS, "web")
	static := http.FileServer(http.FS(web))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})
	mux.Handle("/api/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			writeError(w, http.StatusUnauthorized, "token no válido")
			return
		}
		api.ServeHTTP(w, r)
	}))
	mux.Handle("/", securityHeaders(static))
	return mux
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'")
		h.ServeHTTP(w, r)
	})
}

func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request) {
	var hb Heartbeat
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&hb); err != nil {
		writeError(w, http.StatusBadRequest, "latido ilegible: "+err.Error())
		return
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	if err := s.Store.Update(hb, ip); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, HeartbeatReply{IntervalSeconds: int(s.Interval / time.Second)})
}

// regenerate crea un token nuevo. Si el token viene de la variable de
// entorno no se permite: al reiniciar el contenedor volvería el de la variable.
func (s *Server) regenerate(w http.ResponseWriter, r *http.Request) {
	if s.TokenFromEnv {
		writeError(w, http.StatusConflict, "el token está fijado con USBNETSHARE_REGISTRY_TOKEN: cámbialo en la variable de entorno del contenedor")
		return
	}
	t, err := NewToken()
	if err == nil {
		err = SaveToken(s.DataDir, t)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.mu.Lock()
	s.token = t
	s.mu.Unlock()
	s.Log.Warn("token regenerado desde la GUI; actualízalo en todos los equipos USBnetShare")
	PrintToken(s.Out, t, false, false)
	writeJSON(w, http.StatusOK, map[string]string{"token": t})
}

// PrintToken muestra el token en la consola (docker logs), tal cual, sin
// formato de log para que se pueda copiar fácilmente.
func PrintToken(w io.Writer, token string, fromEnv, created bool) {
	origin := "guardado en el volumen de datos"
	switch {
	case fromEnv:
		origin = "fijado con USBNETSHARE_REGISTRY_TOKEN"
	case created:
		origin = "generado ahora y guardado en el volumen de datos"
	}
	line := strings.Repeat("=", 72)
	_, _ = io.WriteString(w, "\n"+line+"\n  TOKEN DE CONEXIÓN DE USBNETSHARE-REGISTRY ("+origin+")\n\n    "+token+
		"\n\n  Cópialo en cada USBnetShare: Ajustes → Servidor de identidades → Token.\n"+line+"\n\n")
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// Run sirve hasta que ctx termina, guardando peers.json periódicamente.
func (s *Server) Run(ctx context.Context, addr, certFile, keyFile string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second}
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				_ = s.Store.Flush()
				shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = srv.Shutdown(shut)
				cancel()
				return
			case <-t.C:
				if err := s.Store.Flush(); err != nil {
					s.Log.Error("no se pudo guardar peers.json", "err", err)
				}
			}
		}
	}()
	scheme := "http"
	if certFile != "" {
		scheme = "https"
	}
	s.Log.Info("usbnetshare-registry escuchando", "dirección", ln.Addr().String(), "protocolo", scheme)
	if certFile != "" {
		err = srv.ServeTLS(ln, certFile, keyFile)
	} else {
		err = srv.Serve(ln)
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
