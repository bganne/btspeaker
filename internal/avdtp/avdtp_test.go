package avdtp

import (
	"bytes"
	"io"
	"sync"
	"testing"
	"time"

	"btspeaker/internal/bt"
	"btspeaker/internal/sbc"
)

type fakeConn struct {
	addr bt.Addr
	in   chan []byte
	out  chan []byte
	once sync.Once
}

func newConn(a byte) *fakeConn {
	return &fakeConn{addr: bt.Addr{a}, in: make(chan []byte, 16), out: make(chan []byte, 16)}
}
func (f *fakeConn) Read(p []byte) (int, error) {
	b, ok := <-f.in
	if !ok {
		return 0, io.EOF
	}
	return copy(p, b), nil
}
func (f *fakeConn) Write(p []byte) (int, error) {
	f.out <- append([]byte(nil), p...)
	return len(p), nil
}
func (f *fakeConn) Close() error        { f.once.Do(func() { close(f.in) }); return nil }
func (f *fakeConn) RemoteAddr() bt.Addr { return f.addr }
func (f *fakeConn) PSM() uint16         { return 25 }
func (f *fakeConn) InMTU() int          { return 672 }
func (f *fakeConn) OutMTU() int         { return 672 }

type fakeSink struct {
	mu     sync.Mutex
	events []string
	pcm    []int16
}

func (f *fakeSink) Start(r, c int) {
	f.mu.Lock()
	f.events = append(f.events, "start")
	f.mu.Unlock()
	_ = r
	_ = c
}
func (f *fakeSink) Write(p []int16) {
	f.mu.Lock()
	f.pcm = append(f.pcm, p...)
	f.mu.Unlock()
}
func (f *fakeSink) Stop() { f.mu.Lock(); f.events = append(f.events, "stop"); f.mu.Unlock() }
func (f *fakeSink) Drop() { f.mu.Lock(); f.events = append(f.events, "drop"); f.mu.Unlock() }
func (f *fakeSink) get() ([]string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...), len(f.pcm)
}

// fakeDec treats each 4-byte "frame" as 4 samples.
type fakeDec struct {
	resets   int
	rate     int // 0 = 44100
	subbands int // 0 = 8 (as in setConf)
}

func (d *fakeDec) Decode(f []byte, pcm []int16) (int, int, sbc.FrameInfo, error) {
	if len(f) < 4 {
		return 0, 0, sbc.FrameInfo{}, io.ErrUnexpectedEOF
	}
	for i := 0; i < 4; i++ {
		pcm[i] = int16(f[i])
	}
	rate := d.rate
	if rate == 0 {
		rate = 44100
	}
	sb := d.subbands
	if sb == 0 {
		sb = 8
	}
	return 4, 4, sbc.FrameInfo{SampleRate: rate, Channels: 2, Blocks: 16, Subbands: sb}, nil
}
func (d *fakeDec) Reset() { d.resets++ }

func newTestServer(sink AudioSink) *Server {
	s := NewServer(Config{SampleRates: []int{44100, 48000}}, sink)
	s.newDecoder = func() decoder { return &fakeDec{} }
	return s
}

func rpc(t *testing.T, c *fakeConn, req []byte) []byte {
	t.Helper()
	c.in <- req
	select {
	case r := <-c.out:
		return r
	case <-time.After(2 * time.Second):
		t.Fatal("timeout")
	}
	return nil
}

func expect(t *testing.T, c *fakeConn, name string, req, want []byte) {
	t.Helper()
	if got := rpc(t, c, req); !bytes.Equal(got, want) {
		t.Fatalf("%s: got %x want %x", name, got, want)
	}
}

func waitFor(t *testing.T, f func() bool) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if f() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met")
}

// 44.1k joint stereo, 16 blocks, 8 subbands, loudness, bitpool 2..53 (as a Pixel 5 sends).
var setConf = []byte{0x04, 0x08, 0x01, 0x00, 0x07, 0x06, 0x00, 0x00, 0x21, 0x15, 0x02, 0x35}

