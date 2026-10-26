// Package logx prepara el logger (consola + fichero opcional + memoria para
// mostrarlo en la interfaz gráfica).
package logx

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/kardianos/service"
)

// New crea un logger. Si se ejecuta como servicio y no hay fichero
// configurado, escribe en defaultFile (en Windows un servicio no tiene
// consola visible). Las líneas se guardan también en el Ring devuelto.
func New(logFile, level, defaultFile string) (*slog.Logger, *Ring, io.Closer) {
	ring := NewRing(500)
	ws := []io.Writer{os.Stderr, ring}
	var closer io.Closer = io.NopCloser(nil)
	if logFile == "" && !service.Interactive() {
		logFile = defaultFile
	}
	if logFile != "" {
		_ = os.MkdirAll(filepath.Dir(logFile), 0o755)
		if f, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
			ws = append(ws, f)
			closer = f
		}
	}
	lvl := slog.LevelInfo
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	h := slog.NewTextHandler(io.MultiWriter(ws...), &slog.HandlerOptions{Level: lvl})
	return slog.New(h), ring, closer
}

// Line es una línea de log numerada.
type Line struct {
	Seq  uint64 `json:"seq"`
	Text string `json:"text"`
}

// Ring guarda las últimas N líneas. slog.TextHandler hace una sola llamada
// a Write por registro, así que cada Write es una línea completa.
type Ring struct {
	mu   sync.Mutex
	buf  []Line
	max  int
	next uint64
}

func NewRing(max int) *Ring { return &Ring{max: max, next: 1} }

func (r *Ring) Write(p []byte) (int, error) {
	text := strings.TrimRight(string(p), "\r\n")
	r.mu.Lock()
	r.buf = append(r.buf, Line{Seq: r.next, Text: text})
	r.next++
	if len(r.buf) > r.max {
		r.buf = append(r.buf[:0:0], r.buf[len(r.buf)-r.max:]...)
	}
	r.mu.Unlock()
	return len(p), nil
}

// Since devuelve las líneas con Seq > after.
func (r *Ring) Since(after uint64) []Line {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []Line{}
	for _, l := range r.buf {
		if l.Seq > after {
			out = append(out, l)
		}
	}
	return out
}
