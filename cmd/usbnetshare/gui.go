//go:build desktop

package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"

	"github.com/brokensaw/usbnetshare/internal/api"
	"github.com/brokensaw/usbnetshare/internal/appinfo"
	"github.com/brokensaw/usbnetshare/internal/config"
	"github.com/brokensaw/usbnetshare/internal/drivers"
	"github.com/brokensaw/usbnetshare/internal/engine"
	"github.com/brokensaw/usbnetshare/internal/logx"
	"github.com/brokensaw/usbnetshare/internal/svc"
)

//go:embed all:frontend
var assets embed.FS

// App es lo que ve la página (window.go.main.App). Si hay un servicio
// USBnetShare en marcha lo controla por la API local; si no, ejecuta el motor
// dentro de la propia ventana ("modo local").
type App struct {
	cfgPath string
	api     string
	local   *engine.Engine
	log     *slog.Logger

	mu       sync.Mutex
	ctl      engine.Controller
	mode     string // "servicio" | "local"
	stopProc context.CancelFunc
	procDone chan struct{}
}

// UIStatus añade al estado del motor el modo de ejecución.
type UIStatus struct {
	engine.Status
	Mode string `json:"mode"`
}

func runGUI(cfgPath string, cfg *config.App, e *engine.Engine, log *slog.Logger) error {
	a := &App{cfgPath: cfgPath, api: cfg.API, local: e, log: log}
	a.chooseMode()
	defer a.stopLocal()
	return wails.Run(&options.App{
		Title:     appinfo.Name,
		Width:     1040,
		Height:    720,
		MinWidth:  760,
		MinHeight: 520,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		SingleInstanceLock: &options.SingleInstanceLock{UniqueId: "usbnetshare-gui-6f1c2b7e"},
		Bind:               []interface{}{a},
		Windows:            &windows.Options{Theme: windows.SystemDefault},
	})
}

func (a *App) chooseMode() {
	r := api.NewRemote(a.api)
	a.mu.Lock()
	defer a.mu.Unlock()
	if r.Ping() {
		a.ctl, a.mode = r, "servicio"
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = a.local.Run(ctx) }()
	a.ctl, a.mode, a.stopProc, a.procDone = a.local, "local", cancel, done
}

func (a *App) stopLocal() {
	a.mu.Lock()
	cancel, done := a.stopProc, a.procDone
	a.stopProc, a.procDone = nil, nil
	a.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

func (a *App) controller() engine.Controller {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ctl
}

func (a *App) GetStatus() (UIStatus, error) {
	st, err := a.controller().Status()
	a.mu.Lock()
	mode := a.mode
	a.mu.Unlock()
	return UIStatus{Status: st, Mode: mode}, err
}

func (a *App) GetConfig() (config.App, error)              { return a.controller().Config() }
func (a *App) SaveConfig(c config.App) error               { return a.controller().SaveConfig(c) }
func (a *App) LocalDevices() ([]engine.LocalDevice, error) { return a.controller().LocalDevices() }
func (a *App) Discover() ([]engine.RemoteServer, error)    { return a.controller().Discover() }
func (a *App) Logs(after uint64) ([]logx.Line, error)      { return a.controller().Logs(after) }

// TestRegistry prueba la URL y el token del registro sin guardarlos.
func (a *App) TestRegistry(url, token string) error { return a.controller().TestRegistry(url, token) }

// InstallService instala USBnetShare como servicio de Windows (pide permisos de
// administrador) y pasa a controlarlo. El motor local se para antes para
// liberar puertos y dispositivos.
func (a *App) InstallService() error {
	a.stopLocal()
	if err := runElevated("-service", "install", "-config", a.cfgPath); err != nil {
		a.chooseMode()
		return err
	}
	r := api.NewRemote(a.api)
	for i := 0; i < 40 && !r.Ping(); i++ {
		time.Sleep(250 * time.Millisecond)
	}
	if !r.Ping() {
		a.chooseMode()
		return errors.New("el servicio se instaló pero no responde; revisa " + appinfo.ID + ".log")
	}
	a.mu.Lock()
	a.ctl, a.mode = r, "servicio"
	a.mu.Unlock()
	return nil
}

// AllowFirewall añade la regla del Firewall de Windows (pide administrador).
func (a *App) AllowFirewall() error { return runElevated("-firewall", "add") }

// LegacyService indica si sigue instalado el servicio de USBnetShare (versiones
// anteriores): la GUI ofrece sustituirlo con «Instalar como servicio».
func (a *App) LegacyService() bool { return svc.Installed(legacyService) }

// DriversBundled indica si este ejecutable lleva los instaladores de drivers.
func (a *App) DriversBundled() bool { return drivers.Bundled() }

// InstallDrivers instala los drivers indicados ("usbip", "usbdk") con
// permisos de administrador. Devuelve un aviso si hay que reiniciar Windows.
// En modo local reinicia el motor para que el servidor empiece a usar UsbDk
// (en modo servicio lo reinicia el propio proceso elevado).
func (a *App) InstallDrivers(ids []string) (string, error) {
	if len(ids) == 0 {
		return "", errors.New("no hay ningún driver seleccionado")
	}
	err := runElevated("-install-drivers", strings.Join(ids, ","), "-config", a.cfgPath)
	var ee *exec.ExitError
	reboot := errors.As(err, &ee) && ee.ExitCode() == drivers.RebootExitCode
	if err != nil && !reboot {
		return "", err
	}
	a.mu.Lock()
	local := a.mode == "local"
	a.mu.Unlock()
	if local {
		a.stopLocal()
		a.chooseMode()
	}
	if reboot {
		return "Drivers instalados. Reinicia Windows para terminar la instalación.", nil
	}
	return "Drivers instalados.", nil
}

// UninstallService quita el servicio y vuelve al modo local.
func (a *App) UninstallService() error {
	if err := runElevated("-service", "uninstall", "-config", a.cfgPath); err != nil {
		return err
	}
	// La configuración pudo cambiar a través del servicio: se recarga.
	if err := a.local.Reload(); err != nil {
		return fmt.Errorf("servicio desinstalado, pero no se pudo recargar la configuración: %w", err)
	}
	a.chooseMode()
	return nil
}
