package player

import (
	"encoding/binary"
	"errors"
	"sync"
	"testing"
	"time"

	"btspeaker/internal/alsa"
)

type fakePCM struct {
	mu      sync.Mutex
	data    []byte
	drained bool
	closed  bool
	aborted bool
}

func (f *fakePCM) Write(b []byte) error {
	f.mu.Lock()
	f.data = append(f.data, b...)
	f.mu.Unlock()
	return nil
}
func (f *fakePCM) Drain() error { f.mu.Lock(); f.drained = true; f.mu.Unlock(); return nil }
func (f *fakePCM) Abort() error { f.mu.Lock(); f.aborted = true; f.mu.Unlock(); return nil }
func (f *fakePCM) Close() error { f.mu.Lock(); f.closed = true; f.mu.Unlock(); return nil }
func (f *fakePCM) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}
func (f *fakePCM) bytes() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.data...)
}

type fakeBackend struct {
	mu     sync.Mutex
	caps   alsa.Caps
	errs   []error // consumed per Open call; nil entry = success
	opens  int
	probes int
	pcms   []*fakePCM
	last   struct {
		rate, ch int
		f        alsa.Format
	}
}

func (b *fakeBackend) Probe(string) (alsa.Caps, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.probes++
	return b.caps, nil
}
func (b *fakeBackend) Open(_ string, rate, ch int, f alsa.Format, _ int) (PCM, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.opens++
	if len(b.errs) > 0 {
		e := b.errs[0]
		b.errs = b.errs[1:]
		if e != nil {
			return nil, e
		}
	}
	p := &fakePCM{}
	b.pcms = append(b.pcms, p)
	b.last.rate, b.last.ch, b.last.f = rate, ch, f
	return p, nil
}
func (b *fakeBackend) pcm(i int) *fakePCM {
	b.mu.Lock()
	defer b.mu.Unlock()
	if i < len(b.pcms) {
		return b.pcms[i]
	}
	return nil
}
func (b *fakeBackend) nOpens() int { b.mu.Lock(); defer b.mu.Unlock(); return b.opens }

func stdCaps() alsa.Caps {
	return alsa.Caps{Rates: []int{44100, 48000}, Formats: []alsa.Format{alsa.S16LE, alsa.S32LE}, MinChannels: 2, MaxChannels: 2}
}

