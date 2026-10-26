// winres genera el recurso de Windows de USBnetShare.exe (icono e
// información de versión) a partir de logo/usbnetshare256.png. Lo usa
// `make icon`; el resultado (cmd/usbnetshare/rsrc_windows_amd64.syso) está en
// git, así que solo hace falta ejecutarlo si cambia el logo.
//
//	go run ./tools/winres
//
// Necesita windres (MSYS2: mingw-w64-x86_64-binutils, ya incluido con gcc).
// Solo usa la biblioteca estándar: reduce el PNG a varios tamaños, monta un
// .ico con ellos y lo compila con windres.
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
)

// Wails carga el icono de la ventana con el identificador de recurso 3
// (winc.AppIconID); el Explorador de Windows usa el primer grupo de iconos.
const iconID = 3

var sizes = []int{16, 24, 32, 48, 64, 128, 256}

func main() {
	src := flag.String("png", filepath.Join("logo", "usbnetshare256.png"), "logo de 256×256")
	out := flag.String("out", filepath.Join("cmd", "usbnetshare", "rsrc_windows_amd64.syso"), "recurso generado")
	flag.Parse()

	f, err := os.Open(*src)
	if err != nil {
		fail(err)
	}
	img, err := png.Decode(f)
	f.Close()
	if err != nil {
		fail(err)
	}
	if b := img.Bounds(); b.Dx() != 256 || b.Dy() != 256 {
		fail(fmt.Errorf("%s debe ser de 256×256 (es %d×%d)", *src, b.Dx(), b.Dy()))
	}

	tmp, err := os.MkdirTemp("", "winres-")
	if err != nil {
		fail(err)
	}
	defer os.RemoveAll(tmp)

	ico := filepath.Join(tmp, "app.ico")
	if err := os.WriteFile(ico, buildICO(img), 0o644); err != nil {
		fail(err)
	}
	rc := filepath.Join(tmp, "app.rc")
	if err := os.WriteFile(rc, []byte(resourceScript("app.ico")), 0o644); err != nil {
		fail(err)
	}
	abs, _ := filepath.Abs(*out)
	cmd := exec.Command("windres", "-F", "pe-x86-64", "-O", "coff", "-i", "app.rc", "-o", abs)
	cmd.Dir = tmp
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		fail(fmt.Errorf("windres: %w (¿está C:\\msys64\\mingw64\\bin en el PATH?)", err))
	}
	fmt.Println("generado:", *out)
}

// buildICO monta un .ico con una imagen PNG por tamaño (formato admitido
// desde Windows Vista).
func buildICO(img image.Image) []byte {
	var pngs [][]byte
	for _, s := range sizes {
		var buf bytes.Buffer
		if err := png.Encode(&buf, shrink(img, s)); err != nil {
			fail(err)
		}
		pngs = append(pngs, buf.Bytes())
	}
	var out bytes.Buffer
	le := binary.LittleEndian
	_ = binary.Write(&out, le, [3]uint16{0, 1, uint16(len(sizes))}) // reservado, tipo icono, nº de imágenes
	offset := 6 + 16*len(sizes)
	for i, s := range sizes {
		dim := byte(s)
		if s == 256 {
			dim = 0 // 0 significa 256
		}
		out.Write([]byte{dim, dim, 0, 0})                                           // ancho, alto, paleta, reservado
		_ = binary.Write(&out, le, [2]uint16{1, 32})                                // planos, bits por píxel
		_ = binary.Write(&out, le, [2]uint32{uint32(len(pngs[i])), uint32(offset)}) // tamaño, posición
		offset += len(pngs[i])
	}
	for _, p := range pngs {
		out.Write(p)
	}
	return out.Bytes()
}

// shrink reduce la imagen de 256×256 a size×size promediando cada bloque de
// píxeles, ponderando por la transparencia para no oscurecer los bordes.
func shrink(img image.Image, size int) image.Image {
	if size == 256 {
		return img
	}
	k := 256 / size
	dst := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var r, g, b, a uint64
			for dy := 0; dy < k; dy++ {
				for dx := 0; dx < k; dx++ {
					c := color.NRGBAModel.Convert(img.At(x*k+dx, y*k+dy)).(color.NRGBA)
					aa := uint64(c.A)
					r += uint64(c.R) * aa
					g += uint64(c.G) * aa
					b += uint64(c.B) * aa
					a += aa
				}
			}
			if a == 0 {
				continue
			}
			n := uint64(k * k)
			dst.SetNRGBA(x, y, color.NRGBA{R: uint8(r / a), G: uint8(g / a), B: uint8(b / a), A: uint8(a / n)})
		}
	}
	return dst
}

func resourceScript(ico string) string {
	return fmt.Sprintf(`%d ICON "%s"

1 VERSIONINFO
FILEOS 0x40004
FILETYPE 0x1
BEGIN
  BLOCK "StringFileInfo"
  BEGIN
    BLOCK "0C0A04B0"
    BEGIN
      VALUE "ProductName", "USBnetShare"
      VALUE "FileDescription", "USBnetShare: comparte y usa dispositivos USB por la red"
      VALUE "OriginalFilename", "USBnetShare.exe"
      VALUE "InternalName", "USBnetShare"
    END
  END
  BLOCK "VarFileInfo"
  BEGIN
    VALUE "Translation", 0x0C0A, 1200
  END
END
`, iconID, ico)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "winres:", err)
	os.Exit(1)
}
