# usbnetshare-registry: servidor de identidades de USBnetShare.
# Construir desde la raíz del repositorio:
#   docker build -f deploy/registry/Dockerfile -t usbnetshare-registry .

FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/usbnetshare-registry ./cmd/usbnetshare-registry \
 && mkdir -p /out/data

# Imagen final mínima: sin shell ni gestor de paquetes, usuario sin privilegios.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/usbnetshare-registry /usbnetshare-registry
# Carpeta de datos propiedad del usuario nonroot (65532) para que el volumen
# nuevo se cree con permisos de escritura.
COPY --from=build --chown=65532:65532 /out/data /data

ENV USBNETSHARE_REGISTRY_ADDR=":3250" \
    USBNETSHARE_REGISTRY_DATA="/data"
EXPOSE 3250
# token y peers.json: se conservan al actualizar o recrear el contenedor.
VOLUME ["/data"]
USER 65532:65532

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD ["/usbnetshare-registry", "-healthcheck"]

ENTRYPOINT ["/usbnetshare-registry"]
