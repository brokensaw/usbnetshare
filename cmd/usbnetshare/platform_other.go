//go:build !windows

package main

import (
	"errors"
	"os"
	"os/exec"
)

func attachConsole()   {}
func showError(string) {}

// runElevated usa pkexec (aviso gráfico de contraseña) si está disponible.
func runElevated(args ...string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if _, err := exec.LookPath("pkexec"); err != nil {
		return errors.New("ejecuta como root: sudo " + exe + " -service install")
	}
	return exec.Command("pkexec", append([]string{exe}, args...)...).Run()
}

// firewall no hace nada fuera de Windows: abre TCP 3240 y UDP 3241 con el
// firewall de tu distribución (ufw, firewalld...).
func firewall(string) error { return nil }
