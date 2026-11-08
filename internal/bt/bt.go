// Package bt holds the small types shared by the Bluetooth protocol packages
// (sdp, avdtp, avrcp) and the socket layer (hci), so they don't import each other.
package bt

import (
	"fmt"
	"io"
)

// Addr is a BD_ADDR in display order (Addr[0] is the most significant byte,
// "AA" in "AA:BB:CC:DD:EE:FF"). The kernel's wire/sockaddr order is reversed;
// conversion is the hci/mgmt packages' job.
type Addr [6]byte

func (a Addr) String() string {
	return fmt.Sprintf("%02X:%02X:%02X:%02X:%02X:%02X", a[0], a[1], a[2], a[3], a[4], a[5])
}

// ParseAddr parses "AA:BB:CC:DD:EE:FF" (case-insensitive).
func ParseAddr(s string) (Addr, error) {
	var a Addr
	if len(s) != 17 {
		return a, fmt.Errorf("bad bluetooth address %q", s)
	}
	for i := range 6 {
		if i > 0 && s[i*3-1] != ':' {
			return a, fmt.Errorf("bad bluetooth address %q", s)
		}
		hi, ok1 := hexVal(s[i*3])
		lo, ok2 := hexVal(s[i*3+1])
		if !ok1 || !ok2 {
			return a, fmt.Errorf("bad bluetooth address %q", s)
		}
		b := hi<<4 | lo
		a[i] = b
	}
	return a, nil
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// Conn is one accepted L2CAP SOCK_SEQPACKET channel: each Read returns exactly
// one L2CAP SDU (pass a buffer of at least InMTU bytes), each Write sends one.
type Conn interface {
	io.ReadWriteCloser
	RemoteAddr() Addr
	PSM() uint16
	InMTU() int  // max SDU we can receive
	OutMTU() int // max SDU we may send
}
