// Package protocol implementa el protocolo USB/IP (versión 1.1.1), el mismo que
// usan el kernel de Linux (usbip-host / vhci-hcd) y usbip-win2 en Windows.
// Ser compatible a nivel de cable permite reutilizar los drivers ya existentes
// del lado cliente en lugar de escribir un controlador USB virtual propio.
package protocol

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
)

const Version uint16 = 0x0111

// Códigos de operación de la fase de control (antes de importar un dispositivo).
const (
	OpReqDevlist uint16 = 0x8005
	OpRepDevlist uint16 = 0x0005
	OpReqImport  uint16 = 0x8003
	OpRepImport  uint16 = 0x0003
)

// Estados de respuesta (ST_*).
const (
	StOK      uint32 = 0
	StNA      uint32 = 1
	StDevBusy uint32 = 2
	StDevErr  uint32 = 3
	StNoDev   uint32 = 4
	StError   uint32 = 5
)

func StatusText(st uint32) string {
	switch st {
	case StOK:
		return "ok"
	case StNA:
		return "no disponible"
	case StDevBusy:
		return "dispositivo ocupado"
	case StDevErr:
		return "error de dispositivo"
	case StNoDev:
		return "dispositivo no encontrado"
	default:
		return fmt.Sprintf("error (%d)", st)
	}
}

// Comandos de la fase de transferencia (URBs).
const (
	CmdSubmit uint32 = 1
	CmdUnlink uint32 = 2
	RetSubmit uint32 = 3
	RetUnlink uint32 = 4

	DirOut uint32 = 0
	DirIn  uint32 = 1
)

// Velocidades tal como las codifica USB/IP (enum usb_device_speed del kernel).
const (
	SpeedUnknown   uint32 = 0
	SpeedLow       uint32 = 1
	SpeedFull      uint32 = 2
	SpeedHigh      uint32 = 3
	SpeedWireless  uint32 = 4
	SpeedSuper     uint32 = 5
	SpeedSuperPlus uint32 = 6
)

// Errno negativos que se devuelven en RET_SUBMIT / RET_UNLINK.
const (
	ENODEV     int32 = -19
	EPIPE      int32 = -32
	EOVERFLOW  int32 = -75
	EPROTO     int32 = -71
	EOPNOTSUPP int32 = -95
	ECONNRESET int32 = -104
	ETIMEDOUT  int32 = -110
)

const (
	pathLen    = 256
	busIDLen   = 32
	deviceSize = 312
	urbHdrSize = 48
)

type OpHeader struct {
	Version uint16
	Code    uint16
	Status  uint32
}

func ReadOpHeader(r io.Reader) (OpHeader, error) {
	var h OpHeader
	err := binary.Read(r, binary.BigEndian, &h)
	return h, err
}

func WriteOpHeader(w io.Writer, code uint16, status uint32) error {
	return binary.Write(w, binary.BigEndian, OpHeader{Version, code, status})
}

type Interface struct {
	Class, SubClass, Protocol uint8
}

// Device describe un dispositivo exportable (struct usbip_usb_device).
type Device struct {
	Path               string
	BusID              string
	BusNum, DevNum     uint32
	Speed              uint32
	IDVendor           uint16
	IDProduct          uint16
	BcdDevice          uint16
	DeviceClass        uint8
	DeviceSubClass     uint8
	DeviceProtocol     uint8
	ConfigurationValue uint8
	NumConfigurations  uint8
	NumInterfaces      uint8
	Interfaces         []Interface

	// Product y Manufacturer no viajan por el protocolo; solo se usan para
	// mostrar y en el descubrimiento.
	Product      string
	Manufacturer string
}

func (d *Device) DevID() uint32 { return d.BusNum<<16 | d.DevNum }

func (d *Device) String() string {
	s := fmt.Sprintf("%s %04x:%04x", d.BusID, d.IDVendor, d.IDProduct)
	if d.Product != "" {
		s += " (" + d.Product + ")"
	}
	return s
}

func putFixed(dst []byte, s string) {
	n := copy(dst, s)
	if n == len(dst) { // garantiza terminación NUL
		dst[len(dst)-1] = 0
	}
}

func getFixed(src []byte) string {
	if i := bytes.IndexByte(src, 0); i >= 0 {
		return string(src[:i])
	}
	return string(src)
}

