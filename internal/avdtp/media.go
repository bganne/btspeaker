package avdtp

import (
	"errors"
	"log"
	"time"

	"btspeaker/internal/bt"
	"btspeaker/internal/sbc"
)

// maxSeqJump is the largest RTP sequence step treated as loss (forward) or
// reordering (backward); beyond it the stream is resynchronised.
const maxSeqJump = 1000

// maxDupRun consecutive duplicate/reordered drops mean the sequence restarted: resync.
const maxDupRun = 16

// maxTSOnlyGaps is how many consecutive timestamp-only gaps (contiguous
// sequence numbers) are concealed before the stream is judged to stamp RTP
// off the sample clock (ms, 90 kHz ticks) and timestamp-only concealment stops.
const maxTSOnlyGaps = 8

// minTSCleanPackets is how many consecutive packets with a consistent timeline
// must precede a timestamp-only gap before it is concealed. A source stamping
// RTP off a non-sample clock never has such a run, so it gets no silence at all.
const minTSCleanPackets = 16

// conceal appends n frames of faded silence to out and accounts them as concealed audio.
func (s *Server) conceal(cc *concealer, out []int16, n, rate int) []int16 {
	out = cc.silence(out, n)
	s.concealedUs.Add(uint64(n) * 1e6 / uint64(rate))
	return out
}

// mediaLoop reads RTP/SBC packets from the media channel of d until it closes.
func (s *Server) mediaLoop(d *device, c bt.Conn) {
	dec := s.newDecoder()
	handed := false // c became a signalling channel: it must stay open
	defer func() {
		s.mu.Lock()
		owns := d.media == c
		if owns {
			d.media = nil
			// Transport gone without CLOSE: back to configured so OPEN can retry.
			if d.st > stConfigured {
				d.st = stConfigured
			}
		}
		if owns {
			// The pending signalling Read may have no deadline (armed while media
			// was live): re-arm it so a silent peer cannot hold its slot forever.
			// Under mu so the signalling loop cannot overwrite it afterwards.
			if dl, ok := d.sig.(interface{ SetDeadline(time.Time) error }); ok {
				dl.SetDeadline(time.Now().Add(idleTimeout))
			}
		}
		s.mu.Unlock()
		if owns { // a newer channel's stream must not be cut by this stale loop
			s.stopSink(d, false)
		}
		if !handed {
			c.Close()
		}
	}()
	n := c.InMTU()
	if n < 672 {
		n = 672
	}
	buf := make([]byte, n)
	st := newStream(s, d, c, dec)
	for first := true; ; first = false {
		l, err := c.Read(buf)
		if err != nil {
			return
		}
		if first && !isRTP(buf[:l]) {
			// Not media: a new signalling channel (e.g. a reconnect before the
			// old session timed out). Replace the stale session with it.
			handed = true
			pkt := append([]byte(nil), buf[:l]...)
			s.mu.Lock()
			if d.media == c {
				d.media = nil
			}
			old := s.devs[d.addr]
			s.replaceSession(old, c, pkt) // unlocks s.mu
			return
		}
		s.mu.Lock()
		streaming := d.st == stStreaming && s.owner == d && d.media == c
		g := d.gen
		cfg := d.cfg
		s.mu.Unlock()
		if !streaming {
			continue
		}
		st.begin(cfg, g)
		payload := rtpPayload(buf[:l])
		if len(payload) < 1 {
			continue
		}
		if payload[0]&0x80 != 0 {
			// Fragmented SBC frames: unsupported, drop. Before the sequence
			// update, so the next packet sees the loss and conceals it.
			continue
		}
		seq := uint16(buf[2])<<8 | uint16(buf[3])
		ts := uint32(buf[4])<<24 | uint32(buf[5])<<16 | uint32(buf[6])<<8 | uint32(buf[7])
		ssrc := uint32(buf[8])<<24 | uint32(buf[9])<<16 | uint32(buf[10])<<8 | uint32(buf[11])
		st.onPacket(seq, ts, ssrc, payload)
	}
}

// stream is the per-media-channel RTP state: sequence/timestamp tracking, loss
// concealment and decoding. onPacket does not allocate.
type stream struct {
	s   *Server
	d   *device
	c   bt.Conn
	dec decoder
	cfg sbcConfig
	gen int // d.gen this state belongs to; -1 before the first packet

	pcm    [256]int16
	out    []int16 // room for 15 decoded frames plus up to 100 ms of concealment; reused, sink.Write doesn't retain
	cc     concealer
	budget concealBudget

	dups   int // consecutive duplicate/reordered drops
	tsOnly int // consecutive timestamp-only gaps; sticks at maxTSOnlyGaps until a stream reset
	// tsClean counts consecutive packets whose timestamp matched the decoded timeline.
	tsClean  int
	warned   bool
	ssrc     uint32
	haveSSRC bool
}

