package main

import "testing"

// Las variables nuevas mandan; las antiguas de usbnet siguen funcionando.
func TestEnvLegacy(t *testing.T) {
	t.Setenv("USBNET_REGISTRY_OFFLINE_AFTER", "60s")
	if v := env("OFFLINE_AFTER", "45s"); v != "60s" {
		t.Fatalf("variable antigua: %q", v)
	}
	t.Setenv("USBNETSHARE_REGISTRY_OFFLINE_AFTER", "90s")
	if v := env("OFFLINE_AFTER", "45s"); v != "90s" {
		t.Fatalf("la variable nueva debe mandar: %q", v)
	}
	if v := env("NO_EXISTE", "def"); v != "def" {
		t.Fatalf("valor por defecto: %q", v)
	}
}