func TestFullFlow(t *testing.T) {
	sink := &fakeSink{}
	s := newTestServer(sink)
	sig := newConn(1)
	go s.HandleConn(sig)

	expect(t, sig, "discover", []byte{0x00, 0x01}, []byte{0x02, 0x01, 0x04, 0x08})
	expect(t, sig, "getallcaps", []byte{0x10, 0x0C, 0x04},
		[]byte{0x12, 0x0C, 0x01, 0x00, 0x07, 0x06, 0x00, 0x00, 0x30 | 0x0F, 0xFF, 0x02, 53})
	// wrong SEID
	expect(t, sig, "badseid", []byte{0x20, 0x02, 0x08}, []byte{0x23, 0x02, 0x12})
	expect(t, sig, "setconf", append([]byte{0x30, 0x03}, setConf...), []byte{0x32, 0x03})
	expect(t, sig, "getconf", []byte{0x40, 0x04, 0x04},
		[]byte{0x42, 0x04, 0x01, 0x00, 0x07, 0x06, 0x00, 0x00, 0x21, 0x15, 0x02, 0x35})
	expect(t, sig, "start-too-early", []byte{0x50, 0x07, 0x04}, []byte{0x53, 0x07, 0x04, 0x31})
	expect(t, sig, "open", []byte{0x60, 0x06, 0x04}, []byte{0x62, 0x06})
	if st := s.Streams(); len(st) != 1 || st[0].State != "open" || st[0].SampleRate != 44100 ||
		st[0].Mode != "joint stereo" || st[0].Blocks != 16 || st[0].Subbands != 8 || st[0].Bitpool != 53 {
		t.Fatalf("streams %+v", st)
	}

	media := newConn(1)
	go s.HandleConn(media)
	waitFor(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.devs[sig.addr].media != nil })

	expect(t, sig, "start", []byte{0x70, 0x07, 0x04}, []byte{0x72, 0x07})
	if ev, _ := sink.get(); len(ev) != 1 || ev[0] != "start" {
		t.Fatalf("events %v", ev)
	}
	// RTP (12 bytes) + header (2 frames) + 2x4-byte frames.
	pkt := append([]byte{0x80, 0x60, 0, 1, 0, 0, 0, 1, 0, 0, 0, 1, 0x02}, 1, 2, 3, 4, 5, 6, 7, 8)
	media.in <- pkt
	// Fragmented packet is dropped.
	media.in <- append([]byte{0x80, 0x60, 0, 2, 0, 0, 0, 1, 0, 0, 0, 1, 0x81}, 1, 2, 3, 4)
	media.in <- append([]byte{0x80, 0x60, 0, 3, 0, 0, 0, 1, 0, 0, 0, 1, 0x02}, 1, 2, 3, 4, 5, 6, 7, 8)
	waitFor(t, func() bool { _, n := sink.get(); return n == 16 })

	expect(t, sig, "suspend", []byte{0x80, 0x09, 0x04}, []byte{0x82, 0x09})
	if ev, _ := sink.get(); len(ev) != 2 || ev[1] != "stop" {
		t.Fatalf("events %v", ev)
	}
	expect(t, sig, "unknown", []byte{0x90, 0x2F}, []byte{0x91, 0x2F})
	expect(t, sig, "delayreport", []byte{0xA0, 0x0D, 0x04, 0x00, 0x10}, []byte{0xA3, 0x0D, 0x19})
	expect(t, sig, "close", []byte{0xB0, 0x08, 0x04}, []byte{0xB2, 0x08})
	waitFor(t, func() bool { return len(s.Streams()) == 0 })
	// media channel was closed by the server
	if _, ok := <-media.in; ok {
		t.Fatal("media not closed")
	}
	sig.Close()
	waitFor(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return len(s.devs) == 0 })
}

