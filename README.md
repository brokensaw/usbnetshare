<img src="logo/usbnetshare256.png" alt="USBnetShare" width="96" align="right">

# USBnetShare

**Comparte dispositivos USB por la red (USB/IP) y úsalos desde otro equipo como si estuvieran
enchufados en él.**

> **Tutoriales paso a paso, con capturas:**
> [USBnetShare en Windows: drivers, modo servidor y modo cliente](tutoriales/tutorial_usbnetshare.md) ·
> [El coordinador en Docker / Portainer](tutoriales/tutorial_coordinador.md)

## Índice

1. [Descripción general](#1-descripción-general)
2. [Stack tecnológico](#2-stack-tecnológico)
3. [Instalación y ejecución](#3-instalación-y-ejecución)
4. [Estructura del proyecto](#4-estructura-del-proyecto)
5. [Funcionalidades principales](#5-funcionalidades-principales)
6. [Uso en redes industriales](#6-uso-en-redes-industriales)
7. [Actualizar desde usbnet](#7-actualizar-desde-usbnet-versiones-anteriores)
8. [Seguridad, limitaciones y licencias](#8-seguridad-limitaciones-y-licencias)

---

## 1. Descripción general

USBnetShare permite usar un dispositivo USB conectado a un ordenador desde **otro ordenador de
la red**. El equipo que lo usa lo ve como un USB local: los programas de siempre funcionan sin
cambios (TIA Portal / STEP 7 con su driver normal, programas de puerto serie, etc.).

Nació para un caso concreto: programar y diagnosticar **PLC Siemens** a través de un
**Siemens PC Adapter USB** que está en planta, junto a la máquina, sin que el técnico tenga
que desplazarse. Sirve igual para otros adaptadores serie/PLC, como los **conversores
USB‑RS485**.

El sistema tiene dos piezas:

| Pieza | Qué es |
|---|---|
| **USBnetShare** (`USBnetShare.exe` en Windows, `usbnetshare` en Linux) | Una sola aplicación con dos roles que se activan por separado: **servidor** (comparte sus USB) y **cliente** (usa USB remotos). En Windows tiene interfaz gráfica; en Linux funciona por consola y como servicio. |
| **usbnetshare-registry** («coordinador» o «servidor de identidades») | Servicio en Docker con interfaz web. Cada USBnetShare se registra en él con su **ID**, IP y roles, y los clientes le preguntan dónde está cada servidor. |

```
                     ┌────────── usbnetshare-registry (Docker) ─────────┐
                     │  ID · nombre · IP · roles · USB · conexiones      │
                     │  GUI web  http://servidor:3250   (token)          │
                     └───────▲──────────────────────────────▲───────────┘
                    latido   │ HTTP 3250              HTTP  │ ¿dónde está el ID …?
 ┌──────── PC que comparte ──┴───┐                ┌─────────┴── PC que usa el USB ───┐
 │ PC Adapter USB                │   USB/IP TCP   │ TIA Portal / STEP 7              │
 │ USBnetShare (servidor)        │◄──── 3240 ────►│ USBnetShare (cliente)            │
 └───────────────────────────────┘                │ driver usbip-win2 (USB virtual)  │
                                                  └──────────────────────────────────┘
```

Los datos del USB viajan **directamente** entre los dos equipos (USB/IP sobre TCP 3240). El
coordinador solo guarda quién es quién y dónde está; no pasa ningún dato del USB por él.

---

## 2. Stack tecnológico

| Área | Tecnología | Uso en el proyecto |
|---|---|---|
| Lenguaje | **Go 1.25** | Todo el código: aplicación, servidor y cliente USB/IP, coordinador y herramientas. |
| Interfaz de escritorio | **Wails v2** (WebView2 en Windows) | Ventana de la aplicación, con la interfaz embebida en el ejecutable. |
| Interfaz web | **HTML, CSS y JavaScript** sin frameworks | Ventana de USBnetShare (`cmd/usbnetshare/frontend`) y web del coordinador (`internal/registry/web`). |
| Protocolo | **USB/IP 1.1.1** (estándar del kernel Linux) | Transporte de las transferencias USB por TCP; interoperable con `usbip`. |
| Acceso al USB (Linux) | Módulos del kernel **`usbip-host`** (servidor) y **`vhci-hcd`** (cliente) | Exportar y crear el USB virtual mediante sysfs. |
| Acceso al USB (Windows) | **libusb 1.0** vía **gousb** (cgo) + **UsbDk 1.0.22** | Abrir el USB en el servidor sin cambiar su driver (alternativa: WinUSB/Zadig). |
| USB virtual (Windows) | **usbip-win2 0.9.8.1** | Controlador del rol cliente; USBnetShare le habla por IOCTL (sin `usbip.exe`). |
| Servicio del sistema | **kardianos/service** | Servicio de Windows / systemd con reinicio automático. |
| Configuración | **YAML** (`gopkg.in/yaml.v3`) | `USBnetShare.yaml` con carga estricta y migración desde versiones anteriores. |
| Coordinador | **Docker** (imagen *distroless*, usuario sin privilegios), **Portainer** | Despliegue y actualización desde el repositorio (`pull_policy: build`). |
| Nombres de dispositivos | **usb.ids** (linux-usb.org) | Fabricante y producto de cada USB. |
| Windows API | `golang.org/x/sys/windows` | SetupAPI, IOCTL, servicios, firewall (`netsh`), UAC. |
| Compilación | **Make**, **MSYS2** (gcc, libusb, pkg-config, windres) | Binarios de Windows con cgo, recurso con el icono, binarios de Linux estáticos. |
| Pruebas | `go test` (+ **goja** para validar el JavaScript de la interfaz) | Pruebas por paquete, incluido un servidor USB/IP de extremo a extremo con backend simulado. |

---

## 3. Instalación y ejecución

### 3.1 Requisitos

| Componente | Requisitos |
|---|---|
| USBnetShare en Windows | Windows 10 1903 o posterior / Windows 11, 64 bits, permisos de administrador para instalar drivers y servicio. |
| USBnetShare en Linux | amd64 o arm64 (p. ej. Raspberry Pi), root, módulos `usbip-host` y/o `vhci-hcd` (paquete `linux-tools-generic`, o `usbip` en Raspberry Pi OS). |
| Coordinador | Un equipo con Docker (y opcionalmente Portainer), accesible por TCP 3250 desde los PC. |
| Red | Los PC que comparten deben aceptar conexiones en TCP 3240 (la instalación del servicio añade la regla del firewall). |

### 3.2 Coordinador (Docker)

**Con Portainer (recomendado):** *Stacks → Add stack → Repository*

- **Repository URL:** `http://<servidor-git>/<grupo>/USBNetShare.git` (con autenticación si es privado).
- **Repository reference:** la rama o etiqueta a desplegar, p. ej. `refs/heads/main`.
- **Compose path:** `deploy/registry/docker-compose.yml`.
- El token aparece en *Containers → usbnetshare-registry → Logs*.
- Para actualizar: subir los cambios y pulsar *Pull and redeploy*. La imagen se construye desde el
  código (`pull_policy: build`); no se descarga de ningún registro.

**Con Docker Compose:**

```bash
docker compose -f deploy/registry/docker-compose.yml up -d --build
docker compose -f deploy/registry/docker-compose.yml logs usbnetshare-registry   # muestra el token
```

**Con Docker a mano:**

```bash
docker build -f deploy/registry/Dockerfile -t usbnetshare-registry .
docker run -d --name usbnetshare-registry --restart unless-stopped \
  -p 3250:3250 -v usbnet-registry-data:/data usbnetshare-registry
docker logs usbnetshare-registry        # muestra el TOKEN de conexión
```

Interfaz web: `http://<servidor-docker>:3250` (pide el token).

**Token**

- Si no se indica, se **genera en el primer arranque**, se muestra en la consola y se guarda en
  `/data`. Se **conserva** al actualizar o recrear el contenedor mientras se mantenga el volumen.
- Para fijarlo: `USBNETSHARE_REGISTRY_TOKEN` (mínimo 16 caracteres); tiene prioridad.
- Desde la web se puede regenerar (salvo si viene de la variable); después hay que ponerlo en
  todos los equipos.

**Variables de entorno** (se aceptan también las antiguas `USBNET_REGISTRY_*`)

| Variable | Por defecto | Descripción |
|---|---|---|
| `USBNETSHARE_REGISTRY_TOKEN` | (generado) | Token de conexión. |
| `USBNETSHARE_REGISTRY_ADDR` | `:3250` | Dirección de escucha. |
| `USBNETSHARE_REGISTRY_DATA` | `/data` | Carpeta con el token y `peers.json`. |
| `USBNETSHARE_REGISTRY_OFFLINE_AFTER` | `45s` | Sin latido durante este tiempo, el equipo aparece desconectado (mínimo 20 s). |
| `USBNETSHARE_REGISTRY_TLS_CERT` / `_KEY` | — | Certificado y clave para HTTPS (recomendado fuera de una red de confianza). |
| `USBNETSHARE_REGISTRY_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. |

### 3.3 USBnetShare en Windows

1. Copia `USBnetShare.exe`, `libusb-1.0.dll` y la carpeta `licenses` en una carpeta y abre el
   programa.
2. **Ajustes → Drivers de Windows → Instalar drivers seleccionados** (pide administrador). Los
   instaladores oficiales y firmados van dentro del ejecutable y se instalan sin ventanas:
   - **usbip-win2 0.9.8.1**: en los PC que *usan* USB remotos. Reinicia los hubs USB 3.0 (los USB
     se desconectan un momento).
   - **UsbDk 1.0.22**: en los PC que *comparten* USB.

   Si Windows pide reiniciar, la ventana lo indica. Por consola:
   `USBnetShare -install-drivers usbip,usbdk` (como administrador).
3. **Ajustes → Servidor de identidades:** URL (`http://<servidor-docker>:3250`) y token →
   «Probar conexión» → «Guardar». La cabecera muestra «Identidades: conectado».
4. **En el PC que comparte:** pestaña «Compartir mis dispositivos» → marcar el USB.
5. **En el PC que lo usa:** «Usar dispositivos remotos» → «Buscar servidores» → «Conectar».
6. **Ajustes → Instalar como servicio** para que funcione sin la ventana abierta (también añade
   la regla del firewall para TCP 3240).

La configuración se guarda en `%ProgramData%\USBnetShare\USBnetShare.yaml` (ejemplo comentado en
[configs/usbnetshare.yaml](configs/usbnetshare.yaml)). Si existe un `USBnetShare.yaml` junto al
ejecutable, se usa ese (instalación portátil).

### 3.4 USBnetShare en Linux (sin interfaz gráfica, p. ej. Raspberry Pi)

```bash
sudo apt install linux-tools-generic        # Raspberry Pi OS: paquete "usbip"
sudo mkdir -p /etc/usbnetshare && sudo cp configs/usbnetshare.yaml /etc/usbnetshare/
# editar registry.url / registry.token y server.enabled: true (o client.enabled)
sudo ./usbnetshare -list                     # ver los USB del equipo
sudo ./usbnetshare -service install          # instalar y arrancar el servicio (systemd)
```

### 3.5 Opciones de línea de comandos

| Opción | Descripción |
|---|---|
| *(sin opciones)* | Abre la interfaz gráfica (Windows). |
| `-config <fichero>` | Fichero de configuración (por defecto, el del apartado 3.3/3.4). |
| `-headless` | Ejecuta sin ventana (lo que usa el servicio). |
| `-service install\|uninstall\|start\|stop\|restart` | Gestiona el servicio del sistema. |
| `-list` | Lista los USB del equipo y si se comparten. |
| `-discover` | Busca servidores y muestra sus dispositivos. |
| `-firewall add\|remove` | Regla del Firewall de Windows (administrador). |
| `-install-drivers usbip,usbdk` | Instala los drivers incluidos (administrador). |
| `-version` | Muestra la versión. |

### 3.6 Compilar

Requiere **Go ≥ 1.25**. Para Windows hace falta cgo (libusb): MSYS2 con
`pacman -S mingw-w64-x86_64-gcc mingw-w64-x86_64-libusb mingw-w64-x86_64-pkg-config`
y `C:\msys64\mingw64\bin` en el `PATH`.

```bash
make test               # pruebas
make windows            # dist/windows-amd64/USBnetShare.exe con GUI y drivers incluidos
make linux              # dist/linux-{amd64,arm64}/usbnetshare (sin GUI, estático)
make registry           # binario Linux del coordinador
make registry-image     # imagen Docker del coordinador
make drivers            # solo descarga los instaladores de drivers (make windows ya lo hace)
make icon               # regenera el icono del .exe desde logo/usbnetshare256.png
```

`make windows` descarga los instaladores de usbip-win2 y UsbDk de sus releases oficiales en
`internal/drivers/payload/` (fuera de git), comprueba su SHA‑256 y los incrusta con
`-tags drivers`; sin esa etiqueta el programa funciona igual, pero Ajustes solo muestra los
enlaces de descarga. Copia `libusb-1.0.dll` junto al `.exe`; las licencias se copian solas.

---

## 4. Estructura del proyecto

```
cmd/
  usbnetshare/               aplicación: GUI Wails, servicio, utilidades de consola
    frontend/                interfaz (HTML/CSS/JS) y logo
    rsrc_windows_amd64.syso  icono e información del .exe (generado con make icon)
  usbnetshare-registry/      coordinador (servidor de identidades)
internal/
  appinfo/                   nombre del programa e identificadores de instalación
  engine/                    une los roles servidor y cliente; latido al coordinador
  server/                    servidor USB/IP: backends kernel (Linux) y libusb (+ UsbDk en Windows)
  client/                    localiza servidores, importa, vigila y reconecta; attach Linux/Windows
  protocol/                  protocolo USB/IP
  registry/                  coordinador: API, almacén, token, web (web/) y cliente
  config/                    configuración YAML, ruta por defecto y migración
  drivers/                   instaladores de usbip-win2 y UsbDk incrustados (Windows)
  usbdrv/                    driver de Windows de cada USB y detección de UsbDk
  usbids/                    nombres de fabricante y producto (usb.ids)
  api/                       API local para que la GUI controle el servicio
  svc/                       instalación del servicio (Windows / systemd)
  discovery/                 descubrimiento por multicast/broadcast (desactivado por defecto)
  netutil/, logx/            ajustes TCP y registro de log
tools/winres/                genera el icono del .exe (make icon)
configs/usbnetshare.yaml     configuración de ejemplo comentada
deploy/
  registry/                  Dockerfile y docker-compose del coordinador
  99-usbnetshare-libusb.rules  regla udev (backend libusb en Linux sin root)
logo/                        logo de la aplicación
third_party/                 usb.ids y licencias de usbip-win2 y UsbDk
tutoriales/                  guías paso a paso con capturas
Makefile                     compilación, pruebas y distribución
```

---

## 5. Funcionalidades principales

### Compartir dispositivos (rol servidor)
- Comparte los USB elegidos (por VID:PID, o también por bus) con una casilla en la interfaz.
- **Windows con UsbDk:** cualquier USB se puede compartir sin cambiar su driver. Mientras se
  comparte, Windows deja de verlo; lo recupera al dejar de compartir o si USBnetShare se cierra.
  Sin UsbDk, solo los dispositivos con WinUSB ([Zadig](https://zadig.akeo.ie/));
  `server.usbdk: off` obliga a ese modo.
- Con UsbDk el bus se muestra como `dk-vvvv-pppp` (p. ej. `dk-13b1-0041`): no cambia aunque el
  dispositivo se reconecte como USB 2 o USB 3.
- **Takeover:** si un cliente se reconecta y la sesión anterior quedó colgada, la cierra y
  atiende la nueva.
- Detecta cuándo se **desenchufa** el USB y cierra la sesión; en Linux lo vuelve a vincular al
  reenchufarlo.
- Restricción opcional de las redes que pueden conectarse (`allow_cidrs`).
- Confirmación antes de compartir teclados o ratones.

### Usar dispositivos remotos (rol cliente)
- «Buscar servidores» muestra los equipos que comparten y sus USB (libre / en uso); «Conectar»
  los adjunta al controlador USB virtual.
- Las conexiones se **guardan** con el ID del servidor y se recuperan solas tras cortes de red,
  reinicios, cambios de IP o de nombre, o si el USB se desenchufa y vuelve a enchufar.
- Estado de cada conexión: conectado, conectando, esperando dispositivo, reintentando.
- Servidores fijos por IP o nombre, para equipos fuera del coordinador.
- Mensajes de error comprensibles para los códigos de usbip-win2.

### Robustez
- Keepalive TCP ajustado (y `TCP_USER_TIMEOUT` en Linux): un corte de menos de ~20 s no rompe la
  sesión.
- Reconexión con espera creciente (1 s → 30 s).
- Funcionamiento como servicio con reinicio automático; la interfaz controla el servicio.

### Coordinador
- Lista de equipos con estado (en línea / desconectado), roles, versión, USB compartidos y quién
  los usa, y las conexiones de cada cliente.
- Eliminar equipos, regenerar el token, HTTPS opcional y chequeo de salud para Docker.

### Drivers e instalación (Windows)
- Instalación de usbip-win2 y UsbDk en un clic, con verificación SHA‑256 y aviso de reinicio.
- Detección del driver de cada USB y de si se puede compartir.
- Instalación como servicio y regla del firewall desde la propia ventana.

### Nombres de dispositivos
- Nombre del propio dispositivo y, si no lo da, el de [usb.ids](http://www.linux-usb.org/usb.ids);
  fabricante siempre desde usb.ids. También en el coordinador.

---

## 6. Uso en redes industriales

USBnetShare **no hace ninguna búsqueda por la red**. Solo habla con:

| Destino | Protocolo | Para qué |
|---|---|---|
| usbnetshare-registry | HTTP(S) TCP 3250 | Latido cada 15 s y consulta de equipos. |
| El servidor USB concreto | USB/IP TCP 3240 | La conexión del dispositivo. |

- El **barrido de subredes se eliminó**: enviaba un paquete a cada IP de cada subred del equipo y
  provocó una tormenta de ARP que dejó a varios PLC sin su periferia PROFINET.
- El **descubrimiento por multicast/broadcast** (UDP 3241) está **desactivado** por defecto. No lo
  actives en equipos conectados a redes de PLC.
- Con un USB desenchufado en el servidor, el cliente solo pregunta por TCP a la IP ya conocida del
  servidor (cada 5 s); no consulta al coordinador mientras esa IP responda.
- Las configuraciones antiguas con `sweep`/`scan` siguen cargando; esas opciones se ignoran y se
  avisa en el registro de log.

Antes de usarlo en planta, comprueba con una captura (Wireshark o `pktmon`) que no sale tráfico
hacia las redes de PLC: solo hacia el coordinador (3250) y el servidor USB (3240). Valídalo en
banco antes de producción, con los responsables de los PLC avisados.

---

## 7. Actualizar desde usbnet (versiones anteriores)

USBnetShare es el nuevo nombre de usbnet. Al actualizar:

- **Configuración:** la primera vez que arranca, copia `%ProgramData%\usbnet\usbnet.yaml` a
  `%ProgramData%\USBnetShare\USBnetShare.yaml` (Linux: `/etc/usbnet/usbnet.yaml` →
  `/etc/usbnetshare/usbnetshare.yaml`). El fichero antiguo se deja como respaldo. Se conservan el
  ID del equipo, el coordinador y las conexiones.
- **Servicio:** si sigue instalado el servicio `usbnet`, la ventana lo avisa y el botón pasa a
  **Actualizar servicio**: quita el servicio antiguo (y su regla de firewall `USBNet`) e instala
  `USBnetShare`.
- **Coordinador:** el contenedor pasa a llamarse `usbnetshare-registry` pero **usa el mismo
  volumen** (`usbnet-registry-data`): el token y la lista de equipos se conservan. En Portainer,
  mantén el nombre del stack y pulsa *Pull and redeploy* con **Prune services**, para retirar el
  contenedor `usbnet-registry` y liberar el puerto 3250. Las variables `USBNET_REGISTRY_*` siguen
  funcionando.
- Equipos con usbnet y con USBnetShare pueden convivir mientras se actualizan: el protocolo no
  cambia.

---

## 8. Seguridad, limitaciones y licencias

- **Seguridad:** USB/IP no cifra ni autentica la conexión TCP 3240. Úsalo en redes de confianza y
  limita con `server.allow_cidrs`. El coordinador exige token; sin TLS, el token viaja sin cifrar.
  La API local solo escucha en `127.0.0.1` y exige una cabecera propia.
- **Reconexión:** si un corte supera la tolerancia del keepalive, el dispositivo se desconecta y
  vuelve a aparecer en el cliente; TIA Portal puede necesitar repetir «Establecer conexión online».
- **Transferencias:** control, bulk e interrupt (suficiente para el PC Adapter y adaptadores
  serie/PLC); no se soportan isócronas (audio/vídeo).
- **UsbDk:** proyecto poco mantenido (última versión 1.0.22). Probarlo en cada modelo de PC, sobre
  todo con Windows 11 e «Integridad de memoria» activada. Algunos dispositivos (p. ej. el
  adaptador de red Linksys USB3GIGV1) hacen fallar su enumeración mientras están compartidos.
- **Software de terceros**, redistribuido sin modificar con sus licencias en `third_party/` (y en
  `dist/windows-amd64/licenses/`):
  [usbip-win2](https://github.com/vadimgrn/usbip-win2) (BSD 2-Clause),
  [UsbDk](https://github.com/daynix/UsbDk) (Apache 2.0) y
  [usb.ids](http://www.linux-usb.org/usb.ids) (BSD-3-Clause; para actualizarlo, sustituye
  `third_party/usb.ids` y recompila).
