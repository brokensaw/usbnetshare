// usbnetshare-registry: servidor de identidades de USBnetShare. Cada equipo se
// registra con su ID, IP y roles, y los clientes localizan aquí los
// servidores por ID, sin búsquedas por la red. Pensado para Docker; se
// configura con variables de entorno (se aceptan también las antiguas
// USBNET_REGISTRY_*, de las versiones usbnet):
//
//	USBNETSHARE_REGISTRY_ADDR           dirección de escucha (por defecto ":3250")
//	USBNETSHARE_REGISTRY_DATA           carpeta de datos/volumen (por defecto "/data")
//	USBNETSHARE_REGISTRY_TOKEN          token fijo (opcional; si no, se genera y se guarda)
//	USBNETSHARE_REGISTRY_OFFLINE_AFTER  sin latido durante este tiempo = desconectado ("45s")
//	USBNETSHARE_REGISTRY_TLS_CERT/_KEY  certificado y clave para HTTPS (opcional)
//	USBNETSHARE_REGISTRY_LOG_LEVEL      debug | info | warn | error
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/brokensaw/usbnetshare/internal/registry"
)

var version = "dev"

// envPrefix es el prefijo de las variables de entorno; legacyEnvPrefix, el de
// las versiones usbnet (se sigue aceptando para no romper despliegues).
const (
	envPrefix       = "USBNETSHARE_REGISTRY_"
	legacyEnvPrefix = "USBNET_REGISTRY_"
)

// env lee USBNETSHARE_REGISTRY_<name> o, si no está, USBNET_REGISTRY_<name>.
func env(name, def string) string {
	for _, p := range []string{envPrefix, legacyEnvPrefix} {
		if v := strings.TrimSpace(os.Getenv(p + name)); v != "" {
			return v
		}
	}
	return def
}

func main() {
	health := flag.Bool("healthcheck", false, "comprueba que el registro responde (para HEALTHCHECK de Docker) y sale")
	showVer := flag.Bool("version", false, "muestra la versión")
	flag.Parse()

	addr := env("ADDR", ":3250")
	if *showVer {
		fmt.Println("usbnetshare-registry", version)
		return
	}
	if *health {
		os.Exit(healthcheck(addr))
	}

	lvl := slog.LevelInfo
	switch strings.ToLower(env("LOG_LEVEL", "info")) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))

	dataDir := env("DATA", "/data")
	offline, err := time.ParseDuration(env("OFFLINE_AFTER", "45s"))
	if err != nil || offline < 20*time.Second {
		log.Error("USBNETSHARE_REGISTRY_OFFLINE_AFTER no válido (mínimo 20s)", "valor", env("OFFLINE_AFTER", ""))
		os.Exit(1)
	}
	cert, key := env("TLS_CERT", ""), env("TLS_KEY", "")
	if (cert == "") != (key == "") {
		log.Error("para HTTPS hacen falta USBNETSHARE_REGISTRY_TLS_CERT y USBNETSHARE_REGISTRY_TLS_KEY")
		os.Exit(1)
	}

	token, fromEnv, created, err := registry.LoadOrCreateToken(dataDir, env("TOKEN", ""))
	if err != nil {
		log.Error("no se pudo preparar el token", "carpeta", dataDir, "err", err)
		os.Exit(1)
	}
	store, err := registry.NewStore(dataDir, offline)
	if err != nil {
		log.Error("no se pudieron leer los datos", "carpeta", dataDir, "err", err)
		os.Exit(1)
	}

	log.Info("usbnetshare-registry", "versión", version, "datos", dataDir)
	registry.PrintToken(os.Stdout, token, fromEnv, created)
	if cert == "" {
		log.Warn("sin TLS: el token viaja sin cifrar; úsalo en una red de confianza o configura USBNETSHARE_REGISTRY_TLS_CERT/_KEY")
	}

	srv := registry.NewServer(store, dataDir, token, fromEnv, version, log)
	srv.Out = os.Stdout
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := srv.Run(ctx, addr, cert, key); err != nil {
		log.Error("el registro se detuvo", "err", err)
		os.Exit(1)
	}
}

// healthcheck consulta /healthz en local (la imagen no tiene curl ni shell).
func healthcheck(addr string) int {
	host := addr
	if strings.HasPrefix(host, ":") {
		host = "127.0.0.1" + host
	}
	scheme := "http"
	hc := &http.Client{Timeout: 3 * time.Second}
	if env("TLS_CERT", "") != "" {
		scheme = "https"
		hc.Transport = insecureLocalTransport()
	}
	resp, err := hc.Get(scheme + "://" + host + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
