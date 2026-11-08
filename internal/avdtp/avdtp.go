// Package avdtp implements the A2DP sink side of AVDTP (signalling + media) over
// kernel L2CAP channels on PSM 25, decoding SBC into an AudioSink.
package avdtp

import (
	"sync"
	"sync/atomic"
	"time"

	"btspeaker/internal/bt"
	"btspeaker/internal/sbc"
)

const PSM = 25

// AudioSink receives decoded audio. Implemented by *player.Player.
type AudioSink interface {
	Start(rate, channels int)
	Write(pcm []int16)
	Stop() // drain queued audio, then release (SUSPEND)
	Drop() // discard queued audio and release now (CLOSE, ABORT, disconnect)
}

type Config struct {
	SampleRates []int // rates the ALSA device supports; only these are advertised (subset of 16k/32k/44.1k/48k)
	MaxBitpool  int   // advertised max SBC bitpool, 2..250; 0 -> 53 (NewServer fallback, A2DP "high quality"; the CLI default is 76); higher = SBC XQ
}

// StreamInfo is the current stream for status display.
type StreamInfo struct {
	Device     bt.Addr
	State      string // "idle","configured","open","streaming","paused"
	SampleRate int
	Channels   int
	Mode       string // "joint stereo", ...
	Subbands   int
	Blocks     int
	Bitpool    int // max negotiated
}

// decoder is the subset of *sbc.Decoder used here (injectable for tests).
type decoder interface {
	Decode(frame []byte, pcm []int16) (consumed, n int, info sbc.FrameInfo, err error)
	Reset()
}

type state int

const (
	stIdle state = iota
	stConfigured
	stOpen
	stStreaming
)

func (s state) String() string {
	return [...]string{"idle", "configured", "open", "streaming"}[s]
}

// sbcConfig is a negotiated (single-valued) SBC configuration.
type sbcConfig struct {
	rate     int
	mode     byte // channel mode bit: 8 mono, 4 dual, 2 stereo, 1 joint
	blocks   int
	subbands int
	alloc    byte // 2 SNR, 1 loudness
	minPool  int
	maxPool  int
}

func (c sbcConfig) channels() int {
	if c.mode == modeMono {
		return 1
	}
	return 2
}

func (c sbcConfig) modeName() string {
	switch c.mode {
	case modeMono:
		return "mono"
	case modeDual:
		return "dual channel"
	case modeStereo:
		return "stereo"
	}
	return "joint stereo"
}

// device is the per-remote-device state; all fields except reasm are guarded
// by Server.mu.
type device struct {
	addr    bt.Addr
	sig     bt.Conn
	media   bt.Conn
	st      state
	cfg     sbcConfig
	gen     int         // incremented on START so the media loop resets its decoder
	sinkOn  bool        // sink.Start was called without matching Stop
	reasm   *reassembly // signalling goroutine only, not guarded by Server.mu
	openTok int         // identifies the current OPEN for its media-arrival timeout
	openTmr *time.Timer
	lastRTP time.Time // last valid, non-duplicate media packet while streaming
}

// staleStreamAfter is how long a streaming owner may go without an accepted RTP
// packet before another source's SET_CONFIGURATION may evict it. A variable so
// tests can shorten it.
var staleStreamAfter = 30 * time.Second

type Server struct {
	cfg  Config
	sink AudioSink
	// ratesMu guards cfg.SampleRates, which SetSampleRates may replace.
	ratesMu sync.Mutex
	// newDecoder builds a per-stream SBC decoder; overridable in tests.
	newDecoder func() decoder

	// sinkMu serializes sink.Start/Stop calls so a concurrent stop can't be
	// overtaken by a start. Lock order: sinkMu before mu.
	sinkMu sync.Mutex
	mu     sync.Mutex
	devs   map[bt.Addr]*device
	owner  *device // device holding the (single) SEP, nil if free
	// changed is closed (and replaced) when a media channel registers or a
	// device is released, waking doStart; see wakeLocked.
	changed chan struct{}

	// Media diagnostics since start (see Stats).
	lostPkts, badFrames, concealedUs, dupPkts, resyncs atomic.Uint64
}

// Stats are cumulative media-path counters since start.
type Stats struct {
	LostPackets uint64 // RTP sequence/timestamp gaps
	BadFrames   uint64 // packets with an undecodable/out-of-spec SBC frame
	ConcealedMs uint64 // silence substituted for lost or bad audio
	DroppedDup  uint64 // duplicate or out-of-order RTP packets dropped
	Resyncs     uint64 // implausible RTP sequence jumps that restarted the decoder
}

func (s *Server) Stats() Stats {
	return Stats{LostPackets: s.lostPkts.Load(), BadFrames: s.badFrames.Load(), ConcealedMs: s.concealedUs.Load() / 1000, DroppedDup: s.dupPkts.Load(), Resyncs: s.resyncs.Load()}
}

