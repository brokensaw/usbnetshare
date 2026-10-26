// fetch descarga los instaladores de drivers que se incrustan en USBnetShare.exe
// (go build -tags drivers) y comprueba su SHA-256. Lo usa `make drivers`.
//
//	go run ./internal/drivers/fetch [-out internal/drivers/payload]
package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/brokensaw/usbnetshare/internal/drivers"
)

func main() {
	out := flag.String("out", filepath.Join("internal", "drivers", "payload"), "carpeta destino")
	flag.Parse()
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fail(err)
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	for _, p := range drivers.Packages {
		dst := filepath.Join(*out, p.File)
		if b, err := os.ReadFile(dst); err == nil && p.Verify(b) == nil {
			fmt.Println("ya descargado:", p.File)
			continue
		}
		fmt.Println("descargando", p.URL)
		resp, err := client.Get(p.URL)
		if err != nil {
			fail(err)
		}
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			fail(err)
		}
		if resp.StatusCode != http.StatusOK {
			fail(fmt.Errorf("%s: HTTP %s", p.URL, resp.Status))
		}
		if err := p.Verify(b); err != nil {
			fail(err)
		}
		if err := os.WriteFile(dst, b, 0o644); err != nil {
			fail(err)
		}
		fmt.Println("ok:", p.File)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "fetch:", err)
	os.Exit(1)
}