// outCap is the initial st.out capacity in samples (~40 KB): 15 frames plus
// 100 ms of fill at 48 kHz stereo plus the fade/concealment budget, with
// margin, so the steady state never reallocates.
const outCap = 20000

func newStream(s *Server, d *device, c bt.Conn, dec decoder) *stream {
	return &stream{s: s, d: d, c: c, dec: dec, gen: -1, out: make([]int16, 0, outCap)}
}

// begin adopts cfg and, when the START generation changed, resets all stream state.
func (st *stream) begin(cfg sbcConfig, g int) {
	st.cfg = cfg
	if g != st.gen {
		st.dec.Reset()
		st.cc.reset(cfg.channels())
		st.gen = g
		st.budget.reset(cfg.rate, time.Now())
		st.tsOnly = 0
		st.tsClean = 0
		st.haveSSRC = false
	}
}

// onPacket processes one RTP packet; payload starts with the SBC media payload
// header byte. Decoded audio (and concealment) goes to the sink.
func (st *stream) onPacket(seq uint16, ts, cur uint32, payload []byte) {
	s, d, c, dec, cc, budget, cfg := st.s, st.d, st.c, st.dec, &st.cc, &st.budget, st.cfg
	ch := cfg.channels()
	g := st.gen
	pcm := st.pcm[:]
	lost, tsPause := false, false
	missing := uint64(1) // packets lost in this gap
	out := st.out[:0]
	ssrcChg := st.haveSSRC && cur != st.ssrc
	st.ssrc, st.haveSSRC = cur, true
	if cc.haveSeq {
		delta := int16(seq - cc.seq)
		resync := ssrcChg // a new source: its sequence space is unrelated
		if !resync {
			switch {
			case delta > 0 && delta <= maxSeqJump:
				lost = delta != 1
				if lost {
					missing = uint64(delta - 1)
				}
			case delta <= 0 && delta >= -maxSeqJump && st.dups < maxDupRun:
				st.dups++
				s.dupPkts.Add(1)
				return // duplicate or reordered
			default:
				// Implausible jump (source restart, or a hostile peer): resync on
				// this packet instead of muting until the next START.
				// Also reached after maxDupRun consecutive backward packets: a
				// restarted sequence landing inside the backward window.
				resync = true
			}
		}
		if resync {
			s.resyncs.Add(1)
			dec.Reset()
			// Fade the old tail out before reset zeroes it, and fade the new source in.
			if cc.last != [2]int16{} {
				out = s.conceal(cc, out, budget.take(fadeFrames, time.Now()), cfg.rate)
			}
			cc.reset(ch)
			cc.fadeIn = fadeFrames
			st.tsOnly = 0
			st.tsClean = 0
		}
	}
	st.dups = 0
	cc.seq, cc.haveSeq = seq, true
	// Valid, non-duplicate packet: only now does it count as liveness, so
	// junk on the channel can't hold the SEP against stale-stream eviction.
	s.mu.Lock()
	if d.st == stStreaming && s.owner == d && d.media == c {
		d.lastRTP = time.Now()
	}
	s.mu.Unlock()
	hdr := payload[0]
	frames := int(hdr & 0x0F)
	frameSamples := cfg.blocks * cfg.subbands
	// Timeline gap (lost packet, or frames dropped from the previous one).
	// Timestamp jitter under one SBC frame with contiguous sequence numbers
	// is not loss; a timestamp-only gap never resets the decoder.
	tsGap := false
	filled := false
	if cc.haveTS {
		gap := int32(ts - cc.nextTS)
		switch {
		case gap <= 0 || !(lost || int(gap) >= frameSamples):
			if !lost {
				st.tsClean++
			}
			if st.tsOnly < maxTSOnlyGaps {
				st.tsOnly = 0
			}
		case lost:
			if st.tsOnly < maxTSOnlyGaps {
				st.tsOnly = 0
			}
			st.tsClean = 0
			tsGap = true
		case st.tsOnly < maxTSOnlyGaps:
			// Conceal only if the timeline was consistent just before:
			// otherwise this is likely a non-sample RTP clock. Only
			// concealed gaps count towards the cap.
			tsGap = st.tsClean >= minTSCleanPackets
			if tsGap {
				st.tsOnly++
			}
			st.tsClean = 0
		}
		// st.tsOnly stays at the cap (disabled) until the stream is reset.
		if tsGap {
			if gap <= int32(cfg.rate/10) { // beyond ~100 ms: restart, don't fill
				// Fill the whole gap, within a wall-clock budget so a peer
				// can't push silence faster than realtime.
				fill := budget.take(int(gap), time.Now())
				out = s.conceal(cc, out, fill, cfg.rate)
				filled = fill > 0
			} else {
				tsPause = true
				cc.fadeIn = fadeFrames
			}
		}
	}
	if lost {
		dec.Reset() // synthesis history is stale
	}
	if lost || (tsGap && !tsPause) { // a pause without SUSPEND loses no packet
		s.lostPkts.Add(missing)
	}
	if lost || tsGap {
		if !filled && cc.last != [2]int16{} {
			// No fill (gap too short, negative or over ~100 ms): still fade
			// the previous audio out so the output doesn't step to zero.
			out = s.conceal(cc, out, budget.take(fadeFrames, time.Now()), cfg.rate)
		}
		cc.fadeIn = fadeFrames
	}
	payload = payload[1:]
	start := len(out)
	for i := 0; i < frames && len(payload) > 0; i++ {
		consumed, k, fi, err := dec.Decode(payload, pcm[:])
		bad := err != nil || consumed <= 0
		if !bad && (fi.SampleRate != cfg.rate || fi.Channels != ch ||
			fi.Blocks != cfg.blocks || fi.Subbands != cfg.subbands) {
			if !st.warned { // once per stream: don't flood the log
				st.warned = true
				log.Printf("avdtp: %v: dropping frames not matching configured %d Hz/%d ch/%d blocks/%d subbands (got %d Hz/%d ch/%d blocks/%d subbands)",
					d.addr, cfg.rate, cfg.channels(), cfg.blocks, cfg.subbands,
					fi.SampleRate, fi.Channels, fi.Blocks, fi.Subbands)
			}
			bad = true
		}
		if bad {
			s.badFrames.Add(1)
			if err == nil || !errors.Is(err, sbc.ErrCRC) {
				dec.Reset() // a CRC failure leaves the decoder state untouched
			}
			if (err == nil || errors.Is(err, sbc.ErrCRC)) && consumed > 0 && consumed <= len(payload) {
				// Frame boundary is known (CRC error or parameter mismatch):
				// conceal just this frame, keep going.
				out = s.conceal(cc, out, budget.take(frameSamples, time.Now()), cfg.rate)
				payload = payload[consumed:]
				continue
			}
			// Can't resync inside this packet: replace the rest with silence,
			// within the realtime budget.
			out = s.conceal(cc, out, budget.take((frames-i)*frameSamples, time.Now()), cfg.rate)
			break
		}
		cc.fade(pcm[:k])
		out = append(out, pcm[:k]...)
		payload = payload[consumed:]
	}
	cc.nextTS = ts + uint32((len(out)-start)/ch)
	cc.haveTS = true
	if len(out) > 0 {
		// Re-check under sinkMu, which stopSink holds while dropping: a
		// late chunk must not reopen the device after Drop/Stop.
		s.sinkMu.Lock()
		s.mu.Lock()
		ok := d.sinkOn && d.gen == g && d.st == stStreaming && d.media == c
		s.mu.Unlock()
		if ok {
			s.sink.Write(out)
		}
		s.sinkMu.Unlock()
	}
	st.out = out
}