func newTest(b *fakeBackend, cfg Config) *Player {
	p := NewWithBackend(cfg, b)
	p.retryEvery = 150 * time.Millisecond
	p.noWrite = 150 * time.Millisecond
	return p
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

var loud = func() []int16 {
	s := make([]int16, 441*2)
	for i := range s {
		s[i] = int16(1000 + i)
	}
	return s
}()
var quiet = make([]int16, 441*2) // 10 ms of silence

func TestOpenOnAudioAndSilenceIgnored(t *testing.T) {
	b := &fakeBackend{caps: stdCaps()}
	p := newTest(b, Config{SoftVolume: false, IdleRelease: time.Hour})
	defer p.Close()
	p.Start(44100, 2)
	p.Write(quiet)
	if p.Status().Open || b.nOpens() != 0 {
		t.Fatal("silence must not open the device")
	}
	p.Write(loud)
	if !p.Status().Open {
		t.Fatal("not open")
	}
	waitFor(t, "data", func() bool { return len(b.pcm(0).bytes()) == len(loud)*2 })
	got := b.pcm(0).bytes()
	// The first ~10 ms fade in from silence (every open), so only the start and
	// the end of the chunk are checked: 0, then within 1% of the input.
	last := int16(binary.LittleEndian.Uint16(got[len(got)-4:]))
	if int16(binary.LittleEndian.Uint16(got)) != 0 || last > loud[len(loud)-2] || int(last) < int(loud[len(loud)-2])*99/100 {
		t.Fatal("passthrough mismatch")
	}
}

func TestBusyBackoff(t *testing.T) {
	b := &fakeBackend{caps: stdCaps(), errs: []error{alsa.ErrBusy, alsa.ErrBusy}}
	p := newTest(b, Config{IdleRelease: time.Hour})
	defer p.Close()
	p.Start(44100, 2)
	p.Write(loud)
	if st := p.Status(); st.Open || st.LastError != "ALSA device busy (mpd playing?)" {
		t.Fatalf("%+v", st)
	}
	p.Write(loud)
	p.Write(loud)
	if b.nOpens() != 1 {
		t.Fatalf("retried too early: %d opens", b.nOpens())
	}
	time.Sleep(170 * time.Millisecond)
	p.Write(loud)
	if b.nOpens() != 2 || p.Status().Open {
		t.Fatalf("second attempt expected: opens=%d", b.nOpens())
	}
	time.Sleep(170 * time.Millisecond)
	p.Write(loud)
	if b.nOpens() != 3 || !p.Status().Open || p.Status().LastError != "" {
		t.Fatalf("third attempt should succeed: %d %+v", b.nOpens(), p.Status())
	}
	if b.probes != 1 {
		t.Fatalf("caps should be cached, probes=%d", b.probes)
	}
}

func TestOtherErrorRetries(t *testing.T) {
	b := &fakeBackend{caps: stdCaps(), errs: []error{errString("boom")}}
	p := newTest(b, Config{IdleRelease: time.Hour})
	defer p.Close()
	p.Start(44100, 2)
	p.Write(loud)
	if p.Status().LastError != "boom" {
		t.Fatalf("%+v", p.Status())
	}
	time.Sleep(170 * time.Millisecond)
	p.Write(loud)
	if !p.Status().Open {
		t.Fatal("should reopen")
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestIdleSilenceReleaseAndReopen(t *testing.T) {
	b := &fakeBackend{caps: stdCaps()}
	p := newTest(b, Config{IdleRelease: 50 * time.Millisecond})
	defer p.Close()
	p.Start(44100, 2)
	p.Write(loud)
	pc := b.pcm(0)
	for i := 0; i < 4; i++ { // 40 ms of silence: still held
		p.Write(quiet)
	}
	if !p.Status().Open {
		t.Fatal("released too early")
	}
	for i := 0; i < 2; i++ {
		p.Write(quiet)
	}
	if p.Status().Open {
		t.Fatal("should release after idle silence")
	}
	waitFor(t, "close", pc.isClosed)
	if pc.drained || !pc.aborted {
		t.Fatalf("idle release should drop queued silence: drained=%v aborted=%v", pc.drained, pc.aborted)
	}
	p.Write(loud)
	if !p.Status().Open || b.nOpens() != 2 {
		t.Fatalf("reopen on audio: opens=%d", b.nOpens())
	}
}

func TestNoWriteWatchdog(t *testing.T) {
	b := &fakeBackend{caps: stdCaps()}
	p := newTest(b, Config{IdleRelease: time.Hour})
	defer p.Close()
	p.Start(44100, 2)
	p.Write(loud)
	waitFor(t, "watchdog release", func() bool { return !p.Status().Open })
	waitFor(t, "close", b.pcm(0).isClosed)
}

func TestStopReleases(t *testing.T) {
	b := &fakeBackend{caps: stdCaps()}
	p := newTest(b, Config{IdleRelease: time.Hour})
	defer p.Close()
	p.Start(44100, 2)
	p.Write(loud)
	p.Stop()
	if p.Status().Open || !b.pcm(0).isClosed() {
		t.Fatal("Stop must release synchronously")
	}
	p.Write(loud) // same stream continues: reopens
	if !p.Status().Open {
		t.Fatal("reopen")
	}
}

func TestFormatSelectionAndMono(t *testing.T) {
	c := stdCaps()
	c.Formats = []alsa.Format{alsa.S24LE, alsa.S32LE}
	b := &fakeBackend{caps: c}
	p := newTest(b, Config{IdleRelease: time.Hour})
	defer p.Close()
	p.Start(44100, 1)
	p.Write([]int16{1000, -1000})
	if b.last.f != alsa.S32LE || b.last.ch != 2 {
		t.Fatalf("%+v", b.last)
	}
	waitFor(t, "data", func() bool { return len(b.pcm(0).bytes()) == 16 })
	d := b.pcm(0).bytes()
	w := func(i int) int32 { return int32(binary.LittleEndian.Uint32(d[i*4:])) }
	// the open fades in: frame 0 is silent, frame 1 is slightly attenuated
	if w(0) != 0 || w(1) != 0 || w(2) != w(3) || w(2) >= 0 || w(2) <= -1000<<16 {
		t.Fatalf("% x", d)
	}
	if f, _, _ := Negotiate(alsa.Caps{Rates: []int{44100}, Formats: []alsa.Format{alsa.S24LE}, MinChannels: 2, MaxChannels: 2}, 44100, 2, false); f != alsa.S24LE {
		t.Fatal("s24")
	}
	if _, _, err := Negotiate(c, 16000, 2, false); err == nil {
		t.Fatal("rate")
	}
}

func TestGainMath(t *testing.T) {
	if gainTable[0] != 0 || gainTable[127] != unity {
		t.Fatal("endpoints")
	}
	for v := 1; v < 128; v++ {
		if gainTable[v] <= gainTable[v-1] {
			t.Fatalf("not monotonic at %d", v)
		}
	}
	// v=100 ≈ -10.6 dB ≈ 0.2958
	if g := float64(gainTable[100]) / unity; g < 0.29 || g > 0.30 {
		t.Fatalf("gain(100)=%v", g)
	}
	in := []int16{32767, -32768, 1000, 0}
	out := make([]byte, 8)
	Convert(out, in, 1, 1, alsa.S16LE, unity)
	if int16(binary.LittleEndian.Uint16(out)) != 32767 || int16(binary.LittleEndian.Uint16(out[2:])) != -32768 {
		t.Fatal("unity passthrough")
	}
	Convert(out, in, 1, 1, alsa.S16LE, unity/2)
	if int16(binary.LittleEndian.Uint16(out[4:])) != 500 || int16(binary.LittleEndian.Uint16(out[2:])) != -16384 {
		t.Fatalf("half gain % x", out)
	}
	Convert(out, in, 1, 1, alsa.S16LE, 0)
	for _, x := range out {
		if x != 0 {
			t.Fatal("mute")
		}
	}
	// clipping with gain > 1
	Convert(out, in, 1, 1, alsa.S16LE, 2*unity)
	if int16(binary.LittleEndian.Uint16(out)) != 32767 || int16(binary.LittleEndian.Uint16(out[2:])) != -32768 {
		t.Fatal("clip16")
	}
	o4 := make([]byte, 16)
	// S32 has no clipping: gain must be <= unity (see Convert).
	Convert(o4, in, 1, 1, alsa.S32LE, unity)
	if int32(binary.LittleEndian.Uint32(o4)) != 32767<<16 || int32(binary.LittleEndian.Uint32(o4[4:])) != -32768<<16 {
		t.Fatal("s32 unity extremes")
	}
	Convert(o4, in, 1, 1, alsa.S24LE, unity)
	if int32(binary.LittleEndian.Uint32(o4[8:])) != 1000<<8 || int32(binary.LittleEndian.Uint32(o4[4:])) != -32768<<8 {
		t.Fatalf("s24 % x", o4)
	}
}

func TestSoftVolumeApplied(t *testing.T) {
	b := &fakeBackend{caps: stdCaps()}
	p := newTest(b, Config{SoftVolume: true, IdleRelease: time.Hour})
	defer p.Close()
	p.SetVolume(0)
	if p.Volume() != 0 {
		t.Fatal()
	}
	p.Start(44100, 2)
	p.Write(loud)
	waitFor(t, "data", func() bool { return len(b.pcm(0).bytes()) == len(loud)*4 }) // stdCaps has S32: preferred with soft volume
	for _, x := range b.pcm(0).bytes() {
		if x != 0 {
			t.Fatal("muted output expected")
		}
	}
	// without SoftVolume the volume is recorded only
	q := newTest(&fakeBackend{caps: stdCaps()}, Config{})
	q.SetVolume(42)
	if q.Status().Volume != 42 {
		t.Fatal("status volume")
	}
}

func TestTestToneFake(t *testing.T) {
	b := &fakeBackend{caps: stdCaps()}
	if err := testTone(b, "hw:0,0", 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	pc := b.pcm(0)
	if !pc.drained || !pc.closed || len(pc.bytes()) != 4410*2*2 {
		t.Fatalf("drained=%v closed=%v n=%d", pc.drained, pc.closed, len(pc.bytes()))
	}
}

type xrunPCM struct{ fakePCM }

func (x *xrunPCM) Xruns() int64 { return 3 }

func TestXrunsInStatus(t *testing.T) {
	b := &xrunBackend{fakeBackend{caps: stdCaps()}}
	p := newTest(&b.fakeBackend, Config{IdleRelease: time.Hour})
	p.backend = b
	defer p.Close()
	p.Start(44100, 2)
	p.Write(loud)
	waitFor(t, "xruns", func() bool { return p.Status().Xruns == 3 })
}

type xrunBackend struct{ fakeBackend }

func (b *xrunBackend) Open(dev string, rate, ch int, f alsa.Format, n int) (PCM, error) {
	return &xrunPCM{}, nil
}

// midXrunPCM simulates an xrun in the middle of its first Write: the first half is
// written, the hook runs on the rest, then the rest is written.
type midXrunPCM struct {
	fakePCM
	hook func([]byte)
	n    int
}

func (m *midXrunPCM) SetAfterXrun(f func([]byte)) { m.hook = f }
func (m *midXrunPCM) Write(b []byte) error {
	m.n++
	if m.n == 1 {
		h := len(b) / 8 * 4 // whole S16 stereo frames
		m.fakePCM.Write(b[:h])
		m.hook(b[h:])
		b = b[h:]
	}
	return m.fakePCM.Write(b)
}

type midXrunBackend struct {
	fakeBackend
	m *midXrunPCM
}

func (b *midXrunBackend) Open(string, int, int, alsa.Format, int) (PCM, error) { return b.m, nil }

func TestXrunFadeInPlaceAndAcrossChunks(t *testing.T) {
	m := &midXrunPCM{}
	b := &midXrunBackend{fakeBackend{caps: stdCaps()}, m}
	p := NewWithBackend(Config{IdleRelease: time.Hour}, b)
	defer p.Close()
	p.Start(44100, 2)
	const chunk = 128 // frames: shorter than fadeFrames
	in := make([]int16, chunk*2)
	for i := range in {
		in[i] = 10000
	}
	n := 2 * (fadeFrames + 2*chunk) / chunk
	for range n {
		p.Write(in)
	}
	waitFor(t, "data", func() bool { return len(m.bytes()) == n*chunk*4 })
	d := m.bytes()
	v := func(fr int) int16 { return int16(binary.LittleEndian.Uint16(d[fr*4:])) }
	half := chunk / 2
	if v(0) != 0 || v(half-1) <= 0 || v(half-1) >= 10000 { // the open itself fades in
		t.Fatal("audio before the xrun not fading in from the open")
	}
	if v(half) != 0 {
		t.Fatalf("remainder not faded from silence: %d", v(half))
	}
	for fr := half + 1; fr < n*chunk; fr++ {
		if v(fr) < v(fr-1) {
			t.Fatalf("ramp not monotonic at frame %d: %d < %d", fr, v(fr), v(fr-1))
		}
	}
	if v(half+fadeFrames-1) == 10000 || v(half+fadeFrames) != 10000 || v(n*chunk-1) != 10000 {
		t.Fatalf("ramp does not end at unity after %d frames", fadeFrames)
	}
}

func TestWriteDoesNotWaitForClosingSession(t *testing.T) {
	h := &hangPCM{release: make(chan struct{}), started: make(chan struct{})}
	b := &hangBackend{fakeBackend: fakeBackend{caps: stdCaps()}, h: h}
	p := NewWithBackend(Config{}, b)
	p.Start(44100, 2)
	p.Write(loud)
	<-h.started
	p.mu.Lock()
	p.releaseLocked() // e.g. the no-Write watchdog: drain is stuck in the driver
	p.mu.Unlock()
	start := time.Now()
	p.Write(loud)
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("Write blocked %v on the closing session", d)
	}
	if p.Status().Open {
		t.Fatal("reopened before the previous session finished")
	}
	p.Close()
}

func TestPrerollZerosBeforeAudio(t *testing.T) {
	b := &fakeBackend{caps: stdCaps()}
	p := newTest(b, Config{IdleRelease: time.Hour, Preroll: 100 * time.Millisecond})
	defer p.Close()
	p.Start(44100, 2)
	p.Write(loud)
	pre := 4410 * 2 * 2 // 100 ms, stereo, S16
	waitFor(t, "data", func() bool { return len(b.pcm(0).bytes()) == pre+len(loud)*2 })
	got := b.pcm(0).bytes()
	for i, v := range got[:pre] {
		if v != 0 {
			t.Fatalf("preroll byte %d = %d", i, v)
		}
	}
	end := int16(binary.LittleEndian.Uint16(got[len(got)-4:])) // start is faded in
	if int(end) < int(loud[len(loud)-2])*99/100 || end > loud[len(loud)-2] {
		t.Fatal("audio does not follow the preroll")
	}
}

// gatedPCM blocks every Write until gate is closed.
type gatedPCM struct {
	fakePCM
	gate chan struct{}
}

func (g *gatedPCM) Write(b []byte) error { <-g.gate; return g.fakePCM.Write(b) }

type gatedBackend struct {
	fakeBackend
	pcm  PCM
	bits int
}

func (b *gatedBackend) Open(dev string, rate, ch int, f alsa.Format, n int) (PCM, error) {
	b.fakeBackend.Open(dev, rate, ch, f, n)
	return b.pcm, nil
}

func TestQueueDropCounted(t *testing.T) {
	g := &gatedPCM{gate: make(chan struct{})}
	b := &gatedBackend{fakeBackend: fakeBackend{caps: stdCaps()}, pcm: g}
	p := newTest(&b.fakeBackend, Config{IdleRelease: time.Hour})
	p.backend = b
	defer p.Close()
	defer func() {
		select {
		case <-g.gate:
		default:
			close(g.gate)
		}
	}()
	p.Start(44100, 2)
	for i := 0; i < 30; i++ { // 30 x 10 ms against a 100 ms bound and a stalled device
		p.Write(loud)
	}
	if d := p.Status().QueueDrop; d < 10*441 {
		t.Fatalf("QueueDrop = %d frames, want >= %d", d, 10*441)
	}
}

func TestFadeTail(t *testing.T) {
	last := []byte{0xe8, 0x03, 0x18, 0xfc} // L=1000 R=-1000
	b := fadeTail(last, alsa.S16LE, 2, fadeFrames)
	if len(b) != fadeFrames*4 {
		t.Fatal("length")
	}
	l0 := int16(binary.LittleEndian.Uint16(b))
	lend := int16(binary.LittleEndian.Uint16(b[len(b)-4:]))
	if l0 < 990 || l0 > 1000 || lend != 0 {
		t.Fatalf("first=%d last=%d", l0, lend)
	}
}

type msbitsPCM struct {
	fakePCM
	bits int
}

func (m *msbitsPCM) MSBits() int { return m.bits }

func TestSoftVolumeFallsBackToS16(t *testing.T) {
	b := &gatedBackend{fakeBackend: fakeBackend{caps: stdCaps()}, pcm: &msbitsPCM{bits: 16}}
	p := newTest(&b.fakeBackend, Config{SoftVolume: true, IdleRelease: time.Hour})
	p.backend = b
	defer p.Close()
	p.Start(44100, 2)
	p.Write(loud)
	waitFor(t, "s16 reopen", func() bool { return b.nOpens() == 2 && b.last.f == alsa.S16LE })
	// Later sessions open S16 directly: no close/reopen gap for mpd to grab.
	p.Drop()
	p.Start(44100, 2)
	p.Write(loud)
	waitFor(t, "direct s16 open", func() bool { return b.nOpens() == 3 && b.last.f == alsa.S16LE })
}

func TestCapsClearedOnNonBusyOpenFailure(t *testing.T) {
	b := &fakeBackend{caps: stdCaps(), errs: []error{alsa.ErrBusy, errors.New("gone")}}
	p := newTest(b, Config{IdleRelease: time.Hour})
	defer p.Close()
	p.mu.Lock()
	p.narrow16 = true // from an earlier device: must be re-learnt after a replug
	p.mu.Unlock()
	p.Start(44100, 2)
	p.Write(loud)
	p.mu.Lock()
	kept := p.caps != nil
	p.mu.Unlock()
	if !kept {
		t.Fatal("busy must keep the probed caps")
	}
	waitFor(t, "non-busy failure", func() bool {
		p.Write(loud)
		p.mu.Lock()
		defer p.mu.Unlock()
		return b.nOpens() >= 2 && p.caps == nil && !p.narrow16
	})
}

func TestDropFadesOutThenIn(t *testing.T) {
	p := NewWithBackend(Config{}, &fakeBackend{caps: stdCaps()})
	m := &fakePCM{}
	s := &session{pcm: m, outCh: 2, fmt: alsa.S16LE, fadeN: fadeLen(16000), fadePos: 160}
	if s.fadeN != 160 {
		t.Fatalf("fadeLen(16000) = %d", s.fadeN)
	}
	s.last = []byte{0xe8, 0x03, 0xe8, 0x03} // 1000
	s.refade.Store(true)
	b := make([]byte, 400*4)
	for i := 0; i < len(b); i += 2 {
		binary.LittleEndian.PutUint16(b[i:], 1000)
	}
	if p.writeChunk(s, b, true) {
		t.Fatal("write failed")
	}
	d := m.bytes()
	if len(d) != (160+400)*4 {
		t.Fatalf("wrote %d bytes", len(d))
	}
	v := func(fr int) int16 { return int16(binary.LittleEndian.Uint16(d[fr*4:])) }
	if v(0) < 990 || v(159) != 0 || v(160) != 0 || v(160+159) >= 1000 || v(160+160) != 1000 {
		t.Fatalf("fade-out/in wrong: %d %d %d %d %d", v(0), v(159), v(160), v(319), v(320))
	}
}