func TestSEPInUseAndSignallingClose(t *testing.T) {
	sink := &fakeSink{}
	s := newTestServer(sink)
	a, b := newConn(1), newConn(2)
	go s.HandleConn(a)
	go s.HandleConn(b)
	expect(t, a, "setconf", append([]byte{0x00, 0x03}, setConf...), []byte{0x02, 0x03})
	expect(t, a, "open", []byte{0x10, 0x06, 0x04}, []byte{0x12, 0x06})
	media := newConn(1)
	go s.HandleConn(media)
	waitFor(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.devs[a.addr].media != nil })
	expect(t, a, "start", []byte{0x20, 0x07, 0x04}, []byte{0x22, 0x07})
	expect(t, b, "discover-inuse", []byte{0x00, 0x01}, []byte{0x02, 0x01, 0x06, 0x08})
	expect(t, b, "setconf-inuse", append([]byte{0x10, 0x03}, setConf...), []byte{0x13, 0x03, 0x00, 0x13})
	a.Close() // signalling dies
	waitFor(t, func() bool { ev, _ := sink.get(); return len(ev) == 2 && ev[1] == "drop" })
	waitFor(t, func() bool { return len(s.Streams()) == 0 })
	expect(t, b, "setconf-free", append([]byte{0x30, 0x03}, setConf...), []byte{0x32, 0x03})
	b.Close()
}

func TestSilentStreamingOwnerEvicted(t *testing.T) {
	old := staleStreamAfter
	defer func() { staleStreamAfter = old }()
	s := newTestServer(&fakeSink{})
	a, b := newConn(1), newConn(2)
	go s.HandleConn(a)
	go s.HandleConn(b)
	expect(t, a, "setconf", append([]byte{0x00, 0x03}, setConf...), []byte{0x02, 0x03})
	expect(t, a, "open", []byte{0x10, 0x06, 0x04}, []byte{0x12, 0x06})
	media := newConn(1)
	go s.HandleConn(media)
	waitFor(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.devs[a.addr].media != nil })
	expect(t, a, "start", []byte{0x20, 0x07, 0x04}, []byte{0x22, 0x07})
	// Recently started: still protected.
	expect(t, b, "inuse", append([]byte{0x10, 0x03}, setConf...), []byte{0x13, 0x03, 0x00, 0x13})
	// No RTP for longer than the threshold: evicted.
	staleStreamAfter = time.Millisecond
	time.Sleep(10 * time.Millisecond)
	expect(t, b, "evict", append([]byte{0x20, 0x03}, setConf...), []byte{0x22, 0x03})
	b.Close()
}

func TestSetConfigValidation(t *testing.T) {
	s := newTestServer(&fakeSink{})
	c := newConn(1)
	go s.HandleConn(c)
	bad := func(mod func(b []byte), want ...byte) {
		t.Helper()
		cfg := append([]byte(nil), setConf...)
		mod(cfg)
		expect(t, c, "bad", append([]byte{0x00, 0x03}, cfg...), append([]byte{0x03, 0x03}, want...))
	}
	bad(func(b []byte) { b[8] = 0x80 | 0x01 }, 0x07, 0xC4)    // two rate bits? 0x80 is 16k: unsupported
	bad(func(b []byte) { b[8] = 0x21 | 0x10 }, 0x07, 0xC3)    // two rates
	bad(func(b []byte) { b[8] = 0x20 | 0x03 }, 0x07, 0xC5)    // two modes
	bad(func(b []byte) { b[9] = 0x15 | 0x40 }, 0x07, 0xDD)    // two blocks
	bad(func(b []byte) { b[9] = 0x80 | 0x01 }, 0x07, 0xC7)    // no subbands
	bad(func(b []byte) { b[9] = 0x15 | 0x02 }, 0x07, 0xC9)    // two alloc
	bad(func(b []byte) { b[10] = 1 }, 0x07, 0xCB)             // min bitpool
	bad(func(b []byte) { b[10], b[11] = 30, 20 }, 0x07, 0xCD) // max < min
	bad(func(b []byte) { b[7] = 0x01 }, 0x07, 0xC2)           // codec type
	bad(func(b []byte) { b[6] = 0x10 }, 0x07, 0xC1)           // media type
	expect(t, c, "badsc", []byte{0x00, 0x03, 0x04, 0x04, 0x01, 0x00}, []byte{0x03, 0x03, 0x07, 0x17})
	// Ok, then RECONFIGURE only when open.
	expect(t, c, "setconf", append([]byte{0x00, 0x03}, setConf...), []byte{0x02, 0x03})
	re := []byte{0x10, 0x05, 0x04, 0x07, 0x06, 0x00, 0x00, 0x10 | 0x02, 0x15, 0x02, 0x30}
	expect(t, c, "reconf-early", re, []byte{0x13, 0x05, 0x00, 0x31})
	expect(t, c, "open", []byte{0x20, 0x06, 0x04}, []byte{0x22, 0x06})
	expect(t, c, "reconf", re, []byte{0x12, 0x05})
	if st := s.Streams(); st[0].SampleRate != 48000 || st[0].Blocks != 16 || st[0].Mode != "stereo" || st[0].Bitpool != 0x30 {
		t.Fatalf("%+v", st)
	}
	c.Close()
}

