// Package api expone el motor por HTTP en 127.0.0.1 para que la interfaz
// gráfica controle el servicio, e incluye el cliente (Remote) de esa API.
//
// Protección: solo escucha en loopback y exige la cabecera X-USBNet, que un
// navegador no puede enviar a otro origen sin una petición previa (CORS) que
// este servidor no autoriza. Así una página web no puede reconfigurarlo.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/brokensaw/usbnetshare/internal/config"
	"github.com/brokensaw/usbnetshare/internal/engine"
	"github.com/brokensaw/usbnetshare/internal/logx"
)

const header = "X-USBNet"

// Serve atiende la API hasta que ctx termina.
func Serve(ctx context.Context, addr string, c engine.Controller, log *slog.Logger) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("API local %s: %w", addr, err)
	}
	if ta, ok := ln.Addr().(*net.TCPAddr); !ok || !ta.IP.IsLoopback() {
		ln.Close()
		return fmt.Errorf("API local: %s no es una dirección loopback", addr)
	}
	srv := &http.Server{Handler: Handler(c), ReadHeaderTimeout: 5 * time.Second}
	go func() { <-ctx.Done(); srv.Close() }()
	log.Info("API local de control", "escucha", ln.Addr().String())
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func Handler(c engine.Controller) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) { reply(w)(c.Status()) })
	mux.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) { reply(w)(c.Config()) })
	mux.HandleFunc("PUT /api/config", func(w http.ResponseWriter, r *http.Request) {
		var a config.App
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&a); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		reply(w)(struct{}{}, c.SaveConfig(a))
	})
	mux.HandleFunc("GET /api/devices", func(w http.ResponseWriter, r *http.Request) { reply(w)(c.LocalDevices()) })
	mux.HandleFunc("GET /api/discover", func(w http.ResponseWriter, r *http.Request) { reply(w)(c.Discover()) })
	mux.HandleFunc("GET /api/logs", func(w http.ResponseWriter, r *http.Request) {
		after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
		reply(w)(c.Logs(after))
	})
	mux.HandleFunc("POST /api/registry/test", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ URL, Token string }
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		reply(w)(struct{}{}, c.TestRegistry(in.URL, in.Token))
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(header) != "1" {
			http.Error(w, "falta la cabecera "+header, http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func reply(w http.ResponseWriter) func(any, error) {
	return func(v any, err error) {
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(v)
	}
}

// Remote implementa engine.Controller contra la API de un servicio.
type Remote struct {
	base string
	hc   *http.Client
}

func NewRemote(addr string) *Remote {
	return &Remote{base: "http://" + addr, hc: &http.Client{Timeout: 20 * time.Second}}
}

// Ping indica si hay un servicio USBnetShare escuchando en la dirección.
func (r *Remote) Ping() bool {
	hc := &http.Client{Timeout: 700 * time.Millisecond}
	req, _ := http.NewRequest("GET", r.base+"/api/status", nil)
	req.Header.Set(header, "1")
	resp, err := hc.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (r *Remote) do(method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, r.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set(header, "1")
	resp, err := r.hc.Do(req)
	if err != nil {
		return fmt.Errorf("no se puede contactar con el servicio USBnetShare: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct{ Error string }
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("servicio: %s: %s", resp.Status, bytes.TrimSpace(b))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (r *Remote) Status() (s engine.Status, err error) {
	err = r.do("GET", "/api/status", nil, &s)
	return
}

func (r *Remote) Config() (a config.App, err error) {
	err = r.do("GET", "/api/config", nil, &a)
	return
}

func (r *Remote) SaveConfig(a config.App) error { return r.do("PUT", "/api/config", a, nil) }

func (r *Remote) LocalDevices() (d []engine.LocalDevice, err error) {
	err = r.do("GET", "/api/devices", nil, &d)
	return
}

func (r *Remote) Discover() (s []engine.RemoteServer, err error) {
	err = r.do("GET", "/api/discover", nil, &s)
	return
}

func (r *Remote) Logs(after uint64) (l []logx.Line, err error) {
	err = r.do("GET", "/api/logs?after="+strconv.FormatUint(after, 10), nil, &l)
	return
}

func (r *Remote) TestRegistry(url, token string) error {
	return r.do("POST", "/api/registry/test", map[string]string{"URL": url, "Token": token}, nil)
}
