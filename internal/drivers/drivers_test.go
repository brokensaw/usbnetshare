package drivers

import "testing"

func TestFindAndVerify(t *testing.T) {
	for _, id := range []string{"usbip", "usbdk", " UsbDk "} {
		if _, ok := Find(id); !ok {
			t.Errorf("Find(%q) no encontró el paquete", id)
		}
	}
	if _, ok := Find("zadig"); ok {
		t.Error("Find(zadig) no debería existir")
	}
	p, _ := Find("usbdk")
	if err := p.Verify([]byte("no es el instalador")); err == nil {
		t.Error("Verify aceptó datos que no son el instalador")
	}
	for _, p := range Packages {
		if len(p.SHA256) != 64 || p.File == "" || p.URL == "" {
			t.Errorf("paquete %s incompleto: %+v", p.ID, p)
		}
	}
}
