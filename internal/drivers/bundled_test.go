//go:build windows && drivers

package drivers

import "testing"

// Con -tags drivers los instaladores deben estar dentro y ser los publicados.
func TestBundledPayload(t *testing.T) {
	if !Bundled() {
		t.Fatal("compilado con -tags drivers pero sin instaladores: ejecuta make drivers")
	}
}
