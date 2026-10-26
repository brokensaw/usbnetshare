package main

import (
	"crypto/tls"
	"net/http"
)

// insecureLocalTransport se usa solo en el healthcheck contra 127.0.0.1: el
// certificado está emitido para el nombre público, no para localhost.
func insecureLocalTransport() http.RoundTripper {
	return &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // solo loopback
}
