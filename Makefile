VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
OUT := dist

.PHONY: all windows drivers icon linux registry registry-image test clean

all: windows linux registry

# USBnetShare.exe para Windows: GUI (Wails) + servidor (libusb, cgo) + cliente,
# con los instaladores de usbip-win2 y UsbDk dentro (Ajustes → Drivers de Windows).
# Compilar en Windows con MSYS2 en el PATH (C:\msys64\mingw64\bin):
#   pacman -S mingw-w64-x86_64-gcc mingw-w64-x86_64-libusb mingw-w64-x86_64-pkg-config
# Copiar libusb-1.0.dll junto al .exe (las licencias de terceros se copian solas).
windows: drivers
	CGO_ENABLED=1 go build -tags "desktop,production,libusb,drivers" -ldflags "$(LDFLAGS) -H windowsgui" -o $(OUT)/windows-amd64/USBnetShare.exe ./cmd/usbnetshare
	mkdir -p $(OUT)/windows-amd64/licenses
	cp third_party/usbip-win2/LICENSE.txt $(OUT)/windows-amd64/licenses/usbip-win2-LICENSE.txt
	cp third_party/UsbDk/LICENSE $(OUT)/windows-amd64/licenses/UsbDk-LICENSE.txt

# Recurso de Windows (icono y datos del .exe) desde logo/usbnetshare256.png.
# El .syso generado está en git: solo hace falta si cambia el logo.
icon:
	go run ./tools/winres

# Descarga los instaladores oficiales a internal/drivers/payload comprobando su SHA-256.
drivers:
	go run ./internal/drivers/fetch

# usbnetshare para Linux sin interfaz gráfica (servidor en una Raspberry Pi, etc.).
linux:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o $(OUT)/linux-amd64/usbnetshare ./cmd/usbnetshare
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o $(OUT)/linux-arm64/usbnetshare ./cmd/usbnetshare

# Servidor de identidades (binario Linux, el que va dentro de Docker).
registry:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(OUT)/linux-amd64/usbnetshare-registry ./cmd/usbnetshare-registry

# Imagen Docker del servidor de identidades.
registry-image:
	docker build -f deploy/registry/Dockerfile --build-arg VERSION=$(VERSION) -t usbnetshare-registry:$(VERSION) -t usbnetshare-registry:latest .

test:
	go test ./...

clean:
	rm -rf $(OUT)
