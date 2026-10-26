//go:build linux

package client

// En Linux el socket ya negociado se entrega al módulo vhci-hcd, que crea un
// dispositivo USB virtual idéntico al remoto (igual que "usbip attach").

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/brokensaw/usbnetshare/internal/config"
	"github.com/brokensaw/usbnetshare/internal/protocol"
)

const (
	vhciDir     = "/sys/devices/platform/vhci_hcd.0"
	vdevStNull  = 4
	vdevStUsed  = 6
	vdevStError = 7
)

type linuxAttacher struct {
	cfg *config.Client
	log *slog.Logger
	mu  sync.Mutex // evita que dos reglas elijan el mismo puerto libre
}

func newAttacher(cfg *config.Client, log *slog.Logger) (Attacher, error) {
	return &linuxAttacher{cfg: cfg, log: log}, nil
}

func (a *linuxAttacher) Prepare() error {
	if out, err := exec.Command("modprobe", "vhci-hcd").CombinedOutput(); err != nil {
		a.log.Warn("modprobe vhci-hcd falló", "err", err, "salida", strings.TrimSpace(string(out)))
	}
	if _, err := os.Stat(filepath.Join(vhciDir, "attach")); err != nil {
		return fmt.Errorf("vhci-hcd no disponible (instala usbip / linux-tools y ejecuta como root): %w", err)
	}
	return nil
}

type vhciPort struct {
	hub    string // "hs" o "ss"
	port   int
	status int
}

func vhciPorts() ([]vhciPort, error) {
	files, _ := filepath.Glob("/sys/devices/platform/vhci_hcd.*/status*")
	if len(files) == 0 {
		return nil, fmt.Errorf("no se encuentra el estado de vhci_hcd")
	}
	var out []vhciPort
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			fs := strings.Fields(line)
			// formato: hub port sta spd dev sockfd local_busid
			if len(fs) < 3 || (fs[0] != "hs" && fs[0] != "ss") {
				continue
			}
			p, err1 := strconv.Atoi(fs[1])
			st, err2 := strconv.Atoi(fs[2])
			if err1 != nil || err2 != nil {
				continue
			}
			out = append(out, vhciPort{hub: fs[0], port: p, status: st})
		}
	}
	return out, nil
}

func freePort(speed uint32) (int, error) {
	ports, err := vhciPorts()
	if err != nil {
		return 0, err
	}
	want := "hs"
	if speed >= protocol.SpeedSuper {
		want = "ss"
	}
	for _, p := range ports {
		if p.hub == want && p.status == vdevStNull {
			return p.port, nil
		}
	}
	return 0, fmt.Errorf("no quedan puertos vhci libres (%s)", want)
}

func (a *linuxAttacher) Attach(ctx context.Context, host string, port int, dev protocol.Device) (Attachment, error) {
	conn, d, err := ImportHandshake(ctx, host, port, dev.BusID, a.cfg.KeepAlive)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	a.mu.Lock()
	defer a.mu.Unlock()
	vport, err := freePort(d.Speed)
	if err != nil {
		return nil, err
	}
	f, err := conn.File()
	if err != nil {
		return nil, err
	}
	defer f.Close() // el kernel mantiene su propia referencia
	line := fmt.Sprintf("%d %d %d %d", vport, f.Fd(), d.DevID(), d.Speed)
	if err := os.WriteFile(filepath.Join(vhciDir, "attach"), []byte(line), 0o200); err != nil {
		return nil, fmt.Errorf("vhci attach (%s): %w", line, err)
	}
	return &linuxAttachment{port: vport}, nil
}

type linuxAttachment struct{ port int }

func (l *linuxAttachment) String() string { return fmt.Sprintf("vhci:%d", l.port) }

func (l *linuxAttachment) Alive() (bool, error) {
	ports, err := vhciPorts()
	if err != nil {
		return false, err
	}
	for _, p := range ports {
		if p.port == l.port {
			return p.status == vdevStUsed, nil
		}
	}
	return false, nil
}

func (l *linuxAttachment) Detach() error {
	alive, _ := l.Alive()
	if !alive {
		return nil
	}
	return os.WriteFile(filepath.Join(vhciDir, "detach"), []byte(strconv.Itoa(l.port)), 0o200)
}

// DriverInfo resume el estado del controlador virtual para la GUI.
func DriverInfo() string {
	if _, err := os.Stat(filepath.Join(vhciDir, "attach")); err != nil {
		return "vhci-hcd no cargado (instala usbip / linux-tools y ejecuta como root)"
	}
	return "vhci-hcd (kernel) · disponible"
}
