package avdtp

import (
	"bytes"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"btspeaker/internal/sbc"
)

// deadlineConn is a fakeConn whose Read honours SetDeadline.
type deadlineConn struct {
	*fakeConn
	mu sync.Mutex
	dl time.Time
}

func newDeadlineConn(a byte) *deadlineConn { return &deadlineConn{fakeConn: newConn(a)} }

func (c *deadlineConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	c.dl = t
	c.mu.Unlock()
	return nil
}

func (c *deadlineConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	dl := c.dl
	c.mu.Unlock()
	if dl.IsZero() {
		return c.fakeConn.Read(p)
	}
	select {
	case b, ok := <-c.in:
		if !ok {
			return 0, errors.New("closed")
		}
		return copy(p, b), nil
	case <-time.After(time.Until(dl)):
		return 0, os.ErrDeadlineExceeded
	}
}

func openStream(t *testing.T, s *Server, sig *fakeConn) *fakeConn {
	t.Helper()
	go s.HandleConn(sig)
	expect(t, sig, "setconf", append([]byte{0x30, 0x03}, setConf...), []byte{0x32, 0x03})
	expect(t, sig, "open", []byte{0x60, 0x06, 0x04}, []byte{0x62, 0x06})
	media := newConn(sig.addr[0])
	go s.HandleConn(media)
	waitFor(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.devs[sig.addr].media != nil })
	expect(t, sig, "start", []byte{0x70, 0x07, 0x04}, []byte{0x72, 0x07})
	return media
}

func TestSuspendedStreamSurvivesIdleTimeout(t *testing.T) {
	old := idleTimeout
	idleTimeout = 40 * time.Millisecond
	defer func() { idleTimeout = old }()
	s := newTestServer(&fakeSink{})
	dc := newDeadlineConn(1)
	go s.HandleConn(dc)
	expect(t, dc.fakeConn, "setconf", append([]byte{0x30, 0x03}, setConf...), []byte{0x32, 0x03})
	expect(t, dc.fakeConn, "open", []byte{0x60, 0x06, 0x04}, []byte{0x62, 0x06})
	media := newConn(1)
	go s.HandleConn(media)
	waitFor(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.devs[dc.addr].media != nil })
	expect(t, dc.fakeConn, "start", []byte{0x70, 0x07, 0x04}, []byte{0x72, 0x07})
	expect(t, dc.fakeConn, "suspend", []byte{0x80, 0x09, 0x04}, []byte{0x82, 0x09})
	time.Sleep(5 * idleTimeout)
	expect(t, dc.fakeConn, "start2", []byte{0x90, 0x07, 0x04}, []byte{0x92, 0x07})
	dc.Close()
}

func TestIdleConfiguredTimesOut(t *testing.T) {
	old := idleTimeout
	idleTimeout = 30 * time.Millisecond
	defer func() { idleTimeout = old }()
	s := newTestServer(&fakeSink{})
	dc := newDeadlineConn(1)
	go s.HandleConn(dc)
	expect(t, dc.fakeConn, "setconf", append([]byte{0x30, 0x03}, setConf...), []byte{0x32, 0x03})
	waitFor(t, func() bool { return !s.HasSession(dc.addr) })
}

func TestTimestampJitterNotLoss(t *testing.T) {
	sink := &fakeSink{}
	s := newTestServer(sink)
	s.newDecoder = func() decoder { return &stepDec{} }
	media := openStream(t, s, newConn(1))
	good := []byte{1, 1, 1, 1, 1, 1, 1, 1}
	// Each packet carries 256 sample frames; timestamps run 10 ticks fast (< one
	// 128-sample SBC frame) every packet.
	for i := 0; i < 4; i++ {
		media.in <- rtp(uint16(i+1), uint32(1000+i*266), good...)
	}
	// Sentinel: once it is out, everything before it has been processed.
	media.in <- rtp(5, uint32(1000+4*266), good...)
	waitFor(t, func() bool { _, n := sink.get(); return n >= 5*512 })
	if _, n := sink.get(); n != 5*512 {
		t.Fatalf("got %d samples, silence inserted", n)
	}
	if st := s.Stats(); st.LostPackets != 0 || st.ConcealedMs != 0 {
		t.Fatalf("stats %+v", st)
	}
}