func TestFragmentedCommand(t *testing.T) {
	s := newTestServer(&fakeSink{})
	c := newConn(1)
	go s.HandleConn(c)
	body := setConf
	c.in <- append([]byte{0x04, 0x03, 0x03}, body[:5]...) // start: label0, 3 packets
	c.in <- append([]byte{0x08}, body[5:9]...)            // continue
	r := rpc(t, c, append([]byte{0x0C}, body[9:]...))     // end
	if !bytes.Equal(r, []byte{0x02, 0x03}) {
		t.Fatalf("%x", r)
	}
	c.Close()
}

func TestReassemblyBounds(t *testing.T) {
	s := newTestServer(&fakeSink{})
	d := &device{}
	ok := func(p []byte) bool { _, _, _, _, ok := s.reassemble(d, p); return ok }
	// START announcing a single packet is invalid.
	if ok([]byte{0x04, 0x01, 0x03, 1}) || d.reasm != nil {
		t.Fatal("start with nPkts<2 accepted")
	}
	// CONTINUE reaching nPkts without END drops the reassembly.
	ok([]byte{0x04, 0x02, 0x03, 1})
	if ok([]byte{0x08, 2}) || d.reasm != nil {
		t.Fatal("overrun continue kept")
	}
	// Oversized message dropped.
	ok([]byte{0x04, 0xFF, 0x03, 1})
	big := append([]byte{0x08}, make([]byte, 600)...)
	ok(big)
	if ok(big) || d.reasm != nil {
		t.Fatal("oversized reassembly kept")
	}
	// A well-formed 2-packet message still works.
	ok([]byte{0x04, 0x02, 0x03, 4})
	if !ok([]byte{0x0C, 0x08}) {
		t.Fatal("valid fragments rejected")
	}
}

func TestMismatchedFramesDropped(t *testing.T) {
	t.Run("rate", func(t *testing.T) { testMismatchedFrames(t, &fakeDec{rate: 48000}) })     // configured for 44.1k
	t.Run("subbands", func(t *testing.T) { testMismatchedFrames(t, &fakeDec{subbands: 4}) }) // configured for 8
}

func testMismatchedFrames(t *testing.T, dec *fakeDec) {
	sink := &fakeSink{}
	s := newTestServer(sink)
	s.newDecoder = func() decoder { return dec }
	sig := newConn(1)
	go s.HandleConn(sig)
	expect(t, sig, "setconf", append([]byte{0x30, 0x03}, setConf...), []byte{0x32, 0x03})
	expect(t, sig, "open", []byte{0x60, 0x06, 0x04}, []byte{0x62, 0x06})
	media := newConn(1)
	go s.HandleConn(media)
	waitFor(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.devs[sig.addr].media != nil })
	expect(t, sig, "start", []byte{0x70, 0x07, 0x04}, []byte{0x72, 0x07})
	media.in <- append([]byte{0x80, 0x60, 0, 1, 0, 0, 0, 1, 0, 0, 0, 1, 0x02}, 1, 2, 3, 4, 5, 6, 7, 8)
	media.in <- append([]byte{0x80, 0x60, 0, 2, 0, 0, 0, 1, 0, 0, 0, 1, 0x01}, 1, 2, 3, 4)
	time.Sleep(50 * time.Millisecond)
	// Mismatched frames are replaced by silence, never played.
	waitFor(t, func() bool { _, n := sink.get(); return n > 0 })
	sink.mu.Lock()
	for _, v := range sink.pcm {
		if v != 0 {
			t.Fatal("mismatched stream produced non-silent audio")
		}
	}
	sink.mu.Unlock()
	// Each mismatched frame has a known length and is counted on its own: 2 + 1.
	if st := s.Stats(); st.BadFrames != 3 {
		t.Fatalf("bad frames %d, want 3", st.BadFrames)
	}
	sig.Close()
}

