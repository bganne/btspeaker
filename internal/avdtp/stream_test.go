package avdtp

import (
	"testing"
)

// streamHarness drives stream.onPacket directly, without a media Conn loop.
type streamHarness struct {
	t    *testing.T
	s    *Server
	sink *fakeSink
	st   *stream
}

func newStreamHarness(t *testing.T) *streamHarness {
	sink := &fakeSink{}
	s := newTestServer(sink)
	c := newConn(1)
	d := &device{st: stStreaming, sinkOn: true, media: c, gen: 1}
	s.owner = d
	st := newStream(s, d, c, &stepDec{})
	st.begin(sbcConfig{rate: 44100, mode: modeStereo, blocks: 16, subbands: 8}, 1)
	return &streamHarness{t: t, s: s, sink: sink, st: st}
}

// good is one SBC payload: header (1 frame) + one 4-byte frame -> 256 samples.
var goodPayload = []byte{1, 1, 1, 1, 1}

func (h *streamHarness) pkt(seq uint16, ts, ssrc uint32) int {
	_, before := h.sink.get()
	h.st.onPacket(seq, ts, ssrc, goodPayload)
	_, after := h.sink.get()
	return after - before
}

func TestStreamInOrder(t *testing.T) {
	h := newStreamHarness(t)
	for i := 0; i < 5; i++ {
		if n := h.pkt(uint16(i), uint32(i*128), 7); n != 256 {
			t.Fatalf("packet %d wrote %d samples", i, n)
		}
	}
	if st := h.s.Stats(); st.LostPackets != 0 || st.ConcealedMs != 0 || st.DroppedDup != 0 || st.Resyncs != 0 {
		t.Fatalf("stats %+v", st)
	}
	if h.st.d.lastRTP.IsZero() {
		t.Fatal("liveness not recorded")
	}
}

func TestStreamGapConcealed(t *testing.T) {
	h := newStreamHarness(t)
	h.pkt(1, 0, 7)
	n := h.pkt(4, 3*128, 7) // seq 2,3 lost: 256 frames timeline gap
	if want := 256 + 2*256; n != want {
		t.Fatalf("wrote %d samples, want %d", n, want)
	}
	if st := h.s.Stats(); st.LostPackets != 2 || st.ConcealedMs == 0 {
		t.Fatalf("stats %+v", st)
	}
}

func TestStreamSeqWrap(t *testing.T) {
	h := newStreamHarness(t)
	h.pkt(65534, 0, 7)
	h.pkt(65535, 128, 7)
	if n := h.pkt(0, 256, 7); n != 256 {
		t.Fatalf("wrap packet wrote %d", n)
	}
	if st := h.s.Stats(); st.LostPackets != 0 || st.DroppedDup != 0 || st.Resyncs != 0 {
		t.Fatalf("stats %+v", st)
	}
}

func TestStreamReorderAfterConcealDropped(t *testing.T) {
	h := newStreamHarness(t)
	h.pkt(1, 0, 7)
	h.pkt(3, 256, 7) // seq 2 concealed
	if n := h.pkt(2, 128, 7); n != 0 {
		t.Fatalf("late packet wrote %d samples", n)
	}
	if st := h.s.Stats(); st.DroppedDup != 1 {
		t.Fatalf("stats %+v", st)
	}
	if n := h.pkt(4, 384, 7); n != 256 {
		t.Fatalf("stream did not continue: %d", n)
	}
}

func TestStreamSSRCChangeResyncs(t *testing.T) {
	h := newStreamHarness(t)
	h.pkt(1, 0, 7)
	h.pkt(2, 128, 7)
	// New source with an unrelated, nearby sequence number: would be a duplicate otherwise.
	if n := h.pkt(1, 0, 9); n != 256+fadeFrames*2 {
		t.Fatalf("wrote %d samples", n)
	}
	if st := h.s.Stats(); st.Resyncs != 1 || st.DroppedDup != 0 {
		t.Fatalf("stats %+v", st)
	}
}

func TestStreamOnPacketAllocFree(t *testing.T) {
	h := newStreamHarness(t)
	h.sink.mu.Lock()
	h.sink.pcm = make([]int16, 0, 1<<20) // keep the fake sink's own growth out of the count
	h.sink.mu.Unlock()
	seq, ts := uint16(0), uint32(0)
	if a := testing.AllocsPerRun(1000, func() {
		h.st.onPacket(seq, ts, 7, goodPayload)
		seq++
		ts += 128
	}); a != 0 {
		t.Fatalf("onPacket allocates: %v per packet", a)
	}
}

// recSink records the size of the largest Write.
type recSink struct {
	fakeSink
	max int
}

func (r *recSink) Write(p []int16) {
	r.mu.Lock()
	r.max = max(r.max, len(p))
	r.mu.Unlock()
	r.fakeSink.Write(p)
}

func newRecStream(sink *recSink) *stream {
	s := newTestServer(sink)
	c := newConn(1)
	d := &device{st: stStreaming, sinkOn: true, media: c, gen: 1}
	s.owner = d
	st := newStream(s, d, c, &stepDec{})
	st.begin(sbcConfig{rate: 44100, mode: modeStereo, blocks: 16, subbands: 8}, 1)
	return st
}

func TestStreamWriteChunksBounded(t *testing.T) {
	sink := &recSink{}
	st := newRecStream(sink)
	st.onPacket(1, 0, 7, goodPayload)
	// 15 frames after a ~90 ms timeline gap: fill plus audio far exceeds one chunk.
	pl := []byte{15}
	for i := 0; i < 15; i++ {
		pl = append(pl, 1, 1, 1, 1)
	}
	st.onPacket(3, 4000, 7, pl)
	_, total := sink.get()
	if total <= 2*maxSinkFrames*2 {
		t.Fatalf("test did not exercise a large write: %d samples", total)
	}
	if sink.max > maxSinkFrames*2 {
		t.Fatalf("Write of %d samples exceeds %d", sink.max, maxSinkFrames*2)
	}
}

// FuzzStream drives the stream state machine with arbitrary packet tuples.
func FuzzStream(f *testing.F) {
	f.Add([]byte{0, 1, 0, 0, 0, 7, 5, 1, 1, 1, 1, 1})
	f.Add([]byte{0, 4, 0, 0, 128, 7, 5, 1, 1, 1, 1, 1})
	f.Fuzz(func(t *testing.T, b []byte) {
		sink := &recSink{}
		st := newRecStream(sink)
		// Records: seq(2) ts(3) ssrc(1) len(1) payload(len%24).
		for len(b) >= 7 {
			seq := uint16(b[0])<<8 | uint16(b[1])
			ts := uint32(b[2])<<16 | uint32(b[3])<<8 | uint32(b[4])
			if b[2]&0x80 != 0 {
				ts |= 0xFF000000
			}
			ssrc := uint32(b[5])
			n := min(int(b[6])%24, len(b)-7)
			p := b[7 : 7+n]
			b = b[7+n:]
			if len(p) < 1 || p[0]&0x80 != 0 {
				continue
			}
			st.onPacket(seq, ts, ssrc, p)
			if sink.max > maxSinkFrames*2 {
				t.Fatalf("Write of %d samples", sink.max)
			}
			if len(st.out) > outCap*4 {
				t.Fatalf("out grew to %d", len(st.out))
			}
		}
	})
}
