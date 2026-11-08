package avdtp

import (
	"errors"
	"time"

	"btspeaker/internal/bt"
)

// idleTimeout bounds a signalling channel that is idle or merely configured
// (or open with no media channel); a stream with media (even suspended) may
// stay quiet forever. A var so tests can shorten it.
var idleTimeout = 2 * time.Minute

// writeTimeout bounds each signalling reply write.
const writeTimeout = 5 * time.Second

const (
	// openTimeout bounds OPEN without a media transport channel following.
	openTimeout = 10 * time.Second
)

// Signal identifiers.
const (
	sigDiscover        = 0x01
	sigGetCapabilities = 0x02
	sigSetConfig       = 0x03
	sigGetConfig       = 0x04
	sigReconfigure     = 0x05
	sigOpen            = 0x06
	sigStart           = 0x07
	sigClose           = 0x08
	sigSuspend         = 0x09
	sigAbort           = 0x0A
	sigSecurityControl = 0x0B
	sigGetAllCaps      = 0x0C
	sigDelayReport     = 0x0D
)

// Message types.
const (
	msgCommand       = 0
	msgGeneralReject = 1
	msgAccept        = 2
	msgReject        = 3
)

// Packet types.
const (
	pktSingle   = 0
	pktStart    = 1
	pktContinue = 2
	pktEnd      = 3
)

// AVDTP / A2DP error codes.
const (
	errBadLength       = 0x11
	errBadACPSEID      = 0x12
	errSEPInUse        = 0x13
	errSEPNotInUse     = 0x14
	errBadServCategory = 0x17
	errBadPayload      = 0x18
	errNotSupported    = 0x19
	errInvalidCaps     = 0x1A
	errBadState        = 0x31

	errInvalidCodecType   = 0xC1
	errNotSuppCodecType   = 0xC2
	errInvalidSampleRate  = 0xC3
	errNotSuppSampleRate  = 0xC4
	errInvalidChannelMode = 0xC5
	errNotSuppChanMode    = 0xC6
	errInvalidSubbands    = 0xC7
	errInvalidAllocMethod = 0xC9
	errInvalidMinBitpool  = 0xCB
	errNotSuppMinBitpool  = 0xCC
	errInvalidMaxBitpool  = 0xCD
	errInvalidBlockLength = 0xDD
)

// Service categories.
const (
	catMediaTransport = 0x01
	catMediaCodec     = 0x07
)

// SBC codec information element bits.
const (
	modeMono   = 8
	modeDual   = 4
	modeStereo = 2
	modeJoint  = 1

	allocSNR      = 2
	allocLoudness = 1
)

const localSEID = 1

// maxReassembled bounds a fragmented signalling message (real ones are < 100 bytes).
const maxReassembled = 1024

type reassembly struct {
	label  byte
	msg    byte // message type from the START fragment
	nPkts  int
	got    int
	signal byte
	data   []byte
}

var sampleRateBits = []struct {
	rate int
	bit  byte
}{{16000, 0x80}, {32000, 0x40}, {44100, 0x20}, {48000, 0x10}}

// A2DP spec 4.3.2.3: block length bits 7..4 = 4, 8, 12, 16.
var blockBits = []struct {
	n   int
	bit byte
}{{4, 0x80}, {8, 0x40}, {12, 0x20}, {16, 0x10}}

func (s *Server) supportedRate(rate int) bool {
	s.ratesMu.Lock()
	defer s.ratesMu.Unlock()
	for _, r := range s.cfg.SampleRates {
		if r == rate {
			return true
		}
	}
	return false
}

// capabilities encodes the SEP capability list advertised to remotes.
func (s *Server) capabilities() []byte {
	var rates byte
	for _, r := range sampleRateBits {
		if s.supportedRate(r.rate) {
			rates |= r.bit
		}
	}
	return []byte{
		catMediaTransport, 0,
		catMediaCodec, 6, 0x00, 0x00, // audio, SBC
		rates | modeMono | modeDual | modeStereo | modeJoint,
		0xF0 | 0x0C | allocSNR | allocLoudness, // blocks 4/8/12/16, subbands 4/8, SNR+loudness
		2, byte(s.cfg.MaxBitpool),
	}
}