func (d *Device) encode() []byte {
	b := make([]byte, deviceSize)
	putFixed(b[0:pathLen], d.Path)
	putFixed(b[pathLen:pathLen+busIDLen], d.BusID)
	be := binary.BigEndian
	be.PutUint32(b[288:], d.BusNum)
	be.PutUint32(b[292:], d.DevNum)
	be.PutUint32(b[296:], d.Speed)
	be.PutUint16(b[300:], d.IDVendor)
	be.PutUint16(b[302:], d.IDProduct)
	be.PutUint16(b[304:], d.BcdDevice)
	b[306] = d.DeviceClass
	b[307] = d.DeviceSubClass
	b[308] = d.DeviceProtocol
	b[309] = d.ConfigurationValue
	b[310] = d.NumConfigurations
	b[311] = d.NumInterfaces
	return b
}

// ReadDevice lee una struct usbip_usb_device (sin interfaces).
func ReadDevice(r io.Reader) (Device, error) {
	b := make([]byte, deviceSize)
	if _, err := io.ReadFull(r, b); err != nil {
		return Device{}, err
	}
	be := binary.BigEndian
	return Device{
		Path:               getFixed(b[0:pathLen]),
		BusID:              getFixed(b[pathLen : pathLen+busIDLen]),
		BusNum:             be.Uint32(b[288:]),
		DevNum:             be.Uint32(b[292:]),
		Speed:              be.Uint32(b[296:]),
		IDVendor:           be.Uint16(b[300:]),
		IDProduct:          be.Uint16(b[302:]),
		BcdDevice:          be.Uint16(b[304:]),
		DeviceClass:        b[306],
		DeviceSubClass:     b[307],
		DeviceProtocol:     b[308],
		ConfigurationValue: b[309],
		NumConfigurations:  b[310],
		NumInterfaces:      b[311],
	}, nil
}

// WriteDevlistReply envía OP_REP_DEVLIST con la lista de dispositivos.
func WriteDevlistReply(w io.Writer, devs []Device) error {
	var buf bytes.Buffer
	_ = WriteOpHeader(&buf, OpRepDevlist, StOK)
	_ = binary.Write(&buf, binary.BigEndian, uint32(len(devs)))
	for i := range devs {
		d := devs[i]
		d.NumInterfaces = uint8(len(d.Interfaces))
		buf.Write(d.encode())
		for _, it := range d.Interfaces {
			buf.Write([]byte{it.Class, it.SubClass, it.Protocol, 0})
		}
	}
	_, err := w.Write(buf.Bytes())
	return err
}

// ReadDevlistReply lee el cuerpo de OP_REP_DEVLIST (la cabecera ya leída).
func ReadDevlistReply(r io.Reader) ([]Device, error) {
	var n uint32
	if err := binary.Read(r, binary.BigEndian, &n); err != nil {
		return nil, err
	}
	if n > 1024 {
		return nil, fmt.Errorf("número de dispositivos absurdo: %d", n)
	}
	devs := make([]Device, 0, n)
	for i := uint32(0); i < n; i++ {
		d, err := ReadDevice(r)
		if err != nil {
			return nil, err
		}
		for j := 0; j < int(d.NumInterfaces); j++ {
			var ib [4]byte
			if _, err := io.ReadFull(r, ib[:]); err != nil {
				return nil, err
			}
			d.Interfaces = append(d.Interfaces, Interface{ib[0], ib[1], ib[2]})
		}
		devs = append(devs, d)
	}
	return devs, nil
}

func WriteImportRequest(w io.Writer, busid string) error {
	var buf bytes.Buffer
	_ = WriteOpHeader(&buf, OpReqImport, StOK)
	b := make([]byte, busIDLen)
	putFixed(b, busid)
	buf.Write(b)
	_, err := w.Write(buf.Bytes())
	return err
}

func ReadImportRequest(r io.Reader) (string, error) {
	b := make([]byte, busIDLen)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", err
	}
	return getFixed(b), nil
}

func WriteImportReply(w io.Writer, status uint32, d *Device) error {
	var buf bytes.Buffer
	_ = WriteOpHeader(&buf, OpRepImport, status)
	if status == StOK && d != nil {
		buf.Write(d.encode())
	}
	_, err := w.Write(buf.Bytes())
	return err
}

