// Package mgmt implements the Linux Bluetooth management (mgmt) protocol
// client: packet framing, the commands and events btspeaker needs, and the
// Controller that runs the startup sequence and the pairing policy.
// All multi-byte fields are little-endian; bdaddr_t is in wire (reversed) order.
package mgmt

import (
	"encoding/binary"
	"errors"
	"fmt"
	"unicode/utf8"

	"btspeaker/internal/bt"
)

const IndexNone = 0xffff

// Command opcodes.
const (
	OpReadIndexList    = 0x0003
	OpReadInfo         = 0x0004
	OpSetPowered       = 0x0005
	OpSetDiscoverable  = 0x0006
	OpSetConnectable   = 0x0007
	OpSetBondable      = 0x0009
	OpSetSSP           = 0x000B
	OpSetLE            = 0x000D
	OpSetDevClass      = 0x000E
	OpSetLocalName     = 0x000F
	OpAddUUID          = 0x0010
	OpRemoveUUID       = 0x0011
	OpLoadLinkKeys     = 0x0012
	OpDisconnect       = 0x0014
	OpGetConnections   = 0x0015
	OpPINCodeReply     = 0x0016
	OpPINCodeNegReply  = 0x0017
	OpSetIOCapability  = 0x0018
	OpUnpairDevice     = 0x001B
	OpUserConfirmReply = 0x001C
	OpUserConfirmNeg   = 0x001D
	OpSetBREDR         = 0x002A
	OpSetSecureConn    = 0x002D
)

// Event codes.
const (
	EvCmdComplete      = 0x0001
	EvCmdStatus        = 0x0002
	EvControllerError  = 0x0003
	EvIndexAdded       = 0x0004
	EvIndexRemoved     = 0x0005
	EvNewSettings      = 0x0006
	EvClassChanged     = 0x0007
	EvLocalNameChanged = 0x0008
	EvNewLinkKey       = 0x0009
	EvDeviceConnected  = 0x000B
	EvDeviceDisconn    = 0x000C
	EvConnectFailed    = 0x000D
	EvPINCodeRequest   = 0x000E
	EvUserConfirmReq   = 0x000F
	EvAuthFailed       = 0x0011
)

// Settings bits (current_settings).
const (
	SettingPowered      = 1 << 0
	SettingConnectable  = 1 << 1
	SettingDiscoverable = 1 << 3
	SettingBondable     = 1 << 4
	SettingSSP          = 1 << 6
	SettingBREDR        = 1 << 7
	SettingLE           = 1 << 9
	SettingSecureConn   = 1 << 11
)

// Address types.
const AddrBREDR = 0

// StatusCode is a mgmt status code (0 = success).
type StatusCode uint8

// Status codes matched by name (mgmt-api.txt).
const (
	StatusNotConnected StatusCode = 0x02
	StatusNotPaired    StatusCode = 0x06
)

var statusNames = [...]string{"Success", "Unknown Command", "Not Connected", "Failed", "Connect Failed",
	"Authentication Failed", "Not Paired", "No Resources", "Timeout", "Already Connected", "Busy",
	"Rejected", "Not Supported", "Invalid Parameters", "Disconnected", "Not Powered", "Cancelled",
	"Invalid Index", "RFKilled", "Already Paired", "Permission Denied"}

func (s StatusCode) String() string {
	if int(s) < len(statusNames) {
		return statusNames[s]
	}
	return fmt.Sprintf("status 0x%02x", uint8(s))
}

// StatusError is returned by Client.Do when the controller answers non-zero.
type StatusError struct {
	Op     uint16
	Status StatusCode
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("mgmt command 0x%04x: %s", e.Op, e.Status)
}

// Header is the 6-byte mgmt packet header.
type Header struct {
	Code  uint16 // opcode (commands) or event code
	Index uint16
	Len   uint16
}

// EncodePacket builds a command packet.
func EncodePacket(opcode, index uint16, params []byte) []byte {
	b := make([]byte, 6+len(params))
	binary.LittleEndian.PutUint16(b[0:], opcode)
	binary.LittleEndian.PutUint16(b[2:], index)
	binary.LittleEndian.PutUint16(b[4:], uint16(len(params)))
	copy(b[6:], params)
	return b
}