// Concurrent START and sink stops must leave sink state consistent with d.sinkOn.
func TestStartStopRace(t *testing.T) {
	sink := &fakeSink{}
	s := newTestServer(sink)
	d := &device{addr: bt.Addr{1}, media: newConn(1)}
	s.devs[d.addr] = d
	s.owner = d
	for i := 0; i < 500; i++ {
		s.mu.Lock()
		d.st = stOpen
		s.mu.Unlock()
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); s.doStart(d, 0, []byte{0x04}) }()
		go func() { defer wg.Done(); s.stopSink(d, false) }()
		wg.Wait()
		ev, _ := sink.get()
		starts, stops := 0, 0
		for _, e := range ev {
			if e == "start" {
				starts++
			} else {
				stops++
			}
		}
		s.mu.Lock()
		on := d.sinkOn
		s.mu.Unlock()
		if (starts-stops == 1) != on || starts-stops < 0 || starts-stops > 1 {
			t.Fatalf("iter %d: events %v sinkOn=%v", i, ev, on)
		}
		s.stopSink(d, false)
		sink.mu.Lock()
		sink.events = nil
		sink.mu.Unlock()
	}
}

func TestStaleOwnerEvicted(t *testing.T) {
	s := newTestServer(&fakeSink{})
	a, b := newConn(1), newConn(2)
	go s.HandleConn(a)
	go s.HandleConn(b)
	expect(t, a, "setconf", append([]byte{0x00, 0x03}, setConf...), []byte{0x02, 0x03})
	expect(t, b, "setconf-evict", append([]byte{0x10, 0x03}, setConf...), []byte{0x12, 0x03})
	if _, ok := <-a.in; ok { // owner's signalling channel was closed
		t.Fatal("old owner not closed")
	}
	b.Close()
}

func TestReconnectReplacesSignalling(t *testing.T) {
	s := newTestServer(&fakeSink{})
	a1 := newConn(1)
	go s.HandleConn(a1)
	expect(t, a1, "setconf", append([]byte{0x00, 0x03}, setConf...), []byte{0x02, 0x03})
	a2 := newConn(1) // same address, old channel still lingering
	go s.HandleConn(a2)
	expect(t, a2, "setconf2", append([]byte{0x10, 0x03}, setConf...), []byte{0x12, 0x03})
	if _, ok := <-a1.in; ok {
		t.Fatal("stale signalling not closed")
	}
	a2.Close()
}

func TestStartWithoutMediaAndAbort(t *testing.T) {
	s := newTestServer(&fakeSink{})
	a, b := newConn(1), newConn(2)
	go s.HandleConn(a)
	go s.HandleConn(b)
	expect(t, a, "setconf", append([]byte{0x00, 0x03}, setConf...), []byte{0x02, 0x03})
	expect(t, a, "open", []byte{0x10, 0x06, 0x04}, []byte{0x12, 0x06})
	expect(t, a, "start-nomedia", []byte{0x20, 0x07, 0x04}, []byte{0x23, 0x07, 0x04, 0x31})
	expect(t, b, "abort-nonowner", []byte{0x00, 0x0A, 0x04}, []byte{0x02, 0x0A})
	if len(s.Streams()) != 1 {
		t.Fatal("non-owner abort released the owner")
	}
	expect(t, a, "abort", []byte{0x30, 0x0A, 0x04}, []byte{0x32, 0x0A})
	if len(s.Streams()) != 0 {
		t.Fatal("owner abort did not release")
	}
	a.Close()
	b.Close()
}

