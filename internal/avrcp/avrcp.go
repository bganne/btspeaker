// Package avrcp implements the minimal AVRCP target needed for absolute volume
// (category 2) over AVCTP on PSM 23.
package avrcp

import (
	"errors"
	"sync"
	"time"

	"btspeaker/internal/bt"
)

const PSM = 23

// Read deadlines (vars so tests can shorten them). A source often opens AVRCP at
// connect and then stays silent for minutes, so after the first PDU a channel is
// only closed after idleTimeout of silence, and never while the same peer has an
// AVDTP session. A stray peer must not hold a slot forever.
var (
	firstPDUTimeout = 10 * time.Second
	idleTimeout     = 30 * time.Minute
	// writeTimeout bounds each write: a peer that sends but never reads must not
	// block a writer (and hold the AVRCP slot) once its socket buffer is full.
	writeTimeout = 5 * time.Second
)

// write sends p with a write deadline; on failure it closes c, which also ends
// HandleConn's read.
func write(c bt.Conn, p []byte) error {
	if wd, ok := c.(interface{ SetWriteDeadline(time.Time) error }); ok {
		wd.SetWriteDeadline(time.Now().Add(writeTimeout))
	}
	_, err := c.Write(p)
	if err != nil {
		c.Close()
	}
	return err
}

// Volume is implemented by *player.Player (0..127).
type Volume interface {
	SetVolume(v uint8)
	Volume() uint8
}

const (
	pidAVRCP = 0x110E

	// AV/C command types / response codes.
	ctypeControl     = 0x0
	ctypeStatus      = 0x1
	ctypeNotify      = 0x3
	respNotImpl      = 0x8
	respAccepted     = 0x9
	respRejected     = 0xA
	respStable       = 0xC
	respChanged      = 0xD
	respInterim      = 0xF
	subunitPanel     = 0x48 // type 0x09 << 3, id 0
	opVendorDep      = 0x00
	opUnitInfo       = 0x30
	opSubunitInfo    = 0x31
	opPassThrough    = 0x7C
	passVolUp        = 0x41 // PASS THROUGH operation ids
	passVolDown      = 0x42
	volStep          = 8 // of 127, per volume up/down press
	companyBTSIG     = 0x001958
	pduGetCaps       = 0x10
	pduRegisterNotif = 0x31
	pduSetAbsVolume  = 0x50
	eventVolume      = 0x0D

	errInvalidCommand = 0x00
	errInvalidParam   = 0x01
	errInternal       = 0x03
)

type Server struct {
	vol Volume
	// HasSession (optional) reports whether a peer currently has an AVDTP
	// session; set before serving.
	HasSession func(bt.Addr) bool
	// MayControlVolume (optional) reports whether a may set the absolute
	// volume (the active AVDTP source, or any session while no one streams).
	MayControlVolume func(bt.Addr) bool

	mu   sync.Mutex
	regs map[bt.Conn]byte // conns with a pending volume notification -> AVCTP label
	busy map[bt.Conn]bool // conns with a notification write in flight
}

func NewServer(v Volume) *Server {
	return &Server{vol: v, regs: map[bt.Conn]byte{}, busy: map[bt.Conn]bool{}}
}

