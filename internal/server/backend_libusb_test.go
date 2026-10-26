//go:build libusb

package server

import (
	"testing"

	"github.com/google/gousb"
)

func TestBusIDsUsbDk(t *testing.T) {
	descs := []*gousb.DeviceDesc{
		{Bus: 3, Port: 13, Path: []int{13}, Vendor: 0x13b1, Product: 0x0041},
		{Bus: 3, Port: 9, Path: []int{9}, Vendor: 0x0908, Product: 0x0004},
		{Bus: 3, Port: 2, Path: []int{2}, Vendor: 0x0908, Product: 0x0004},
	}
	b := &libusbBackend{usbdk: true}
	got := b.busIDs(descs)
	want := []string{"dk-13b1-0041", "dk-0908-0004-2", "dk-0908-0004-1"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("UsbDk busid[%d] = %q, quiero %q", i, got[i], want[i])
		}
	}
	// El mismo Linksys enumerado por la línea USB 2 (puerto 9) conserva el busid.
	descs[0].Port, descs[0].Path = 9, []int{9}
	if id := b.busIDs(descs[:1])[0]; id != "dk-13b1-0041" {
		t.Errorf("cambio de puerto: %q", id)
	}

	b.usbdk = false
	if id := b.busIDs(descs[:1])[0]; id != "3-9" {
		t.Errorf("sin UsbDk: %q, quiero 3-9", id)
	}
}