func TestSequenceJumpResyncs(t *testing.T) {
	sink := &fakeSink{}
	s := newTestServer(sink)
	s.newDecoder = func() decoder { return &stepDec{} }
	media := openStream(t, s, newConn(1))
	good := []byte{1, 1, 1, 1, 1, 1, 1, 1}
	media.in <- rtp(1, 0, good...)
	media.in <- rtp(2, 256, good...)
	media.in <- rtp(1, 0, good...) // duplicate/old: dropped and counted
	media.in <- rtp(2+40000, 512, good...)
	media.in <- rtp(3+40000, 768, good...)
	want := 4*512 + 2*fadeFrames            // plus the old tail faded out at the resync
	media.in <- rtp(4+40000, 1024, good...) // sentinel
	want += 512
	waitFor(t, func() bool { _, n := sink.get(); return n >= want })
	if _, n := sink.get(); n != want {
		t.Fatalf("got %d samples want %d", n, want)
	}
	if st := s.Stats(); st.DroppedDup != 1 || st.ConcealedMs != 0 {
		t.Fatalf("stats %+v", st)
	}
}

func TestDuplicateStartKeepsDecoder(t *testing.T) {
	sink := &fakeSink{}
	s := newTestServer(sink)
	dec := &fakeDec{}
	s.newDecoder = func() decoder { return dec }
	sig := newConn(1)
	media := openStream(t, s, sig)
	media.in <- rtp(1, 0, 1, 2, 3, 4)
	waitFor(t, func() bool { _, n := sink.get(); return n >= 4 })
	before := dec.resets
	expect(t, sig, "start-dup", []byte{0x10, 0x07, 0x04}, []byte{0x12, 0x07})
	media.in <- rtp(2, 2, 1, 2, 3, 4)
	waitFor(t, func() bool { _, n := sink.get(); return n >= 8 })
	if dec.resets != before {
		t.Fatalf("decoder reset by duplicate START (%d -> %d)", before, dec.resets)
	}
}

// crcDec: frames starting with 0xFF fail with ErrCRC but report their length.
type crcDec struct{ stepDec }

func (d crcDec) Decode(f []byte, pcm []int16) (int, int, sbc.FrameInfo, error) {
	if len(f) >= 4 && f[0] == 0xFF {
		return 4, 0, sbc.FrameInfo{}, sbc.ErrCRC
	}
	return d.stepDec.Decode(f, pcm)
}

func TestCRCErrorSkipsOneFrame(t *testing.T) {
	sink := &fakeSink{}
	s := newTestServer(sink)
	s.newDecoder = func() decoder { return crcDec{} }
	media := openStream(t, s, newConn(1))
	media.in <- rtp(1, 0, 1, 1, 1, 1, 0xFF, 0xFF, 0xFF, 0xFF, 1, 1, 1, 1)
	media.in <- rtp(2, 384, 1, 1, 1, 1) // sentinel
	waitFor(t, func() bool { _, n := sink.get(); return n >= 1024 })
	sink.mu.Lock()
	pcm := append([]int16(nil), sink.pcm...)
	sink.mu.Unlock()
	if len(pcm) != 1024 {
		t.Fatalf("got %d samples", len(pcm))
	}
	if pcm[767] != 10000 {
		t.Fatal("frame after the corrupt one was dropped")
	}
	if st := s.Stats(); st.BadFrames != 1 {
		t.Fatalf("stats %+v", st)
	}
}