func (c sbcConfig) encode() []byte {
	var rb, bb byte
	for _, r := range sampleRateBits {
		if r.rate == c.rate {
			rb = r.bit
		}
	}
	for _, b := range blockBits {
		if b.n == c.blocks {
			bb = b.bit
		}
	}
	sb := byte(0x08)
	if c.subbands == 8 {
		sb = 0x04
	}
	return []byte{
		catMediaTransport, 0,
		catMediaCodec, 6, 0x00, 0x00,
		rb | c.mode,
		bb | sb | c.alloc,
		byte(c.minPool), byte(c.maxPool),
	}
}

// popcount1 reports whether exactly one bit is set.
func popcount1(b byte) bool { return b != 0 && b&(b-1) == 0 }

// parseCaps splits a service-capability list into category -> payload.
// ok=false with badCat set on a malformed list.
func parseCaps(b []byte) (caps map[byte][]byte, order []byte, badCat byte, ok bool) {
	caps = map[byte][]byte{}
	for len(b) > 0 {
		if len(b) < 2 || len(b) < 2+int(b[1]) {
			return nil, nil, 0, false
		}
		cat, l := b[0], int(b[1])
		if _, dup := caps[cat]; dup {
			return nil, nil, cat, false
		}
		caps[cat] = b[2 : 2+l]
		order = append(order, cat)
		b = b[2+l:]
	}
	return caps, order, 0, true
}

// validateSBC checks a codec IE payload (6 bytes) and returns the configuration,
// or an AVDTP error code.
func (s *Server) validateSBC(ie []byte) (sbcConfig, byte) {
	var c sbcConfig
	if len(ie) != 6 {
		return c, errBadPayload
	}
	if ie[0] != 0x00 {
		return c, errInvalidCodecType // media type must be audio
	}
	if ie[1] != 0x00 {
		return c, errNotSuppCodecType
	}
	rb := ie[2] & 0xF0
	if !popcount1(rb) {
		return c, errInvalidSampleRate
	}
	for _, r := range sampleRateBits {
		if r.bit == rb {
			c.rate = r.rate
		}
	}
	if !s.supportedRate(c.rate) {
		return c, errNotSuppSampleRate
	}
	c.mode = ie[2] & 0x0F
	if !popcount1(c.mode) {
		return c, errInvalidChannelMode
	}
	bb := ie[3] & 0xF0
	if !popcount1(bb) {
		return c, errInvalidBlockLength
	}
	for _, b := range blockBits {
		if b.bit == bb {
			c.blocks = b.n
		}
	}
	switch ie[3] & 0x0C {
	case 0x08:
		c.subbands = 4
	case 0x04:
		c.subbands = 8
	default:
		return c, errInvalidSubbands
	}
	c.alloc = ie[3] & 0x03
	if !popcount1(c.alloc) {
		return c, errInvalidAllocMethod
	}
	c.minPool, c.maxPool = int(ie[4]), int(ie[5])
	if c.minPool < 2 || c.minPool > 250 {
		return c, errInvalidMinBitpool
	}
	if c.maxPool < c.minPool || c.maxPool > 250 {
		return c, errInvalidMaxBitpool
	}
	if c.minPool > s.cfg.MaxBitpool {
		return c, errNotSuppMinBitpool
	}
	if c.maxPool > s.cfg.MaxBitpool {
		c.maxPool = s.cfg.MaxBitpool // tolerate: clamp to our advertised maximum
	}
	return c, 0
}

func hdr(label, pkt, msg byte) byte { return label<<4 | pkt<<2 | msg }

