package protocol

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestDevlistRoundTrip(t *testing.T) {
	in := []Device{{
		Path: "/sys/devices/x", BusID: "1-1.2", BusNum: 1, DevNum: 5, Speed: SpeedFull,
		IDVendor: 0x0908, IDProduct: 0x0004, BcdDevice: 0x0100, ConfigurationValue: 1,
		NumConfigurations: 1, Interfaces: []Interface{{0xff, 0, 0}},
	}}
	var buf bytes.Buffer
	if err := WriteDevlistReply(&buf, in); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 8+4+312+4 {
		t.Fatalf("tamaño inesperado %d", buf.Len())
	}
	h, _ := ReadOpHeader(&buf)
	if h.Version != Version || h.Code != OpRepDevlist {
		t.Fatalf("cabecera %+v", h)
	}
	out, err := ReadDevlistReply(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if out[0].BusID != "1-1.2" || out[0].IDVendor != 0x0908 || len(out[0].Interfaces) != 1 || out[0].DevID() != 1<<16|5 {
		t.Fatalf("decodificado %+v", out[0])
	}
}

func TestReadURBSubmitOut(t *testing.T) {
	b := make([]byte, 48)
	be := binary.BigEndian
	be.PutUint32(b[0:], CmdSubmit)
	be.PutUint32(b[4:], 7)
	be.PutUint32(b[12:], DirOut)
	be.PutUint32(b[16:], 2)
	be.PutUint32(b[24:], 3)
	be.PutUint32(b[32:], 0xffffffff) // no ISO
	b = append(b, 1, 2, 3)
	u, err := ReadURB(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if u.Header.SeqNum != 7 || u.Header.EP != 2 || !bytes.Equal(u.Data, []byte{1, 2, 3}) || u.IsISO {
		t.Fatalf("%+v", u)
	}
	r := EncodeRetSubmit(7, 0, []byte{9, 9})
	if len(r) != 50 || be.Uint32(r[0:]) != RetSubmit || be.Uint32(r[24:]) != 2 {
		t.Fatal("RET_SUBMIT mal formado")
	}
}