func TestConcealmentFillsBurstLoss(t *testing.T) {
	sink := &fakeSink{}
	s := newTestServer(sink)
	s.newDecoder = func() decoder { return &stepDec{} }
	media := openStream(t, s, newConn(1))
	good := []byte{1, 1, 1, 1} // 128 sample frames
	media.in <- rtp(1, 0, good...)
	media.in <- rtp(5, 512, good...) // seq 2..4 lost: 384 frames
	waitFor(t, func() bool { _, n := sink.get(); return n >= 2*640 })
	time.Sleep(20 * time.Millisecond)
	if _, n := sink.get(); n != 2*640 {
		t.Fatalf("got %d samples, want %d", n, 2*640)
	}
}

func TestConcealmentBudget(t *testing.T) {
	sink := &fakeSink{}
	s := newTestServer(sink)
	s.newDecoder = func() decoder { return &stepDec{} }
	media := openStream(t, s, newConn(1))
	good := []byte{1, 1, 1, 1}
	// Each packet follows a lost one and claims a ~90 ms gap: far faster than realtime.
	for i := range 40 {
		media.in <- rtp(uint16(2*i+1), uint32(i*4128), good...)
	}
	waitFor(t, func() bool { _, n := sink.get(); return n >= 2*40*128 })
	if ms := s.Stats().ConcealedMs; ms < 150 || ms > 400 {
		t.Fatalf("concealed %d ms, want about the 200 ms burst allowance", ms)
	}
}

// Drop must not be followed by a late Write that reopens the device.
type strictSink struct {
	fakeSink
	dropped atomic.Bool
	late    atomic.Int32
}

func (f *strictSink) Start(r, c int) { f.dropped.Store(false); f.fakeSink.Start(r, c) }
func (f *strictSink) Drop()          { f.dropped.Store(true); f.fakeSink.Drop() }
func (f *strictSink) Write(p []int16) {
	if f.dropped.Load() {
		f.late.Add(1)
	}
	f.fakeSink.Write(p)
}

func TestNoWriteAfterDrop(t *testing.T) {
	for i := 0; i < 50; i++ {
		sink := &strictSink{}
		s := newTestServer(sink)
		s.newDecoder = func() decoder { return &stepDec{} }
		sig := newConn(1)
		media := openStream(t, s, sig)
		stop := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			good := []byte{1, 1, 1, 1}
			for q := uint16(1); ; q++ {
				select {
				case <-stop:
					return
				case media.in <- rtp(q, uint32(q)*256, good...):
				default:
					time.Sleep(50 * time.Microsecond)
				}
			}
		}()
		time.Sleep(time.Millisecond)
		s.mu.Lock()
		d := s.devs[sig.addr]
		s.mu.Unlock()
		s.stopSink(d, false)
		time.Sleep(2 * time.Millisecond)
		close(stop)
		wg.Wait()
		if sink.late.Load() != 0 {
			t.Fatalf("iter %d: %d writes after Drop", i, sink.late.Load())
		}
		sig.Close()
	}
}

func TestReconnectEvictsSessionWithMedia(t *testing.T) {
	sink := &fakeSink{}
	s := newTestServer(sink)
	a1 := newConn(1)
	media := openStream(t, s, a1)
	a2 := newConn(1) // phone reconnected; old channels still linger
	go s.HandleConn(a2)
	expect(t, a2, "setconf", append([]byte{0x00, 0x03}, setConf...), []byte{0x02, 0x03})
	if _, ok := <-a1.in; ok {
		t.Fatal("old signalling not closed")
	}
	if _, ok := <-media.in; ok {
		t.Fatal("old media not closed")
	}
	a2.Close()
}

func TestResyncCounted(t *testing.T) {
	sink := &fakeSink{}
	s := newTestServer(sink)
	s.newDecoder = func() decoder { return &stepDec{} }
	media := openStream(t, s, newConn(1))
	good := []byte{1, 1, 1, 1}
	media.in <- rtp(1, 0, good...)
	media.in <- rtp(30000, 128, good...)
	waitFor(t, func() bool { _, n := sink.get(); return n >= 2*256 })
	if st := s.Stats(); st.Resyncs != 1 {
		t.Fatalf("stats %+v", st)
	}
}