// signalling serves the signalling channel of d until it closes.
func (s *Server) signalling(d *device, c bt.Conn, first []byte) {
	defer func() {
		s.release(d)
		s.mu.Lock()
		if s.devs[d.addr] == d {
			delete(s.devs, d.addr)
		}
		s.mu.Unlock()
		c.Close()
	}()
	n := c.InMTU()
	if n < 672 {
		n = 672
	}
	buf := make([]byte, n)
	dl, _ := c.(interface{ SetDeadline(time.Time) error })
	// idleEnd moves only on accepted commands (or when media goes away), so
	// junk or stray packets cannot keep an idle channel open forever.
	var idleEnd time.Time
	for {
		if dl != nil {
			var t time.Time // none once a media channel exists (streaming or suspended)
			s.mu.Lock()
			if d.st < stOpen || d.media == nil {
				if idleEnd.IsZero() {
					idleEnd = time.Now().Add(idleTimeout)
				}
				t = idleEnd
			} else {
				idleEnd = time.Time{} // re-armed in full once media is gone
			}
			dl.SetDeadline(t) // under mu: must not overwrite a concurrent re-arm
			s.mu.Unlock()
		}
		var l int
		var err error
		if first != nil {
			l = copy(buf, first)
			first = nil
		} else {
			l, err = c.Read(buf)
		}
		if err != nil {
			// The deadline was armed before the media channel arrived: if one
			// exists now, the timeout no longer applies.
			var te interface{ Timeout() bool }
			if errors.As(err, &te) && te.Timeout() {
				s.mu.Lock()
				live := d.st >= stOpen && d.media != nil
				s.mu.Unlock()
				if live {
					continue
				}
			}
			return
		}
		label, sig, params, msg, ok := s.reassemble(d, buf[:l])
		if !ok {
			continue
		}
		if msg != msgCommand {
			continue // we never send commands; ignore stray responses
		}
		rsp := s.dispatch(d, label, sig, params)
		if rsp == nil {
			continue
		}
		if rsp[0]&3 == msgAccept {
			idleEnd = time.Now().Add(idleTimeout)
		}
		// Bounded write: a peer that never reads its replies must not block this
		// goroutine for good (the read deadline is cleared while media is live).
		if wd, ok := c.(interface{ SetWriteDeadline(time.Time) error }); ok {
			wd.SetWriteDeadline(time.Now().Add(writeTimeout))
		}
		if _, err := c.Write(rsp); err != nil {
			return
		}
	}
}

// reassemble parses the signalling header and joins fragmented messages.
func (s *Server) reassemble(d *device, p []byte) (label, sig byte, params []byte, msg byte, ok bool) {
	if len(p) < 1 {
		return
	}
	label, pkt, msg := p[0]>>4, p[0]>>2&3, p[0]&3
	switch pkt {
	case pktSingle:
		d.reasm = nil
		if len(p) < 2 {
			return
		}
		return label, p[1] & 0x3F, p[2:], msg, true
	case pktStart:
		d.reasm = nil
		if len(p) < 3 || p[1] < 2 || len(p)-3 > maxReassembled {
			return
		}
		d.reasm = &reassembly{label: label, msg: msg, nPkts: int(p[1]), got: 1, signal: p[2] & 0x3F, data: append([]byte(nil), p[3:]...)}
		return
	case pktContinue, pktEnd:
		r := d.reasm
		if r == nil || r.label != label || r.msg != msg {
			d.reasm = nil
			return
		}
		r.got++
		if len(r.data)+len(p)-1 > maxReassembled || (pkt == pktContinue && r.got >= r.nPkts) {
			d.reasm = nil
			return
		}
		r.data = append(r.data, p[1:]...)
		if pkt == pktContinue {
			return
		}
		d.reasm = nil
		if r.got != r.nPkts {
			return
		}
		return label, r.signal, r.data, r.msg, true
	}
	return
}

func accept(label, sig byte, params ...byte) []byte {
	return append([]byte{hdr(label, pktSingle, msgAccept), sig}, params...)
}

func reject(label, sig byte, params ...byte) []byte {
	return append([]byte{hdr(label, pktSingle, msgReject), sig}, params...)
}

func (s *Server) dispatch(d *device, label, sig byte, p []byte) []byte {
	switch sig {
	case sigDiscover:
		return s.doDiscover(d, label, p)
	case sigGetCapabilities, sigGetAllCaps:
		if len(p) != 1 {
			return reject(label, sig, errBadLength)
		}
		if p[0]>>2 != localSEID {
			return reject(label, sig, errBadACPSEID)
		}
		return accept(label, sig, s.capabilities()...)
	case sigSetConfig:
		return s.doSetConfig(d, label, p)
	case sigGetConfig:
		return s.doGetConfig(d, label, p)
	case sigReconfigure:
		return s.doReconfigure(d, label, p)
	case sigOpen:
		return s.doOpen(d, label, p)
	case sigStart:
		return s.doStart(d, label, p)
	case sigSuspend:
		return s.doSuspend(d, label, p)
	case sigClose:
		return s.doClose(d, label, sig, p)
	case sigAbort:
		return s.doClose(d, label, sig, p)
	case sigSecurityControl, sigDelayReport:
		return reject(label, sig, errNotSupported)
	}
	return []byte{hdr(label, pktSingle, msgGeneralReject), sig}
}