func TestMaxBitpoolBounded(t *testing.T) {
	s := newTestServer(&fakeSink{})
	c := newConn(1)
	go s.HandleConn(c)
	cfg := append([]byte(nil), setConf...)
	cfg[11] = 54 // above advertised 53
	expect(t, c, "maxpool", append([]byte{0x00, 0x03}, cfg...), []byte{0x02, 0x03})
	if st := s.Streams(); len(st) != 1 || st[0].Bitpool != 53 {
		t.Fatalf("not clamped: %+v", st)
	}
	expect(t, c, "again", append([]byte{0x10, 0x03}, setConf...), []byte{0x13, 0x03, 0x00, 0x13}) // own SEP in use
	c.Close()
}

func TestSetSampleRatesUpdatesCapsAndValidation(t *testing.T) {
	s := newTestServer(&fakeSink{})
	c := newConn(1)
	go s.HandleConn(c)
	defer c.Close()
	s.SetSampleRates([]int{48000})
	// Only 48k advertised now.
	expect(t, c, "getallcaps", []byte{0x00, 0x0C, 0x04},
		[]byte{0x02, 0x0C, 0x01, 0x00, 0x07, 0x06, 0x00, 0x00, 0x10 | 0x0F, 0xFF, 0x02, 53})
	// 44.1k (setConf) is now rejected with UNSUPPORTED_CONFIGURATION-class sample-rate error.
	expect(t, c, "setconf44", append([]byte{0x00, 0x03}, setConf...), []byte{0x03, 0x03, 0x07, 0xC4})
	s.SetSampleRates(nil) // ignored
	if !s.supportedRate(48000) || s.supportedRate(44100) {
		t.Fatal("empty SetSampleRates must be a no-op")
	}
}

func TestDiscoverInUseOnlyWhenStreaming(t *testing.T) {
	s := newTestServer(&fakeSink{})
	a, b := newConn(1), newConn(2)
	go s.HandleConn(a)
	go s.HandleConn(b)
	expect(t, a, "setconf", append([]byte{0x00, 0x03}, setConf...), []byte{0x02, 0x03})
	expect(t, b, "discover-configured", []byte{0x00, 0x01}, []byte{0x02, 0x01, 0x04, 0x08})
	expect(t, a, "open", []byte{0x10, 0x06, 0x04}, []byte{0x12, 0x06})
	media := newConn(1)
	go s.HandleConn(media)
	waitFor(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.devs[a.addr].media != nil })
	expect(t, b, "discover-open", []byte{0x10, 0x01}, []byte{0x12, 0x01, 0x04, 0x08})
	expect(t, a, "start", []byte{0x20, 0x07, 0x04}, []byte{0x22, 0x07})
	expect(t, b, "discover-streaming", []byte{0x20, 0x01}, []byte{0x22, 0x01, 0x06, 0x08})
	expect(t, a, "suspend", []byte{0x30, 0x09, 0x04}, []byte{0x32, 0x09})
	expect(t, b, "discover-suspended", []byte{0x30, 0x01}, []byte{0x32, 0x01, 0x04, 0x08})
	a.Close()
	b.Close()
}

func TestStartBeforeMediaRegisters(t *testing.T) {
	s := newTestServer(&fakeSink{})
	a := newConn(1)
	go s.HandleConn(a)
	expect(t, a, "setconf", append([]byte{0x00, 0x03}, setConf...), []byte{0x02, 0x03})
	expect(t, a, "open", []byte{0x10, 0x06, 0x04}, []byte{0x12, 0x06})
	a.in <- []byte{0x20, 0x07, 0x04}
	time.Sleep(60 * time.Millisecond)
	go s.HandleConn(newConn(1))
	select {
	case r := <-a.out:
		if !bytes.Equal(r, []byte{0x22, 0x07}) {
			t.Fatalf("got %x", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout")
	}
	a.Close()
}
