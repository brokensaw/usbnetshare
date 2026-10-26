//go:build windows

package drivers

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// bundle contiene payload/ si se compiló con -tags drivers.
var bundle fs.FS

// Bundled indica si este USBnetShare.exe lleva los instaladores dentro.
func Bundled() bool {
	if bundle == nil {
		return false
	}
	for _, p := range Packages {
		if _, err := fs.Stat(bundle, "payload/"+p.File); err != nil {
			return false
		}
	}
	return true
}

// Install instala en silencio los paquetes indicados ("usbip", "usbdk").
// Requiere administrador. Deja el registro de cada instalador en logDir.
// reboot indica que Windows pide reiniciar para terminar.
func Install(ids []string, logDir string) (reboot bool, err error) {
	if !Bundled() {
		return false, errors.New("este USBnetShare.exe no incluye los instaladores de drivers (compílalo con make drivers windows)")
	}
	var pkgs []Package
	for _, id := range ids {
		p, ok := Find(id)
		if !ok {
			return false, fmt.Errorf("driver desconocido %q (usbip | usbdk)", id)
		}
		pkgs = append(pkgs, p)
	}
	// UsbDk primero: usbip-win2 reinicia los hubs USB 3.0 al instalarse.
	if len(pkgs) == 2 && pkgs[0].ID == "usbip" {
		pkgs[0], pkgs[1] = pkgs[1], pkgs[0]
	}

	dir, err := privateTempDir()
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(dir)

	for _, p := range pkgs {
		r, err := installOne(p, dir, logDir)
		if err != nil {
			return reboot, err
		}
		reboot = reboot || r
	}
	return reboot, nil
}

func installOne(p Package, dir, logDir string) (reboot bool, err error) {
	data, err := fs.ReadFile(bundle, "payload/"+p.File)
	if err != nil {
		return false, err
	}
	if err := p.Verify(data); err != nil {
		return false, err
	}
	path := filepath.Join(dir, p.File)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return false, err
	}
	logFile := filepath.Join(logDir, "install-"+p.ID+".log")

	var cmd *exec.Cmd
	switch p.ID {
	case "usbip":
		// Inno Setup. Solo cliente (sin GUI ni SDK) y el runtime de Visual C++
		// que necesita devnode.exe durante la instalación.
		cmd = exec.Command(path, "/VERYSILENT", "/SUPPRESSMSGBOXES", "/NORESTART",
			fmt.Sprintf("/RESTARTEXITCODE=%d", RebootExitCode),
			"/COMPONENTS=main,client", "/TASKS=vcredist", "/LOG="+logFile)
	case "usbdk":
		msiexec := filepath.Join(os.Getenv("SystemRoot"), "System32", "msiexec.exe")
		cmd = exec.Command(msiexec, "/i", path, "/qn", "/norestart", "/l*v", logFile)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	err = cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		switch ee.ExitCode() {
		case RebootExitCode, 1641: // reinicio necesario / iniciado
			return true, nil
		case 1638: // MSI: ya hay otra versión instalada
			return false, nil
		}
		return false, fmt.Errorf("%s %s: el instalador terminó con código %d (detalles en %s)", p.Name, p.Version, ee.ExitCode(), logFile)
	}
	if err != nil {
		return false, fmt.Errorf("%s %s: %w", p.Name, p.Version, err)
	}
	return false, nil
}

// privateTempDir crea una carpeta en %SystemRoot%\Temp accesible solo para
// SYSTEM y Administradores: el usuario no puede cambiar el instalador entre
// que se escribe y se ejecuta como administrador.
func privateTempDir() (string, error) {
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)")
	if err != nil {
		return "", err
	}
	sa := &windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	rnd := make([]byte, 8)
	_, _ = rand.Read(rnd)
	dir := filepath.Join(os.Getenv("SystemRoot"), "Temp", "USBnetShare-drivers-"+hex.EncodeToString(rnd))
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return "", err
	}
	if err := windows.CreateDirectory(p, sa); err != nil {
		return "", fmt.Errorf("creando %s: %w", dir, err)
	}
	return dir, nil
}