// ownerActive reports whether another device is actively streaming on the SEP
// (so it must not be reported free or evicted). Caller holds s.mu.
func (s *Server) ownerActive(d *device) bool {
	o := s.owner
	return o != nil && o != d && o.st == stStreaming && time.Since(o.lastRTP) <= staleStreamAfter
}

func (s *Server) doDiscover(d *device, label byte, p []byte) []byte {
	if len(p) != 0 {
		return reject(label, sigDiscover, errBadLength)
	}
	s.mu.Lock()
	inUse := s.ownerActive(d)
	s.mu.Unlock()
	b0 := byte(localSEID << 2)
	if inUse {
		b0 |= 2
	}
	return accept(label, sigDiscover, b0, 0x08) // audio, sink
}

func (s *Server) doSetConfig(d *device, label byte, p []byte) []byte {
	if len(p) < 2 {
		return reject(label, sigSetConfig, 0, errBadLength)
	}
	if p[0]>>2 != localSEID {
		return reject(label, sigSetConfig, 0, errBadACPSEID)
	}
	caps, order, badCat, ok := parseCaps(p[2:])
	if !ok && badCat != 0 {
		return reject(label, sigSetConfig, badCat, errBadServCategory)
	}
	if !ok {
		return reject(label, sigSetConfig, 0, errBadLength)
	}
	for _, cat := range order {
		if cat != catMediaTransport && cat != catMediaCodec {
			return reject(label, sigSetConfig, cat, errBadServCategory)
		}
	}
	if mt, ok := caps[catMediaTransport]; !ok || len(mt) != 0 {
		return reject(label, sigSetConfig, catMediaTransport, errBadServCategory)
	}
	ie, ok := caps[catMediaCodec]
	if !ok {
		return reject(label, sigSetConfig, catMediaCodec, errBadServCategory)
	}
	cfg, code := s.validateSBC(ie)
	if code != 0 {
		return reject(label, sigSetConfig, catMediaCodec, code)
	}
	s.mu.Lock()
	if o := s.owner; o != nil && o != d {
		if s.ownerActive(d) {
			s.mu.Unlock()
			return reject(label, sigSetConfig, 0, errSEPInUse)
		}
		// Stale owner (idle or merely configured/open): evict it.
		osig := o.sig
		s.mu.Unlock()
		s.release(o)
		osig.Close()
		s.mu.Lock()
		if s.owner != nil && s.owner != d {
			s.mu.Unlock()
			return reject(label, sigSetConfig, 0, errSEPInUse)
		}
	}
	if d.st != stIdle {
		s.mu.Unlock()
		return reject(label, sigSetConfig, 0, errSEPInUse)
	}
	s.owner = d
	d.cfg = cfg
	d.st = stConfigured
	s.mu.Unlock()
	return accept(label, sigSetConfig)
}

// ownerState checks that d owns the SEP and is at least in state min (and at
// most max); returns an AVDTP error code or 0.
func (s *Server) ownerState(d *device, min, max state) byte {
	if s.owner != d || d.st == stIdle {
		return errBadState
	}
	if d.st < min || d.st > max {
		return errBadState
	}
	return 0
}

