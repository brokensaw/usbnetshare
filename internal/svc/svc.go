// Package svc permite ejecutar e instalar los binarios como servicio del
// sistema (systemd en Linux, Servicio de Windows) con reinicio automático.
package svc

import (
	"context"
	"log/slog"
	"runtime"
	"sync"
	"time"

	"github.com/kardianos/service"
)

type Runner func(ctx context.Context) error

type program struct {
	run    Runner
	log    *slog.Logger
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func (p *program) Start(s service.Service) error {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		// Persistencia a nivel de proceso: si el núcleo termina con error
		// (p.ej. puerto ocupado, driver aún no cargado al arrancar), se reinicia.
		for ctx.Err() == nil {
			err := p.run(ctx)
			if ctx.Err() != nil {
				return
			}
			p.log.Error("el servicio terminó inesperadamente; reiniciando en 5 s", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
			}
		}
	}()
	return nil
}

func (p *program) Stop(s service.Service) error {
	p.cancel()
	done := make(chan struct{})
	go func() { p.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		p.log.Warn("parada forzada por timeout")
	}
	return nil
}

type Spec struct {
	Name, DisplayName, Description string
	Args                           []string
}

// Run ejecuta la acción indicada: "" (ejecutar), install, uninstall, start,
// stop, restart.
func Run(spec Spec, action string, r Runner, log *slog.Logger) error {
	cfg := &service.Config{
		Name:        spec.Name,
		DisplayName: spec.DisplayName,
		Description: spec.Description,
		Arguments:   spec.Args,
		Option: service.KeyValue{
			"Restart":                "always", // systemd
			"OnFailure":              "restart",
			"OnFailureDelayDuration": "5s",
			"DelayedAutoStart":       true,
		},
	}
	if runtime.GOOS == "linux" {
		cfg.Dependencies = []string{"After=network-online.target", "Wants=network-online.target"}
	} else if runtime.GOOS == "windows" {
		cfg.Dependencies = []string{"Tcpip"}
	}
	prg := &program{run: r, log: log}
	s, err := service.New(prg, cfg)
	if err != nil {
		return err
	}
	if action != "" {
		return service.Control(s, action)
	}
	return s.Run()
}

// Installed indica si el servicio está instalado en el sistema.
func Installed(spec Spec) bool {
	s, err := service.New(&program{}, &service.Config{Name: spec.Name})
	if err != nil {
		return false
	}
	_, err = s.Status()
	return err == nil
}