func TestConcealBudgetLeakyBucket(t *testing.T) {
	const rate = 48000
	t0 := time.Now()
	var b concealBudget
	b.reset(rate, t0)
	burst := rate / 5
	// 100 h of clean streaming must not overflow nor bank credit.
	now := t0.Add(100 * time.Hour)
	if got := b.take(rate, now); got != burst {
		t.Fatalf("after 100 h: took %d, want burst %d", got, burst)
	}
	// Back-to-back gaps: only realtime refill on top of the (spent) burst.
	total := 0
	for i := range 100 {
		total += b.take(rate/10, now.Add(time.Duration(i)*10*time.Millisecond))
	}
	// 99 steps of 10 ms = 990 ms realtime.
	if want := rate * 990 / 1000; total < want-rate/100 || total > want+rate/100 {
		t.Fatalf("flood conceded %d frames, want about %d", total, want)
	}
}

// writeDeadlineConn records the write deadline in force at each Write.
type writeDeadlineConn struct {
	*fakeConn
	mu      sync.Mutex
	wd      time.Time
	atWrite []time.Time
}

func (c *writeDeadlineConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.wd = t
	c.mu.Unlock()
	return nil
}

func (c *writeDeadlineConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.atWrite = append(c.atWrite, c.wd)
	c.mu.Unlock()
	return c.fakeConn.Write(p)
}

func TestSignalReplyHasWriteDeadline(t *testing.T) {
	s := newTestServer(&fakeSink{})
	wc := &writeDeadlineConn{fakeConn: newConn(1)}
	go s.HandleConn(wc)
	expect(t, wc.fakeConn, "setconf", append([]byte{0x30, 0x03}, setConf...), []byte{0x32, 0x03})
	wc.Close()
	wc.mu.Lock()
	defer wc.mu.Unlock()
	if len(wc.atWrite) != 1 || wc.atWrite[0].IsZero() || time.Until(wc.atWrite[0]) > writeTimeout {
		t.Fatalf("write deadlines %v", wc.atWrite)
	}
}

func TestTimestampOnlyGapsDisabledAfterCap(t *testing.T) {
	sink := &fakeSink{}
	s := newTestServer(sink)
	s.newDecoder = func() decoder { return &stepDec{} }
	media := openStream(t, s, newConn(1))
	good := []byte{1, 1, 1, 1, 1, 1, 1, 1}
	// Contiguous sequence numbers, but timestamps in a different unit: every
	// packet looks like a ~1800-tick gap.
	const n = 20
	for i := 0; i < n; i++ {
		media.in <- rtp(uint16(i+1), uint32(i*2000), good...)
	}
	waitFor(t, func() bool { _, k := sink.get(); return k >= n*256 })
	time.Sleep(20 * time.Millisecond)
	if st := s.Stats(); st.LostPackets != 0 || st.ConcealedMs != 0 {
		t.Fatalf("lost %d concealed %d ms, want none for a non-sample RTP clock", st.LostPackets, st.ConcealedMs)
	}
}

func TestTimestampOnlyGapConcealedAfterCleanRun(t *testing.T) {
	sink := &fakeSink{}
	s := newTestServer(sink)
	s.newDecoder = func() decoder { return &stepDec{} }
	media := openStream(t, s, newConn(1))
	good := []byte{1, 1, 1, 1, 1, 1, 1, 1}
	const n = 30
	var ts uint32
	for i := 0; i < n; i++ {
		if i == 25 {
			ts += 1000 // contiguous seq, timeline skips ahead
		}
		media.in <- rtp(uint16(i+1), ts, good...)
		ts += 256
	}
	waitFor(t, func() bool { _, k := sink.get(); return k >= n*256 })
	time.Sleep(20 * time.Millisecond)
	if st := s.Stats(); st.LostPackets != 1 {
		t.Fatalf("lost %d, want 1", st.LostPackets)
	}
}

