//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"

	"github.com/brokensaw/usbnetshare/internal/appinfo"
)

var procAttachConsole = windows.NewLazySystemDLL("kernel32.dll").NewProc("AttachConsole")

// attachConsole conecta la salida a la consola del proceso padre (si se
// lanzó desde un terminal), porque el .exe está compilado como app gráfica.
func attachConsole() {
	const attachParentProcess = ^uintptr(0)
	if r, _, _ := procAttachConsole.Call(attachParentProcess); r == 0 {
		return
	}
	if f, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0); err == nil {
		os.Stdout, os.Stderr = f, f
	}
}

func showError(msg string) {
	if len(os.Args) > 1 {
		return // modo consola: ya se escribió en stderr
	}
	t, _ := windows.UTF16PtrFromString(appinfo.Name)
	m, _ := windows.UTF16PtrFromString(msg)
	windows.MessageBox(0, m, t, windows.MB_OK|windows.MB_ICONERROR)
}

// runElevated ejecuta este mismo programa como administrador (aparece el
// aviso de UAC) y espera a que termine.
func runElevated(args ...string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	var qa []string
	for _, a := range args {
		qa = append(qa, quote(a))
	}
	ps := fmt.Sprintf("$p = Start-Process -FilePath %s -ArgumentList %s -Verb RunAs -Wait -PassThru -WindowStyle Hidden; exit $p.ExitCode",
		quote(exe), strings.Join(qa, ","))
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", ps)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "cancel") {
			return fmt.Errorf("operación cancelada en el aviso de administrador")
		}
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// firewallRule es la regla de entrada; legacyFirewallRule, la de usbnet.
const (
	firewallRule       = appinfo.Name
	legacyFirewallRule = "USBNet"
)

// firewall añade ("add") o quita ("remove") la regla de entrada del Firewall
// de Windows para este ejecutable, en todos los perfiles. Cubre TCP 3240
// (servidor), UDP 3241 (descubrimiento) y las respuestas que recibe el
// cliente al buscar servidores. Requiere administrador.
func firewall(action string) error {
	if action != "add" && action != "remove" {
		return fmt.Errorf("acción %q no válida (add | remove)", action)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	netsh := func(args ...string) (string, error) {
		cmd := exec.Command("netsh", append([]string{"advfirewall", "firewall"}, args...)...)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	_, _ = netsh("delete", "rule", "name="+firewallRule)       // idempotente
	_, _ = netsh("delete", "rule", "name="+legacyFirewallRule) // la de usbnet, si queda
	if action == "remove" {
		return nil
	}
	if out, err := netsh("add", "rule", "name="+firewallRule, "dir=in", "action=allow",
		"program="+exe, "enable=yes", "profile=any",
		"description="+appinfo.Name+": USB por red (TCP 3240, UDP 3241)"); err != nil {
		return fmt.Errorf("netsh: %v: %s", err, out)
	}
	return nil
}