// DecodePacket splits one packet into header and params.
func DecodePacket(b []byte) (Header, []byte, error) {
	if len(b) < 6 {
		return Header{}, nil, errors.New("mgmt: short packet")
	}
	h := Header{binary.LittleEndian.Uint16(b[0:]), binary.LittleEndian.Uint16(b[2:]), binary.LittleEndian.Uint16(b[4:])}
	if int(h.Len) != len(b)-6 {
		return h, nil, fmt.Errorf("mgmt: length %d does not match payload %d", h.Len, len(b)-6)
	}
	return h, b[6:], nil
}

// DecodeCmdResult parses the params of Command Complete (opcode, status, data)
// or Command Status (opcode, status).
func DecodeCmdResult(p []byte) (opcode uint16, st StatusCode, data []byte, err error) {
	if len(p) < 3 {
		return 0, 0, nil, errors.New("mgmt: short command result")
	}
	return binary.LittleEndian.Uint16(p), StatusCode(p[2]), p[3:], nil
}

func wireAddr(a bt.Addr) [6]byte {
	var w [6]byte
	for i := range a {
		w[5-i] = a[i]
	}
	return w
}

func addrFromWire(w []byte) bt.Addr {
	var a bt.Addr
	for i := range a {
		a[i] = w[5-i]
	}
	return a
}

// addrParam is the 7-byte mgmt address (bdaddr + type) used by most device commands.
func addrParam(a bt.Addr) []byte {
	w := wireAddr(a)
	return append(w[:], AddrBREDR)
}

func u8(v bool) []byte {
	if v {
		return []byte{1}
	}
	return []byte{0}
}

// Info is the Read Controller Information reply.
type Info struct {
	Addr         bt.Addr
	Version      uint8
	Manufacturer uint16
	Supported    uint32
	Current      uint32
	Class        [3]byte
	Name         string
	ShortName    string
}

const infoLen = 6 + 1 + 2 + 4 + 4 + 3 + 249 + 11