func TestTimestampOnlyPauseNotLoss(t *testing.T) {
	sink := &fakeSink{}
	s := newTestServer(sink)
	s.newDecoder = func() decoder { return &stepDec{} }
	media := openStream(t, s, newConn(1))
	good := []byte{1, 1, 1, 1, 1, 1, 1, 1}
	const n = 30
	var ts uint32
	for i := 0; i < n; i++ {
		if i == 25 {
			ts += 44100 // contiguous seq, 1 s pause without SUSPEND
		}
		media.in <- rtp(uint16(i+1), ts, good...)
		ts += 256
	}
	waitFor(t, func() bool { _, k := sink.get(); return k >= n*256 })
	if st := s.Stats(); st.LostPackets != 0 {
		t.Fatalf("lost %d, want 0 for a pause", st.LostPackets)
	}
}

func TestBackwardRestartRecovers(t *testing.T) {
	sink := &fakeSink{}
	s := newTestServer(sink)
	s.newDecoder = func() decoder { return &stepDec{} }
	media := openStream(t, s, newConn(1))
	good := []byte{1, 1, 1, 1}
	media.in <- rtp(5000, 0, good...)
	// Source restarts at a sequence inside the backward window.
	for i := 0; i < maxDupRun+3; i++ {
		media.in <- rtp(uint16(4500+i), uint32(i*128), good...)
	}
	waitFor(t, func() bool { return s.Stats().Resyncs == 1 })
	waitFor(t, func() bool { _, n := sink.get(); return n >= 4*256 })
	if st := s.Stats(); st.DroppedDup != maxDupRun {
		t.Fatalf("stats %+v", st)
	}
}

func TestSSRCChangeResyncsImmediately(t *testing.T) {
	sink := &fakeSink{}
	s := newTestServer(sink)
	s.newDecoder = func() decoder { return &stepDec{} }
	media := openStream(t, s, newConn(1))
	good := []byte{1, 1, 1, 1}
	media.in <- rtp(100, 0, good...)
	media.in <- rtp(101, 128, good...)
	// New source, nearby (backward) sequence number: would otherwise be dropped.
	p := rtp(99, 5000, good...)
	p[11] = 2
	media.in <- p
	waitFor(t, func() bool { return s.Stats().Resyncs == 1 })
	waitFor(t, func() bool { _, n := sink.get(); return n >= 3*256 })
	if st := s.Stats(); st.DroppedDup != 0 {
		t.Fatalf("stats %+v", st)
	}
}

func TestSSRCChangeFadesOldTail(t *testing.T) {
	sink := &fakeSink{}
	s := newTestServer(sink)
	s.newDecoder = func() decoder { return &stepDec{} }
	media := openStream(t, s, newConn(1))
	good := []byte{1, 1, 1, 1}
	media.in <- rtp(100, 0, good...)
	p := rtp(100, 128, good...)
	p[11] = 2
	media.in <- p
	want := 2*128 + 2*fadeFrames + 2*128
	waitFor(t, func() bool { _, n := sink.get(); return n >= want })
	time.Sleep(20 * time.Millisecond)
	sink.mu.Lock()
	pcm := append([]int16(nil), sink.pcm...)
	sink.mu.Unlock()
	if len(pcm) != want {
		t.Fatalf("got %d samples, want %d", len(pcm), want)
	}
	if a, z := pcm[256], pcm[256+2*(fadeFrames-1)]; a < 9000 || z != 0 {
		t.Fatalf("no fade-out tail: first %d last %d", a, z)
	}
	if f := pcm[256+2*fadeFrames]; f >= 10000 {
		t.Fatalf("new source not faded in: %d", f)
	}
}

func TestLostPacketsCountsBurst(t *testing.T) {
	sink := &fakeSink{}
	s := newTestServer(sink)
	s.newDecoder = func() decoder { return &stepDec{} }
	media := openStream(t, s, newConn(1))
	good := []byte{1, 1, 1, 1}
	media.in <- rtp(1, 0, good...)
	media.in <- rtp(5, 4*128, good...) // 3 lost
	waitFor(t, func() bool { return s.Stats().LostPackets == 3 })
}

