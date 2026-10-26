// USBnetShare: comparte dispositivos USB por red (USB/IP) y/o los usa como si
// fueran locales. Una sola aplicación con rol servidor y rol cliente.
//
//	USBnetShare                      abre la interfaz gráfica
//	USBnetShare -headless            ejecuta sin ventana (lo que usa el servicio)
//	USBnetShare -service install     instala y arranca el servicio del sistema
//	USBnetShare -list | -discover    utilidades de consola
//	USBnetShare -install-drivers usbip,usbdk   instala los drivers de Windows incluidos
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/brokensaw/usbnetshare/internal/api"
	"github.com/brokensaw/usbnetshare/internal/appinfo"
	"github.com/brokensaw/usbnetshare/internal/config"
	"github.com/brokensaw/usbnetshare/internal/drivers"
	"github.com/brokensaw/usbnetshare/internal/engine"
	"github.com/brokensaw/usbnetshare/internal/logx"
	"github.com/brokensaw/usbnetshare/internal/svc"
)

var version = "dev"

// serviceName es el servicio del sistema ("USBnetShare"; "usbnetshare" en Linux).
var serviceName = appinfo.ID

func main() {
	cfgPath := flag.String("config", config.DefaultPath(), "fichero de configuración")
	action := flag.String("service", "", "install | uninstall | start | stop | restart")
	headless := flag.Bool("headless", false, "ejecuta sin interfaz gráfica (modo servicio)")
	list := flag.Bool("list", false, "lista los dispositivos USB locales y sale")
	disc := flag.Bool("discover", false, "busca servidores en la red, muestra sus dispositivos y sale")
	fw := flag.String("firewall", "", "add | remove: regla del firewall para "+appinfo.Name+" (requiere administrador)")
	instDrv := flag.String("install-drivers", "", "usbip,usbdk: instala en silencio los drivers de Windows incluidos (requiere administrador)")
	showVer := flag.Bool("version", false, "muestra la versión")
	flag.Parse()

	// El ejecutable de Windows se compila sin consola (es una app gráfica);
	// para los modos de consola se reutiliza la del terminal que lo lanzó.
	if *headless || *list || *disc || *showVer || *action != "" || *fw != "" || *instDrv != "" || flag.NArg() > 0 {
		attachConsole()
	}
	if *fw != "" {
		if err := firewall(*fw); err != nil {
			fatal("firewall:", err)
		}
		fmt.Println("firewall:", *fw, "OK")
		return
	}

	if *instDrv != "" {
		installDrivers(*cfgPath, *instDrv)
		return
	}

	if *showVer {
		fmt.Println(appinfo.Name, version)
		return
	}
	abs, _ := filepath.Abs(*cfgPath)
	cfg, err := config.Load(abs)
	if err != nil {
		fatal("configuración:", err)
	}
	log, ring, closer := logx.New(cfg.LogFile, cfg.LogLevel, filepath.Join(filepath.Dir(abs), appinfo.ID+".log"))
	defer closer.Close()
	e := engine.New(abs, version, cfg, log, ring)

	switch {
	case *list:
		listCmd(e)
	case *disc:
		discoverCmd(e)
	case *action != "" || *headless:
		runService(abs, cfg, e, log, *action)
	default:
		if err := runGUI(abs, cfg, e, log); err != nil {
			fatal("interfaz gráfica:", err)
		}
	}
}

func fatal(a ...any) {
	fmt.Fprintln(os.Stderr, a...)
	showError(fmt.Sprintln(a...))
	os.Exit(1)
}

func serviceSpec(cfgPath string) svc.Spec {
	return svc.Spec{
		Name:        serviceName,
		DisplayName: appinfo.Name,
		Description: "Comparte y usa dispositivos USB por red (USB/IP) con descubrimiento y reconexión automática.",
		Args:        []string{"-headless", "-config", cfgPath},
	}
}