// HandleConn serves one accepted PSM 23 channel until it closes (blocks).
func (s *Server) HandleConn(c bt.Conn) {
	defer func() {
		s.mu.Lock()
		delete(s.regs, c)
		s.mu.Unlock()
		c.Close()
	}()
	n := c.InMTU()
	if n < 512 {
		n = 512
	}
	buf := make([]byte, n)
	dl, _ := c.(interface{ SetReadDeadline(time.Time) error })
	peer := c.RemoteAddr()
	first := true
	firstEnd := time.Now().Add(firstPDUTimeout) // absolute: junk must not re-arm it
	for {
		if dl != nil {
			d := firstEnd
			if !first {
				d = time.Now().Add(idleTimeout)
			}
			dl.SetReadDeadline(d)
		}
		l, err := c.Read(buf)
		if err != nil {
			var te interface{ Timeout() bool }
			if errors.As(err, &te) && te.Timeout() && s.HasSession != nil && s.HasSession(peer) {
				firstEnd = time.Now().Add(firstPDUTimeout) // don't spin on a past deadline
				continue                                   // silent but the audio link is up: keep volume control
			}
			return
		}
		if first && isAVRCP(buf[:l]) { // junk must not earn the long idle timeout
			first = false
		}
		rsp, volChanged := s.process(c, buf[:l])
		if rsp != nil {
			if err := write(c, rsp); err != nil {
				return
			}
		}
		if volChanged {
			// After the ACCEPTED reply, on this goroutine: ordered, no re-entrant write.
			s.NotifyVolume(s.vol.Volume())
		}
	}
}

// NotifyVolume sends a CHANGED volume notification on every connection that
// registered for EVENT_VOLUME_CHANGED (registration is one-shot).
func (s *Server) NotifyVolume(v uint8) {
	s.mu.Lock()
	regs := s.regs
	s.regs = map[bt.Conn]byte{}
	var send []bt.Conn
	for c := range regs {
		if !s.busy[c] { // a stuck peer gets at most one blocked writer
			s.busy[c] = true
			send = append(send, c)
		} else {
			s.regs[c] = regs[c] // keep the one-shot registration for the next change
		}
	}
	s.mu.Unlock()
	for _, c := range send {
		pkt := avctp(regs[c], true, false, pidAVRCP, vendor(respChanged, subunitPanel, pduRegisterNotif, []byte{eventVolume, v & 0x7F}))
		// Own goroutine: one peer blocking up to writeTimeout must not delay the others or the caller.
		go func() {
			write(c, pkt)
			s.mu.Lock()
			delete(s.busy, c)
			s.mu.Unlock()
		}()
	}
}

func avctp(label byte, response, ipid bool, pid uint16, payload []byte) []byte {
	b0 := label << 4
	if response {
		b0 |= 2
	}
	if ipid {
		b0 |= 1
	}
	return append([]byte{b0, byte(pid >> 8), byte(pid)}, payload...)
}

// vendor builds a Bluetooth SIG vendor dependent AV/C frame.
func vendor(ctype, subunit, pdu byte, params []byte) []byte {
	out := []byte{ctype, subunit, opVendorDep, 0x00, 0x19, 0x58, pdu, 0x00, byte(len(params) >> 8), byte(len(params))}
	return append(out, params...)
}

// isAVRCP reports whether p is a well-formed unfragmented AVCTP packet carrying an
// AV/C frame for the AVRCP profile.
func isAVRCP(p []byte) bool {
	return len(p) >= 6 && p[0]>>2&3 == 0 && uint16(p[1])<<8|uint16(p[2]) == pidAVRCP
}

// process handles one AVCTP packet and reports whether a PASS THROUGH step changed the volume (the
// caller then owes registered peers an EVENT_VOLUME_CHANGED).
func (s *Server) process(c bt.Conn, p []byte) (rsp []byte, volChanged bool) {
	if len(p) < 3 {
		return nil, false
	}
	label := p[0] >> 4
	ptype := p[0] >> 2 & 3
	isResp := p[0]&2 != 0
	if ptype != 0 || isResp {
		return nil, false // fragmented AVCTP is unsupported; responses need no answer
	}
	pid := uint16(p[1])<<8 | uint16(p[2])
	if pid != pidAVRCP {
		return avctp(label, true, true, pid, nil), false
	}
	frame := p[3:]
	if len(frame) < 3 {
		return nil, false
	}
	ctype, subunit, op := frame[0]&0x0F, frame[1], frame[2]
	ops := frame[3:]
	reply := func(code byte, operands []byte) []byte {
		return avctp(label, true, false, pid, append([]byte{code, subunit, op}, operands...))
	}
	switch op {
	case opUnitInfo:
		return reply(respStable, []byte{0x07, 0x48, 0x00, 0x19, 0x58}), false
	case opSubunitInfo:
		return reply(respStable, []byte{0x07, 0x48, 0xFF, 0xFF, 0xFF}), false
	case opVendorDep:
		return s.vendorDep(c, label, ctype, subunit, ops), false
	case opPassThrough:
		code, changed := s.passThrough(c, ctype, ops)
		return reply(code, ops), changed
	}
	return reply(respNotImpl, ops), false
}