func TestLongGapFadesOut(t *testing.T) {
	sink := &fakeSink{}
	s := newTestServer(sink)
	s.newDecoder = func() decoder { return &stepDec{} }
	media := openStream(t, s, newConn(1))
	good := []byte{1, 1, 1, 1} // 128 sample frames
	media.in <- rtp(1, 0, good...)
	media.in <- rtp(3, 20000, good...) // lost, gap > 100 ms: no fill
	want := 2 * (128 + fadeFrames + 128)
	waitFor(t, func() bool { _, n := sink.get(); return n >= want })
	time.Sleep(20 * time.Millisecond)
	sink.mu.Lock()
	pcm := append([]int16(nil), sink.pcm...)
	sink.mu.Unlock()
	if len(pcm) != want {
		t.Fatalf("got %d samples, want %d", len(pcm), want)
	}
	if a, z := pcm[256], pcm[256+2*(fadeFrames-1)]; a < 9000 || z != 0 {
		t.Fatalf("no fade-out tail: first %d last %d", a, z)
	}
}

func TestFragmentedPacketCountsAsLoss(t *testing.T) {
	sink := &fakeSink{}
	s := newTestServer(sink)
	s.newDecoder = func() decoder { return &stepDec{} }
	media := openStream(t, s, newConn(1))
	good := []byte{1, 1, 1, 1}
	media.in <- rtp(1, 0, good...)
	frag := rtp(2, 128, good...)
	frag[12] |= 0x80
	media.in <- frag
	media.in <- rtp(3, 256, good...)
	waitFor(t, func() bool { _, n := sink.get(); return n >= 3*256 })
	time.Sleep(20 * time.Millisecond)
	if _, n := sink.get(); n != 3*256 {
		t.Fatalf("got %d samples, want %d", n, 3*256)
	}
	if st := s.Stats(); st.LostPackets != 1 {
		t.Fatalf("stats %+v", st)
	}
}

func lastRTP(s *Server, a [6]byte) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.devs {
		return d.lastRTP
	}
	return time.Time{}
}

func TestJunkAndDuplicatesDontRefreshLastRTP(t *testing.T) {
	sink := &fakeSink{}
	s := newTestServer(sink)
	s.newDecoder = func() decoder { return &stepDec{} }
	media := openStream(t, s, newConn(1))
	media.in <- rtp(1, 0, 1, 1, 1, 1)
	waitFor(t, func() bool { _, n := sink.get(); return n >= 256 })
	old := time.Now().Add(-time.Hour)
	set := func() {
		s.mu.Lock()
		for _, d := range s.devs {
			d.lastRTP = old
		}
		s.mu.Unlock()
	}
	set()
	media.in <- []byte{1, 2, 3}                           // too short for RTP
	media.in <- append([]byte{0x40}, make([]byte, 20)...) // wrong version
	media.in <- rtp(1, 0, 1, 1, 1, 1)                     // duplicate
	waitFor(t, func() bool { return s.Stats().DroppedDup == 1 })
	if got := lastRTP(s, [6]byte{}); !got.Equal(old) {
		t.Fatal("junk or duplicate refreshed lastRTP")
	}
	media.in <- rtp(2, 128) // valid header, no frames
	media.in <- rtp(2, 128) // duplicate: a sync point for the empty one
	waitFor(t, func() bool { return s.Stats().DroppedDup == 2 })
	if got := lastRTP(s, [6]byte{}); !got.Equal(old) {
		t.Fatal("empty packet refreshed lastRTP")
	}
	if st := s.Streams(); len(st) != 1 || st[0].State != "paused" {
		t.Fatalf("no audio for an hour: streams %+v, want paused", st)
	}
	media.in <- rtp(3, 128, 1, 1, 1, 1)
	waitFor(t, func() bool { return !lastRTP(s, [6]byte{}).Equal(old) })
	if st := s.Streams(); len(st) != 1 || st[0].State != "streaming" {
		t.Fatalf("audio again: streams %+v, want streaming", st)
	}
}