// runService ejecuta el motor sin ventana (como servicio o en consola) junto
// con la API local que usa la interfaz gráfica; o ejecuta una acción sobre
// el servicio instalado.
func runService(cfgPath string, cfg *config.App, e *engine.Engine, log *slog.Logger, action string) {
	run := func(ctx context.Context) error {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		go func() {
			if err := api.Serve(ctx, cfg.API, e, log); err != nil {
				log.Error("la API local no está disponible; la interfaz gráfica no podrá controlar el servicio", "err", err)
			}
		}()
		return e.Run(ctx)
	}
	if action == "install" {
		removeLegacyService(log)
		// Instalar implica también arrancar: es lo que se espera desde la GUI.
		if err := svc.Run(serviceSpec(cfgPath), "install", run, log); err != nil {
			fatal("servicio install:", err)
		}
		if err := firewall("add"); err != nil {
			log.Warn("no se pudo añadir la regla del firewall", "err", err)
		}
		action = "start"
	}
	if action == "uninstall" {
		removeLegacyService(log)
		_ = svc.Run(serviceSpec(cfgPath), "stop", run, log)
		_ = firewall("remove")
	}
	if err := svc.Run(serviceSpec(cfgPath), action, run, log); err != nil {
		fatal("servicio "+action+":", err)
	}
	if action != "" {
		fmt.Printf("servicio: %s OK\n", action)
	}
}

// installDrivers instala los drivers incluidos (proceso ya elevado). Si
// USBnetShare corre como servicio lo reinicia para que el servidor use UsbDk.
// Sale con drivers.RebootExitCode si Windows pide reiniciar.
func installDrivers(cfgPath, list string) {
	abs, _ := filepath.Abs(cfgPath)
	reboot, err := drivers.Install(strings.Split(list, ","), filepath.Dir(abs))
	if err != nil {
		fatal("drivers:", err)
	}
	_ = svc.Run(serviceSpec(abs), "restart", nil, nil) // falla si no hay servicio: da igual
	fmt.Println("drivers:", list, "OK")
	if reboot {
		fmt.Println("reinicia Windows para terminar la instalación")
		os.Exit(drivers.RebootExitCode)
	}
}

// legacyService es el servicio de las versiones anteriores (usbnet).
var legacyService = svc.Spec{Name: appinfo.LegacyID}

// removeLegacyService para y desinstala el servicio de USBnetShare si sigue
// instalado: lo sustituye el de USBnetShare. Requiere administrador.
func removeLegacyService(log *slog.Logger) {
	if !svc.Installed(legacyService) {
		return
	}
	_ = svc.Run(legacyService, "stop", nil, log)
	if err := svc.Run(legacyService, "uninstall", nil, log); err != nil {
		log.Warn("no se pudo quitar el servicio antiguo de USBnetShare", "err", err)
		return
	}
	log.Info("servicio antiguo de USBnetShare sustituido por " + appinfo.Name)
}

func listCmd(e *engine.Engine) {
	devs, err := e.LocalDevices()
	if err != nil {
		fatal(err)
	}
	fmt.Printf("%-12s %-10s %-9s %s\n", "BUSID", "VID:PID", "EXPORTA", "PRODUCTO")
	for _, d := range devs {
		mark := ""
		if d.Exported {
			mark = "sí"
		}
		fmt.Printf("%-12s %s:%s  %-9s %s\n", d.BusID, d.VID, d.PID, mark, d.Product)
	}
}

func discoverCmd(e *engine.Engine) {
	found, _ := e.Discover()
	if len(found) == 0 {
		fmt.Println("No se encontró ningún servidor (¿firewall bloqueando UDP 3241? ¿otra subred?).")
	}
	for _, f := range found {
		fmt.Printf("Servidor %q en %s:%d\n", f.Name, f.Host, f.Port)
		for _, d := range f.Devices {
			st := "libre"
			if d.Busy {
				st = "en uso"
			}
			fmt.Printf("   %-12s %s:%s  %-7s %s\n", d.BusID, d.VID, d.PID, st, d.Product)
		}
	}
}
