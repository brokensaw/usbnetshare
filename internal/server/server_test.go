package server

import (
	"context"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	"github.com/brokensaw/usbnetshare/internal/client"
	"github.com/brokensaw/usbnetshare/internal/config"
	"github.com/brokensaw/usbnetshare/internal/discovery"
	"github.com/brokensaw/usbnetshare/internal/protocol"
)

// Backend simulado: un "PC Adapter" falso cuyo Serve mantiene la sesión
// hasta que el contexto se cancela o el cliente cierra.
type fakeBackend struct{ busy map[string]bool }

var fakeDev = protocol.Device{BusID: "1-1", BusNum: 1, DevNum: 3, Speed: protocol.SpeedFull,
	IDVendor: 0x0908, IDProduct: 0x0004, ConfigurationValue: 1, NumConfigurations: 1,
	Interfaces: []protocol.Interface{{0xff, 0, 0}}, Product: "PC Adapter (fake)"}

func (f *fakeBackend) Name() string                                               { return "fake" }
func (f *fakeBackend) List() ([]protocol.Device, error)                           { return []protocol.Device{fakeDev}, nil }
func (f *fakeBackend) Maintain(ctx context.Context, _ func(protocol.Device) bool) { <-ctx.Done() }
func (f *fakeBackend) Close() error                                               { return nil }
func (f *fakeBackend) Open(busid string, force bool) (Session, error)             { return &fakeSess{}, nil }

type fakeSess struct{}

func (s *fakeSess) Device() protocol.Device { return fakeDev }
func (s *fakeSess) Abort()                  {}
func (s *fakeSess) Serve(ctx context.Context, c *net.TCPConn) error {
	defer c.Close()
	go func() { <-ctx.Done(); c.Close() }()
	buf := make([]byte, 1)
	_, err := c.Read(buf)
	return err
}

func TestEndToEnd(t *testing.T) {
	register("fake", func(*slog.Logger) (Backend, error) { return &fakeBackend{}, nil })
	on := true // este test prueba justamente el descubrimiento por difusión
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := &config.Server{
		Name: "taller-pi", Listen: ":43240", Backend: "fake",
		Export:    []config.Match{{VID: "0908"}},
		Discovery: config.Discovery{Enabled: &on, Port: 43241, Group: config.DefaultDiscGroup, Key: "secreto"},
		KeepAlive: config.KeepAlive{Idle: time.Second, Interval: time.Second, Count: 3, UserTimeout: 5 * time.Second},
	}
	srv, err := New(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Run(ctx)
	time.Sleep(300 * time.Millisecond)

	// 1) descubrimiento por nombre (sin IP)
	found, _ := discovery.Discover(ctx, discovery.Options{Port: 43241, Group: config.DefaultDiscGroup,
		Key: []byte("secreto"), Name: "taller-pi", Timeout: 1500 * time.Millisecond})
	if len(found) != 1 || found[0].Port != 43240 || len(found[0].Devices) != 1 {
		t.Fatalf("descubrimiento: %+v", found)
	}
	// clave equivocada: el servidor no debe responder
	bad, _ := discovery.Discover(ctx, discovery.Options{Port: 43241, Group: config.DefaultDiscGroup,
		Key: []byte("otra"), Timeout: 800 * time.Millisecond})
	if len(bad) != 0 {
		t.Fatalf("respondió con clave incorrecta: %+v", bad)
	}
	host := found[0].Host

	// 2) devlist
	devs, err := client.Devlist(ctx, host, 43240, cfg.KeepAlive)
	if err != nil || len(devs) != 1 || devs[0].IDVendor != 0x0908 {
		t.Fatalf("devlist: %v %+v", err, devs)
	}

	// 3) import
	c1, d, err := client.ImportHandshake(ctx, host, 43240, "1-1", cfg.KeepAlive)
	if err != nil || d.DevID() != fakeDev.DevID() {
		t.Fatalf("import: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	found, _ = discovery.Discover(ctx, discovery.Options{Port: 43241, Group: config.DefaultDiscGroup,
		Key: []byte("secreto"), Timeout: 1500 * time.Millisecond})
	if len(found) != 1 || !found[0].Devices[0].Busy {
		t.Fatalf("debería figurar como ocupado: %+v", found)
	}

	// 4) simulamos que el cliente perdió la red sin cerrar (c1 sigue abierto)
	//    y se reconecta: el takeover debe cerrar la sesión huérfana.
	c2, _, err := client.ImportHandshake(ctx, host, 43240, "1-1", cfg.KeepAlive)
	if err != nil {
		t.Fatalf("takeover: %v", err)
	}
	defer c2.Close()
	_ = c1.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c1.Read(make([]byte, 1)); err == nil {
		t.Fatal("la sesión antigua debería estar cerrada")
	}

	// 5) import de un busid inexistente
	if _, _, err := client.ImportHandshake(ctx, host, 43240, "9-9", cfg.KeepAlive); err == nil {
		t.Fatal("debería fallar")
	}
}