func NewServer(cfg Config, sink AudioSink) *Server {
	if cfg.MaxBitpool <= 0 {
		cfg.MaxBitpool = 53
	}
	if len(cfg.SampleRates) == 0 {
		cfg.SampleRates = []int{44100, 48000}
	}
	return &Server{
		cfg:        cfg,
		sink:       sink,
		newDecoder: func() decoder { return sbc.NewDecoder() },
		devs:       map[bt.Addr]*device{},
		changed:    make(chan struct{}),
	}
}

// wakeLocked wakes goroutines waiting on s.changed. Caller holds s.mu.
func (s *Server) wakeLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// SetSampleRates replaces the supported rates (e.g. after a successful late
// device probe). It affects later GET_CAPABILITIES and SET_CONFIGURATION; an
// already configured stream is unaffected.
func (s *Server) SetSampleRates(rates []int) {
	if len(rates) == 0 {
		return
	}
	s.ratesMu.Lock()
	s.cfg.SampleRates = append([]int(nil), rates...)
	s.ratesMu.Unlock()
}

// HasSession reports whether a is currently connected on PSM 25 (used by AVRCP
// to keep its channel open while audio signalling is up).
func (s *Server) HasSession(a bt.Addr) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.devs[a] != nil
}

// MayControlVolume reports whether a may set the absolute volume: it owns the
// SEP, or nobody does and a has a signalling session (initial volume sync
// arrives before SET_CONFIGURATION).
func (s *Server) MayControlVolume(a bt.Addr) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.owner != nil {
		return s.owner.addr == a
	}
	return s.devs[a] != nil
}

// Streams returns current streams (normally 0 or 1).
// pausedAfter is how long a streaming SEP may go without audio before
// Streams reports it as paused.
const pausedAfter = time.Second

func (s *Server) Streams() []StreamInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []StreamInfo
	if d := s.owner; d != nil {
		st := d.st.String()
		if d.st == stStreaming && time.Since(d.lastRTP) > pausedAfter {
			// Many sources pause without SUSPEND: the AVDTP state stays
			// streaming while no audio arrives.
			st = "paused"
		}
		out = append(out, StreamInfo{
			Device:     d.addr,
			State:      st,
			SampleRate: d.cfg.rate,
			Channels:   d.cfg.channels(),
			Mode:       d.cfg.modeName(),
			Subbands:   d.cfg.subbands,
			Blocks:     d.cfg.blocks,
			Bitpool:    d.cfg.maxPool,
		})
	}
	return out
}

// HandleConn serves one accepted PSM 25 channel until it closes. The first
// channel from a device is signalling, the next one (after OPEN) is media.
// Blocks; call in its own goroutine.
func (s *Server) HandleConn(c bt.Conn) {
	addr := c.RemoteAddr()
	s.mu.Lock()
	d := s.devs[addr]
	switch {
	case d == nil:
		d = &device{addr: addr, sig: c}
		s.devs[addr] = d
		s.mu.Unlock()
		s.signalling(d, c, nil)
	case d.media == nil && d.st == stOpen && s.owner == d:
		d.media = c
		if d.openTmr != nil {
			d.openTmr.Stop()
			d.openTmr = nil
		}
		s.wakeLocked()
		s.mu.Unlock()
		s.mediaLoop(d, c)
	default:
		s.replaceSession(d, c, nil) // unlocks s.mu
	}
}

// replaceSession serves c as the new signalling channel of d's address. An
// existing session d (nil if gone) is stale (link loss not yet detected,
// supervision timeout pending): it is evicted, media included, and replaced by
// a fresh device state. first, if non-nil, is a signalling packet already read
// from c. Called with s.mu held, which it releases; blocks until c closes.
func (s *Server) replaceSession(old *device, c bt.Conn, first []byte) {
	d := &device{addr: c.RemoteAddr(), sig: c}
	s.devs[d.addr] = d
	s.mu.Unlock()
	if old != nil {
		osig := old.sig
		s.release(old)
		osig.Close()
	}
	s.signalling(d, c, first)
}

// stopSink stops the sink if this device started it, draining queued audio
// when drain is set (SUSPEND) or dropping it otherwise. Caller must not hold s.mu.
func (s *Server) stopSink(d *device, drain bool) {
	s.sinkMu.Lock()
	defer s.sinkMu.Unlock()
	s.mu.Lock()
	on := d.sinkOn
	d.sinkOn = false
	s.mu.Unlock()
	if !on {
		return
	}
	if drain {
		s.sink.Stop()
	} else {
		s.sink.Drop()
	}
}

// release returns d's SEP to idle: stops audio, closes media, frees ownership.
func (s *Server) release(d *device) {
	s.stopSink(d, false)
	s.mu.Lock()
	media := d.media
	d.media = nil
	if d.openTmr != nil {
		d.openTmr.Stop()
		d.openTmr = nil
	}
	d.st = stIdle
	if s.owner == d {
		s.owner = nil
	}
	s.wakeLocked()
	s.mu.Unlock()
	if media != nil {
		media.Close()
	}
}