func (s *Server) doGetConfig(d *device, label byte, p []byte) []byte {
	if len(p) != 1 {
		return reject(label, sigGetConfig, errBadLength)
	}
	if p[0]>>2 != localSEID {
		return reject(label, sigGetConfig, errBadACPSEID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if code := s.ownerState(d, stConfigured, stStreaming); code != 0 {
		return reject(label, sigGetConfig, code)
	}
	return accept(label, sigGetConfig, d.cfg.encode()...)
}

func (s *Server) doReconfigure(d *device, label byte, p []byte) []byte {
	if len(p) < 1 {
		return reject(label, sigReconfigure, 0, errBadLength)
	}
	if p[0]>>2 != localSEID {
		return reject(label, sigReconfigure, 0, errBadACPSEID)
	}
	caps, order, badCat, ok := parseCaps(p[1:])
	if !ok && badCat != 0 {
		return reject(label, sigReconfigure, badCat, errBadServCategory)
	}
	if !ok {
		return reject(label, sigReconfigure, 0, errBadLength)
	}
	for _, cat := range order {
		if cat != catMediaCodec {
			return reject(label, sigReconfigure, cat, errInvalidCaps)
		}
	}
	ie, ok := caps[catMediaCodec]
	if !ok {
		return reject(label, sigReconfigure, catMediaCodec, errBadServCategory)
	}
	cfg, code := s.validateSBC(ie)
	if code != 0 {
		return reject(label, sigReconfigure, catMediaCodec, code)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if code := s.ownerState(d, stOpen, stOpen); code != 0 {
		return reject(label, sigReconfigure, 0, code)
	}
	d.cfg = cfg
	return accept(label, sigReconfigure)
}

func (s *Server) doOpen(d *device, label byte, p []byte) []byte {
	if len(p) != 1 {
		return reject(label, sigOpen, errBadLength)
	}
	if p[0]>>2 != localSEID {
		return reject(label, sigOpen, errBadACPSEID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if code := s.ownerState(d, stConfigured, stConfigured); code != 0 {
		return reject(label, sigOpen, code)
	}
	d.st = stOpen
	d.openTok++
	tok := d.openTok
	if d.openTmr != nil {
		d.openTmr.Stop()
	}
	d.openTmr = time.AfterFunc(openTimeout, func() {
		s.mu.Lock()
		stale := d.st == stOpen && d.media == nil && d.openTok == tok
		sig := d.sig
		s.mu.Unlock()
		if stale && sig != nil {
			sig.Close() // signalling teardown releases the SEP
		}
	})
	return accept(label, sigOpen)
}

// seids validates the SEID list of START/SUSPEND; returns reject params on error.
func seidsOK(p []byte) (bad []byte) {
	if len(p) == 0 {
		return []byte{0, errBadLength}
	}
	for _, b := range p {
		if b>>2 != localSEID {
			return []byte{b, errBadACPSEID}
		}
	}
	return nil
}

// startMediaWait bounds how long START waits for the media channel.
const startMediaWait = time.Second

func (s *Server) doStart(d *device, label byte, p []byte) []byte {
	if bad := seidsOK(p); bad != nil {
		return reject(label, sigStart, bad...)
	}
	// START may overtake the media channel registration: give it a moment.
	// Wait without holding any lock (sinkMu before mu).
	timeout := time.NewTimer(startMediaWait)
	defer timeout.Stop()
	for waiting := true; waiting; {
		s.mu.Lock()
		wait := s.owner == d && d.st == stOpen && d.media == nil
		ch := s.changed
		s.mu.Unlock()
		if !wait {
			break
		}
		select {
		case <-ch:
		case <-timeout.C:
			waiting = false
		}
	}
	// sinkMu serializes sink transitions with state changes (see stopSink).
	s.sinkMu.Lock()
	defer s.sinkMu.Unlock()
	s.mu.Lock()
	if code := s.ownerState(d, stOpen, stStreaming); code != 0 {
		s.mu.Unlock()
		return reject(label, sigStart, p[0], code)
	}
	if d.media == nil {
		s.mu.Unlock()
		return reject(label, sigStart, p[0], errBadState)
	}
	cfg := d.cfg
	already := d.st == stStreaming
	d.st = stStreaming
	d.lastRTP = time.Now() // grace period until the first packet
	if !already {          // a duplicate START must not reset the decoder mid-stream
		d.gen++
	}
	start := !d.sinkOn && !already
	if start {
		d.sinkOn = true
	}
	s.mu.Unlock()
	if start {
		s.sink.Start(cfg.rate, cfg.channels())
	}
	return accept(label, sigStart)
}

func (s *Server) doSuspend(d *device, label byte, p []byte) []byte {
	if bad := seidsOK(p); bad != nil {
		return reject(label, sigSuspend, bad...)
	}
	s.mu.Lock()
	if code := s.ownerState(d, stStreaming, stStreaming); code != 0 {
		s.mu.Unlock()
		return reject(label, sigSuspend, p[0], code)
	}
	d.st = stOpen
	s.mu.Unlock()
	s.stopSink(d, true)
	return accept(label, sigSuspend)
}

func (s *Server) doClose(d *device, label, sig byte, p []byte) []byte {
	if len(p) != 1 {
		return reject(label, sig, errBadLength)
	}
	if p[0]>>2 != localSEID {
		return reject(label, sig, errBadACPSEID)
	}
	s.mu.Lock()
	code := byte(0)
	owner := s.owner == d
	if sig == sigClose {
		code = s.ownerState(d, stOpen, stStreaming)
	}
	s.mu.Unlock()
	if code != 0 {
		return reject(label, sig, code)
	}
	if sig == sigClose || owner { // ABORT is always accepted; release only if ours
		s.release(d)
	}
	return accept(label, sig)
}