func cstr(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// DecodeInfo parses a Read Controller Information reply.
func DecodeInfo(p []byte) (Info, error) {
	if len(p) < infoLen {
		return Info{}, errors.New("mgmt: short controller info")
	}
	var i Info
	i.Addr = addrFromWire(p[0:6])
	i.Version = p[6]
	i.Manufacturer = binary.LittleEndian.Uint16(p[7:])
	i.Supported = binary.LittleEndian.Uint32(p[9:])
	i.Current = binary.LittleEndian.Uint32(p[13:])
	copy(i.Class[:], p[17:20])
	i.Name = cstr(p[20 : 20+249])
	i.ShortName = cstr(p[269 : 269+11])
	return i, nil
}

// DecodeIndexList parses a Read Controller Index List reply.
func DecodeIndexList(p []byte) ([]uint16, error) {
	if len(p) < 2 {
		return nil, errors.New("mgmt: short index list")
	}
	n := int(binary.LittleEndian.Uint16(p))
	if len(p) < 2+2*n {
		return nil, errors.New("mgmt: truncated index list")
	}
	out := make([]uint16, n)
	for i := range out {
		out[i] = binary.LittleEndian.Uint16(p[2+2*i:])
	}
	return out, nil
}

// EncodeSetDiscoverable: mode 0 off, 1 general, 2 limited; timeout in seconds (0 = none).
func EncodeSetDiscoverable(mode uint8, timeout uint16) []byte {
	return []byte{mode, byte(timeout), byte(timeout >> 8)}
}

// EncodeSetLocalName builds the fixed 249+11 byte parameter (name truncated to 248 bytes).
func EncodeSetLocalName(name string) []byte {
	b := make([]byte, 249+11)
	if len(name) > 248 {
		n := 248
		for n > 0 && !utf8.RuneStart(name[n]) { // do not cut a rune in half
			n--
		}
		name = name[:n]
	}
	copy(b, name)
	return b
}

// EncodeAddUUID builds Add UUID for a 16-bit Bluetooth SIG UUID.
// svcHint holds bits 16..23 of the Class of Device the service contributes.
func EncodeAddUUID(uuid16 uint16, svcHint uint8) []byte {
	// 0000xxxx-0000-1000-8000-00805F9B34FB, little-endian on the wire.
	b := []byte{0xFB, 0x34, 0x9B, 0x5F, 0x80, 0x00, 0x00, 0x80, 0x00, 0x10, 0x00, 0x00, byte(uuid16), byte(uuid16 >> 8), 0, 0, svcHint}
	return b
}

// Key is a link key as carried by Load Link Keys / New Link Key.
type Key struct {
	Addr   bt.Addr
	Type   uint8
	Value  [16]byte
	PinLen uint8
}

func (k Key) encode() []byte {
	b := addrParam(k.Addr)
	b = append(b, k.Type)
	b = append(b, k.Value[:]...)
	return append(b, k.PinLen)
}

// EncodeLoadLinkKeys builds Load Link Keys (debug_keys = 0).
func EncodeLoadLinkKeys(keys []Key) []byte {
	b := []byte{0, byte(len(keys)), byte(len(keys) >> 8)}
	for _, k := range keys {
		b = append(b, k.encode()...)
	}
	return b
}

// NewLinkKey is the New Link Key event.
type NewLinkKey struct {
	StoreHint bool
	Key       Key
}

// DecodeNewLinkKey parses the New Link Key event params.
func DecodeNewLinkKey(p []byte) (NewLinkKey, error) {
	if len(p) < 1+7+1+16+1 {
		return NewLinkKey{}, errors.New("mgmt: short new link key")
	}
	var n NewLinkKey
	if p[7] != AddrBREDR {
		return NewLinkKey{}, ErrNotBREDR
	}
	n.StoreHint = p[0] != 0
	n.Key.Addr = addrFromWire(p[1:7])
	n.Key.Type = p[8]
	copy(n.Key.Value[:], p[9:25])
	n.Key.PinLen = p[25]
	return n, nil
}

// ErrNotBREDR is returned when decoding a device event for an LE address:
// only BR/EDR peers matter here, and an LE random address can collide with a
// BR/EDR one.
var ErrNotBREDR = errors.New("mgmt: not a BR/EDR address")

// DeviceConnected is the Device Connected event.
type DeviceConnected struct {
	Addr  bt.Addr
	Flags uint32
	Name  string // from EIR, may be empty
}

// DecodeDeviceConnected parses the event params.
func DecodeDeviceConnected(p []byte) (DeviceConnected, error) {
	if len(p) < 7+4+2 {
		return DeviceConnected{}, errors.New("mgmt: short device connected")
	}
	if p[6] != AddrBREDR {
		return DeviceConnected{}, ErrNotBREDR
	}
	d := DeviceConnected{Addr: addrFromWire(p[0:6]), Flags: binary.LittleEndian.Uint32(p[7:])}
	n := int(binary.LittleEndian.Uint16(p[11:]))
	if len(p) < 13+n {
		return DeviceConnected{}, errors.New("mgmt: truncated EIR")
	}
	d.Name = EIRName(p[13 : 13+n])
	return d, nil
}

// EIRName extracts the complete (0x09) or else shortened (0x08) local name from EIR data.
func EIRName(eir []byte) string {
	var short string
	for len(eir) >= 2 {
		l := int(eir[0])
		if l == 0 || l+1 > len(eir) {
			break
		}
		switch eir[1] {
		case 0x09:
			return string(eir[2 : 1+l])
		case 0x08:
			short = string(eir[2 : 1+l])
		}
		eir = eir[1+l:]
	}
	return short
}

// DecodeAddrEvent parses events that begin with a 7-byte address (Device
// Disconnected, PIN Code Request, User Confirmation Request, Auth Failed,
// Connect Failed). rest is whatever follows the address.
func DecodeAddrEvent(p []byte) (addr bt.Addr, addrType uint8, rest []byte, err error) {
	if len(p) < 7 {
		return addr, 0, nil, errors.New("mgmt: short address event")
	}
	if p[6] != AddrBREDR {
		return addr, p[6], nil, ErrNotBREDR
	}
	return addrFromWire(p[0:6]), p[6], p[7:], nil
}

// DecodeConnections parses a Get Connections reply.
func DecodeConnections(p []byte) ([]bt.Addr, error) {
	if len(p) < 2 {
		return nil, errors.New("mgmt: short connections")
	}
	n := int(binary.LittleEndian.Uint16(p))
	if len(p) < 2+7*n {
		return nil, errors.New("mgmt: truncated connections")
	}
	out := make([]bt.Addr, 0, n)
	for i := range n {
		off := 2 + 7*i
		if p[off+6] == AddrBREDR {
			out = append(out, addrFromWire(p[off:off+6]))
		}
	}
	return out, nil
}

// EncodePINReply builds PIN Code Reply with a fixed PIN.
func EncodePINReply(a bt.Addr, pin string) []byte {
	b := addrParam(a)
	var p [16]byte
	copy(p[:], pin)
	b = append(b, byte(len(pin)))
	return append(b, p[:]...)
}