// passThrough handles PASS THROUGH (operands: state|op_id, length, data) and
// returns the response code and whether the volume was stepped. Volume up/down is mandatory for category 2: it
// steps the volume on press, for sources that do not use absolute volume.
func (s *Server) passThrough(c bt.Conn, ctype byte, ops []byte) (byte, bool) {
	if ctype != ctypeControl || len(ops) < 2 || len(ops) != 2+int(ops[1]) {
		return respNotImpl, false
	}
	opID, released := ops[0]&0x7F, ops[0]&0x80 != 0
	if opID != passVolUp && opID != passVolDown {
		return respNotImpl, false
	}
	if s.MayControlVolume != nil && !s.MayControlVolume(c.RemoteAddr()) {
		return respRejected, false
	}
	if !released {
		v := int(s.vol.Volume())
		if opID == passVolUp {
			v = min(v+volStep, 127)
		} else {
			v = max(v-volStep, 0)
		}
		s.vol.SetVolume(uint8(v))
	}
	return respAccepted, !released
}

func (s *Server) vendorDep(c bt.Conn, label, ctype, subunit byte, ops []byte) []byte {
	reply := func(code, pdu byte, params []byte) []byte {
		return avctp(label, true, false, pidAVRCP, vendor(code, subunit, pdu, params))
	}
	notImpl := func() []byte {
		return avctp(label, true, false, pidAVRCP, append([]byte{respNotImpl, subunit, opVendorDep}, ops...))
	}
	if len(ops) < 7 || (uint32(ops[0])<<16|uint32(ops[1])<<8|uint32(ops[2])) != companyBTSIG {
		return notImpl()
	}
	pdu, ptype := ops[3], ops[4]&3
	plen := int(ops[5])<<8 | int(ops[6])
	params := ops[7:]
	if ptype != 0 || plen != len(params) {
		return reply(respRejected, pdu, []byte{errInvalidCommand})
	}
	rej := func(code byte) []byte { return reply(respRejected, pdu, []byte{code}) }
	switch pdu {
	case pduGetCaps:
		if ctype != ctypeStatus {
			return notImpl()
		}
		if len(params) != 1 {
			return rej(errInvalidParam)
		}
		switch params[0] {
		case 0x02:
			return reply(respStable, pdu, []byte{0x02, 1, 0x00, 0x19, 0x58})
		case 0x03:
			return reply(respStable, pdu, []byte{0x03, 1, eventVolume})
		}
		return rej(errInvalidParam)
	case pduRegisterNotif:
		if ctype != ctypeNotify {
			return notImpl()
		}
		if len(params) != 5 || params[0] != eventVolume {
			return rej(errInvalidParam)
		}
		s.mu.Lock()
		s.regs[c] = label
		s.mu.Unlock()
		return reply(respInterim, pdu, []byte{eventVolume, s.vol.Volume() & 0x7F})
	case pduSetAbsVolume:
		if len(params) != 1 {
			return rej(errInvalidParam)
		}
		if ctype != ctypeControl {
			return notImpl()
		}
		// Only the streaming source may change the volume, not just any paired peer.
		if s.MayControlVolume != nil && !s.MayControlVolume(c.RemoteAddr()) {
			return rej(errInternal)
		}
		v := params[0] & 0x7F
		s.vol.SetVolume(v)
		return reply(respAccepted, pdu, []byte{v})
	}
	return rej(errInvalidCommand)
}
