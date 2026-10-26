package usbids

import "testing"

func TestVendor(t *testing.T) {
	for vid, want := range map[uint16]string{
		0x0908: "Siemens AG",
		0x0951: "Kingston Technology",
		0x046d: "Logitech, Inc.",
		0x13b1: "Linksys",
	} {
		if got := Vendor(vid); got != want {
			t.Errorf("Vendor(%04x) = %q, quiero %q", vid, got, want)
		}
	}
	if Vendor(0xfffe) != "" {
		t.Error("un VID desconocido debería devolver vacío")
	}
}

func TestProduct(t *testing.T) {
	for k, want := range map[[2]uint16]string{
		{0x13b1, 0x0041}: "Gigabit Ethernet Adapter",
		{0x046d, 0xc534}: "Nano Receiver",
	} {
		if got := Product(k[0], k[1]); got != want {
			t.Errorf("Product(%04x:%04x) = %q, quiero %q", k[0], k[1], got, want)
		}
	}
	// Las secciones de clases de usb.ids ("C 03  Human Interface Device" y sus
	// subclases tabuladas) no deben colarse como productos.
	if p := Product(0xfffe, 0x0001); p != "" {
		t.Errorf("producto de VID desconocido: %q", p)
	}
}

func TestFill(t *testing.T) {
	// El nombre que da el dispositivo manda; usb.ids rellena lo que falta.
	p, m := Fill(0x13b1, 0x0041, "Linksys USB3GIGV1", "")
	if p != "Linksys USB3GIGV1" || m != "Linksys" {
		t.Errorf("Fill = %q, %q", p, m)
	}
	p, m = FillHex("13b1", "0041", "", "")
	if p != "Gigabit Ethernet Adapter" || m != "Linksys" {
		t.Errorf("FillHex = %q, %q", p, m)
	}
	if p, m = FillHex("zz", "0041", "x", "y"); p != "x" || m != "y" {
		t.Errorf("FillHex con VID inválido = %q, %q", p, m)
	}
}