// ---------------------------------------------------------------------------
// Fase de transferencia (URBs)
// ---------------------------------------------------------------------------

type HeaderBasic struct {
	Command, SeqNum, DevID, Direction, EP uint32
}

type CmdSubmitBody struct {
	TransferFlags uint32
	BufferLength  int32
	StartFrame    int32
	NumPackets    int32
	Interval      int32
	Setup         [8]byte
}

type CmdUnlinkBody struct {
	UnlinkSeqNum uint32
}

// URB es un comando recibido del cliente ya decodificado.
type URB struct {
	Header HeaderBasic
	Submit CmdSubmitBody // válido si Command == CmdSubmit
	Unlink CmdUnlinkBody // válido si Command == CmdUnlink
	Data   []byte        // datos OUT
	IsISO  bool
}

// ReadURB lee un CMD_SUBMIT o CMD_UNLINK completo, incluidos los datos OUT y
// los descriptores ISO (que se consumen para no desincronizar el flujo).
func ReadURB(r io.Reader) (*URB, error) {
	b := make([]byte, urbHdrSize)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	be := binary.BigEndian
	u := &URB{Header: HeaderBasic{
		Command:   be.Uint32(b[0:]),
		SeqNum:    be.Uint32(b[4:]),
		DevID:     be.Uint32(b[8:]),
		Direction: be.Uint32(b[12:]),
		EP:        be.Uint32(b[16:]),
	}}
	switch u.Header.Command {
	case CmdSubmit:
		s := &u.Submit
		s.TransferFlags = be.Uint32(b[20:])
		s.BufferLength = int32(be.Uint32(b[24:]))
		s.StartFrame = int32(be.Uint32(b[28:]))
		s.NumPackets = int32(be.Uint32(b[32:]))
		s.Interval = int32(be.Uint32(b[36:]))
		copy(s.Setup[:], b[40:48])
		if s.BufferLength < 0 || s.BufferLength > 16<<20 {
			return nil, fmt.Errorf("longitud de buffer inválida: %d", s.BufferLength)
		}
		if u.Header.Direction == DirOut && s.BufferLength > 0 {
			u.Data = make([]byte, s.BufferLength)
			if _, err := io.ReadFull(r, u.Data); err != nil {
				return nil, err
			}
		}
		// number_of_packets vale 0 o 0xffffffff para transferencias no ISO.
		if s.NumPackets > 0 {
			u.IsISO = true
			if s.NumPackets > 1024 {
				return nil, fmt.Errorf("demasiados paquetes ISO: %d", s.NumPackets)
			}
			if _, err := io.CopyN(io.Discard, r, int64(s.NumPackets)*16); err != nil {
				return nil, err
			}
		}
	case CmdUnlink:
		u.Unlink.UnlinkSeqNum = be.Uint32(b[20:])
	default:
		return nil, fmt.Errorf("comando USB/IP desconocido: %d", u.Header.Command)
	}
	return u, nil
}

// EncodeRetSubmit construye un RET_SUBMIT (cabecera + datos IN).
func EncodeRetSubmit(seq uint32, status int32, data []byte) []byte {
	b := make([]byte, urbHdrSize+len(data))
	be := binary.BigEndian
	be.PutUint32(b[0:], RetSubmit)
	be.PutUint32(b[4:], seq)
	// devid, direction y ep van a 0 en las respuestas.
	be.PutUint32(b[20:], uint32(status))
	be.PutUint32(b[24:], uint32(len(data)))
	copy(b[urbHdrSize:], data)
	return b
}

// EncodeRetSubmitOut construye un RET_SUBMIT para una transferencia OUT
// (actual_length sin datos detrás).
func EncodeRetSubmitOut(seq uint32, status int32, actual int) []byte {
	b := make([]byte, urbHdrSize)
	be := binary.BigEndian
	be.PutUint32(b[0:], RetSubmit)
	be.PutUint32(b[4:], seq)
	be.PutUint32(b[20:], uint32(status))
	be.PutUint32(b[24:], uint32(actual))
	return b
}

func EncodeRetUnlink(seq uint32, status int32) []byte {
	b := make([]byte, urbHdrSize)
	be := binary.BigEndian
	be.PutUint32(b[0:], RetUnlink)
	be.PutUint32(b[4:], seq)
	be.PutUint32(b[20:], uint32(status))
	return b
}