func TestBadFrameSilenceIsBudgeted(t *testing.T) {
	sink := &fakeSink{}
	s := newTestServer(sink)
	s.newDecoder = func() decoder { return &stepDec{} }
	media := openStream(t, s, newConn(1))
	// Each tiny packet claims 15 frames but holds one undecodable one.
	for i := range 40 {
		p := rtp(uint16(i+1), uint32(i*1920), 0, 0, 0, 0)
		p[12] = 15
		media.in <- p
	}
	waitFor(t, func() bool { return s.Stats().BadFrames == 40 })
	if ms := s.Stats().ConcealedMs; ms > 400 {
		t.Fatalf("concealed %d ms, want about the 200 ms burst allowance", ms)
	}
}

// mismatchDec: frames starting with 2 decode but report other parameters.
type mismatchDec struct{ stepDec }

func (d mismatchDec) Decode(f []byte, pcm []int16) (int, int, sbc.FrameInfo, error) {
	if len(f) >= 4 && f[0] == 2 {
		_, k, fi, err := d.stepDec.Decode([]byte{1, 1, 1, 1}, pcm)
		fi.Subbands = 4
		return 4, k, fi, err
	}
	return d.stepDec.Decode(f, pcm)
}

func TestParameterMismatchSkipsOneFrame(t *testing.T) {
	sink := &fakeSink{}
	s := newTestServer(sink)
	s.newDecoder = func() decoder { return mismatchDec{} }
	media := openStream(t, s, newConn(1))
	media.in <- rtp(1, 0, 1, 1, 1, 1, 2, 2, 2, 2, 1, 1, 1, 1)
	media.in <- rtp(2, 384, 1, 1, 1, 1) // sentinel
	waitFor(t, func() bool { _, n := sink.get(); return n >= 1024 })
	sink.mu.Lock()
	pcm := append([]int16(nil), sink.pcm...)
	sink.mu.Unlock()
	if len(pcm) != 1024 {
		t.Fatalf("got %d samples", len(pcm))
	}
	if pcm[767] != 10000 {
		t.Fatal("frame after the mismatched one was dropped")
	}
}

// crcFakeDec fails frames starting with 0xFF with ErrCRC (length known).
type crcFakeDec struct{ fakeDec }

func (d *crcFakeDec) Decode(f []byte, pcm []int16) (int, int, sbc.FrameInfo, error) {
	if len(f) >= 4 && f[0] == 0xFF {
		return 4, 0, sbc.FrameInfo{}, sbc.ErrCRC
	}
	return d.fakeDec.Decode(f, pcm)
}

func TestCRCErrorKeepsDecoderState(t *testing.T) {
	sink := &fakeSink{}
	s := newTestServer(sink)
	dec := &crcFakeDec{}
	s.newDecoder = func() decoder { return dec }
	sig := newConn(1)
	media := openStream(t, s, sig)
	media.in <- rtp(1, 0, 1, 2, 3, 4)
	waitFor(t, func() bool { _, n := sink.get(); return n >= 4 })
	before := dec.resets
	media.in <- rtp(2, 2, 0xFF, 2, 3, 4)
	media.in <- rtp(3, 4, 1, 2, 3, 4)
	waitFor(t, func() bool { return s.Stats().BadFrames >= 1 })
	waitFor(t, func() bool { _, n := sink.get(); return n >= 8 })
	if dec.resets != before {
		t.Fatalf("decoder reset on CRC error: %d -> %d", before, dec.resets)
	}
}