// concealBudget is a leaky bucket bounding gap concealment to realtime plus a
// burst allowance: credit accrues at rate frames per second, capped at concealBurst.
type concealBudget struct {
	last   time.Time
	rate   int
	credit int64 // frames
}

const concealBurst = 200 * time.Millisecond

func (b *concealBudget) burst() int64 {
	return int64(concealBurst) * int64(b.rate) / int64(time.Second)
}

// reset starts with a full burst.
func (b *concealBudget) reset(rate int, now time.Time) {
	*b = concealBudget{last: now, rate: rate}
	b.credit = b.burst()
}

// take returns how many of want frames may be concealed now, and charges them.
func (b *concealBudget) take(want int, now time.Time) int {
	d := min(max(now.Sub(b.last), 0), concealBurst) // small: no overflow
	b.last = now
	b.credit = min(b.credit+int64(d)*int64(b.rate)/int64(time.Second), b.burst())
	n := min(int64(max(want, 0)), b.credit)
	b.credit -= n
	return int(n)
}

// isRTP reports whether b starts with a plausible RTP version 2 header.
// AVDTP signalling with transaction label 8–11 also has 2 in the top bits, so
// a single-packet header (packet type 0) whose second byte is a signal id
// (RFA zero, id ≤ DELAYREPORT) is taken for signalling. A2DP RTP uses a
// dynamic payload type (second byte ≥ 0x60), so it is not affected.
func isRTP(b []byte) bool {
	if len(b) < 12 || b[0]>>6 != 2 {
		return false
	}
	return b[0]&0x0C != 0 || b[1] > 0x0D
}

// rtpPayload strips the RTP header (CSRCs, extension, padding); nil if malformed.
func rtpPayload(b []byte) []byte {
	if len(b) < 12 || b[0]>>6 != 2 {
		return nil
	}
	pos := 12 + 4*int(b[0]&0x0F)
	if b[0]&0x10 != 0 {
		if len(b) < pos+4 {
			return nil
		}
		pos += 4 + 4*(int(b[pos+2])<<8|int(b[pos+3]))
	}
	end := len(b)
	if b[0]&0x20 != 0 {
		end -= int(b[len(b)-1])
	}
	if pos > end {
		return nil
	}
	return b[pos:end]
}