func TestMediaChannelCarryingSignallingIsHandedOver(t *testing.T) {
	s := newTestServer(&fakeSink{})
	sig := newConn(1)
	go s.HandleConn(sig)
	expect(t, sig, "setconf", append([]byte{0x30, 0x03}, setConf...), []byte{0x32, 0x03})
	expect(t, sig, "open", []byte{0x60, 0x06, 0x04}, []byte{0x62, 0x06})
	// The phone reconnected: its new signalling channel arrives while the
	// old session is OPEN, so it is first taken for media.
	nsig := newConn(1)
	go s.HandleConn(nsig)
	waitFor(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.devs[sig.addr].media == nsig })
	// DISCOVER (label 5): the first packet must be answered, not dropped.
	r := rpc(t, nsig, []byte{0x50, 0x01})
	if len(r) < 2 || r[0] != 0x52 || r[1] != 0x01 {
		t.Fatalf("discover reply %x", r)
	}
	s.mu.Lock()
	d := s.devs[sig.addr]
	ok := d.sig == any(nsig) && d.media == nil
	s.mu.Unlock()
	if !ok {
		t.Fatal("session not replaced")
	}
	// The old signalling channel was evicted.
	if _, open := <-sig.in; open {
		t.Fatal("old signalling channel still open")
	}
	// The new channel keeps working.
	expect(t, nsig, "setconf", append([]byte{0x30, 0x03}, setConf...), []byte{0x32, 0x03})
}

// A reconnect whose first command is SET_CONFIGURATION with label 8–11 has an
// RTP-like first byte (0x9x): it must still be taken for signalling.
func TestMediaChannelSignallingHighLabel(t *testing.T) {
	s := newTestServer(&fakeSink{})
	sig := newConn(1)
	go s.HandleConn(sig)
	expect(t, sig, "setconf", append([]byte{0x30, 0x03}, setConf...), []byte{0x32, 0x03})
	expect(t, sig, "open", []byte{0x60, 0x06, 0x04}, []byte{0x62, 0x06})
	nsig := newConn(1)
	go s.HandleConn(nsig)
	waitFor(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.devs[sig.addr].media == nsig })
	cmd := append([]byte{0x90, 0x03}, setConf...)
	if !(len(cmd) >= 12 && cmd[0]>>6 == 2) {
		t.Fatal("test packet does not look like RTP")
	}
	r := rpc(t, nsig, cmd)
	if len(r) < 2 || r[0]&0xF0 != 0x90 || r[1] != 0x03 {
		t.Fatalf("setconf reply %x", r)
	}
}

func TestMediaOnlyAcceptedWhenOpen(t *testing.T) {
	s := newTestServer(&fakeSink{})
	sig := newConn(1)
	go s.HandleConn(sig)
	expect(t, sig, "setconf", append([]byte{0x30, 0x03}, setConf...), []byte{0x32, 0x03})
	// Configured but not OPEN: a second channel is a new signalling channel.
	nsig := newConn(1)
	go s.HandleConn(nsig)
	r := rpc(t, nsig, []byte{0x50, 0x01})
	if len(r) < 2 || r[0] != 0x52 {
		t.Fatalf("discover reply %x", r)
	}
}

func TestParseCapsDuplicateCategory(t *testing.T) {
	_, _, bad, ok := parseCaps([]byte{catMediaTransport, 0, catMediaCodec, 1, 0, catMediaCodec, 0})
	if ok || bad != catMediaCodec {
		t.Fatalf("ok=%v bad=%#x", ok, bad)
	}
	s := newTestServer(&fakeSink{})
	d := &device{}
	p := append(append([]byte(nil), setConf...), catMediaTransport, 0)
	r := s.doSetConfig(d, 3, p)
	if !bytes.Equal(r, []byte{0x33, 0x03, catMediaTransport, errBadServCategory}) {
		t.Fatalf("%x", r)
	}
}

func TestReassemblyMessageTypeMustMatchStart(t *testing.T) {
	s := newTestServer(&fakeSink{})
	d := &device{}
	_, _, _, _, ok := s.reassemble(d, []byte{0x04, 0x02, 0x03, 4}) // START, command
	if ok {
		t.Fatal("start completed")
	}
	if _, _, _, _, ok := s.reassemble(d, []byte{0x0E, 0x08}); ok { // END, accept
		t.Fatal("END with different message type accepted")
	}
}
