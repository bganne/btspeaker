package avrcp

import (
	"bytes"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"btspeaker/internal/bt"
)

type fakeConn struct {
	in  chan []byte
	out chan []byte
}

func newFakeConn() *fakeConn { return &fakeConn{in: make(chan []byte, 16), out: make(chan []byte, 16)} }
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
func (f *fakeConn) Close() error        { return nil }
func (f *fakeConn) RemoteAddr() bt.Addr { return bt.Addr{} }
func (f *fakeConn) PSM() uint16         { return 23 }
func (f *fakeConn) InMTU() int          { return 672 }
func (f *fakeConn) OutMTU() int         { return 672 }

type fakeVol struct {
	mu sync.Mutex
	v  uint8
}

func (f *fakeVol) SetVolume(v uint8) { f.mu.Lock(); f.v = v; f.mu.Unlock() }
func (f *fakeVol) Volume() uint8     { f.mu.Lock(); defer f.mu.Unlock(); return f.v }

func exchange(t *testing.T, c *fakeConn, req []byte) []byte {
	t.Helper()
	c.in <- req
	return <-c.out
}

func TestSequence(t *testing.T) {
	v := &fakeVol{v: 42}
	s := NewServer(v)
	c := newFakeConn()
	done := make(chan struct{})
	go func() { s.HandleConn(c); close(done) }()

	check := func(name string, req, want []byte) {
		t.Helper()
		if got := exchange(t, c, req); !bytes.Equal(got, want) {
			t.Fatalf("%s: got %x want %x", name, got, want)
		}
	}
	// UNIT INFO
	check("unitinfo", []byte{0x10, 0x11, 0x0E, 0x01, 0xFF, 0x30, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF},
		[]byte{0x12, 0x11, 0x0E, 0x0C, 0xFF, 0x30, 0x07, 0x48, 0x00, 0x19, 0x58})
	// SUBUNIT INFO
	check("subunitinfo", []byte{0x20, 0x11, 0x0E, 0x01, 0xFF, 0x31, 0x07, 0xFF, 0xFF, 0xFF, 0xFF},
		[]byte{0x22, 0x11, 0x0E, 0x0C, 0xFF, 0x31, 0x07, 0x48, 0xFF, 0xFF, 0xFF})
	// PASS THROUGH (play) -> NOT_IMPLEMENTED
	check("passthrough", []byte{0x30, 0x11, 0x0E, 0x00, 0x48, 0x7C, 0x44, 0x00},
		[]byte{0x32, 0x11, 0x0E, 0x08, 0x48, 0x7C, 0x44, 0x00})
	// GetCapabilities company id
	check("caps2", []byte{0x40, 0x11, 0x0E, 0x01, 0x48, 0x00, 0x00, 0x19, 0x58, 0x10, 0x00, 0x00, 0x01, 0x02},
		[]byte{0x42, 0x11, 0x0E, 0x0C, 0x48, 0x00, 0x00, 0x19, 0x58, 0x10, 0x00, 0x00, 0x05, 0x02, 0x01, 0x00, 0x19, 0x58})
	// GetCapabilities events
	check("caps3", []byte{0x50, 0x11, 0x0E, 0x01, 0x48, 0x00, 0x00, 0x19, 0x58, 0x10, 0x00, 0x00, 0x01, 0x03},
		[]byte{0x52, 0x11, 0x0E, 0x0C, 0x48, 0x00, 0x00, 0x19, 0x58, 0x10, 0x00, 0x00, 0x03, 0x03, 0x01, 0x0D})
	// RegisterNotification(volume)
	check("regnotif", []byte{0x60, 0x11, 0x0E, 0x03, 0x48, 0x00, 0x00, 0x19, 0x58, 0x31, 0x00, 0x00, 0x05, 0x0D, 0, 0, 0, 0},
		[]byte{0x62, 0x11, 0x0E, 0x0F, 0x48, 0x00, 0x00, 0x19, 0x58, 0x31, 0x00, 0x00, 0x02, 0x0D, 42})
	// SetAbsoluteVolume (high bit ignored)
	check("setvol", []byte{0x70, 0x11, 0x0E, 0x00, 0x48, 0x00, 0x00, 0x19, 0x58, 0x50, 0x00, 0x00, 0x01, 0xFF},
		[]byte{0x72, 0x11, 0x0E, 0x09, 0x48, 0x00, 0x00, 0x19, 0x58, 0x50, 0x00, 0x00, 0x01, 0x7F})
	if v.Volume() != 0x7F {
		t.Fatal("volume not applied")
	}
	// Unknown vendor PDU -> REJECTED invalid command
	check("unkpdu", []byte{0x80, 0x11, 0x0E, 0x01, 0x48, 0x00, 0x00, 0x19, 0x58, 0x20, 0x00, 0x00, 0x00},
		[]byte{0x82, 0x11, 0x0E, 0x0A, 0x48, 0x00, 0x00, 0x19, 0x58, 0x20, 0x00, 0x00, 0x01, 0x00})
	// Unknown PID -> IPID response
	check("ipid", []byte{0x90, 0x12, 0x34, 0x01, 0x48, 0x00},
		[]byte{0x93, 0x12, 0x34})
	// Foreign company -> NOT_IMPLEMENTED
	check("company", []byte{0xA0, 0x11, 0x0E, 0x01, 0x48, 0x00, 0x00, 0x00, 0x01, 0x10, 0x00, 0x00, 0x00},
		[]byte{0xA2, 0x11, 0x0E, 0x08, 0x48, 0x00, 0x00, 0x00, 0x01, 0x10, 0x00, 0x00, 0x00})

	// External volume change -> CHANGED with the registered label (0x6).
	s.NotifyVolume(0x20)
	got := <-c.out
	want := []byte{0x62, 0x11, 0x0E, 0x0D, 0x48, 0x00, 0x00, 0x19, 0x58, 0x31, 0x00, 0x00, 0x02, 0x0D, 0x20}
	if !bytes.Equal(got, want) {
		t.Fatalf("changed: got %x want %x", got, want)
	}
	// Fragmented packet is dropped silently; next request still works.
	c.in <- []byte{0xB4, 0x02, 0x11, 0x0E, 0x00}
	check("after-frag", []byte{0xC0, 0x11, 0x0E, 0x01, 0xFF, 0x30, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF},
		[]byte{0xC2, 0x11, 0x0E, 0x0C, 0xFF, 0x30, 0x07, 0x48, 0x00, 0x19, 0x58})
	close(c.in)
	<-done
}

func FuzzAVRCP(f *testing.F) {
	f.Add([]byte{0x00, 0x11, 0x0E, 0x01, 0x48, 0x00, 0x00, 0x19, 0x58, 0x50, 0x00, 0x00, 0x01, 0x40})
	f.Add([]byte{0x10, 0x11, 0x0E, 0x03, 0x48, 0x00, 0x00, 0x19, 0x58, 0x31, 0x00, 0x00, 0x05, 0x0D, 0, 0, 0, 0})
	f.Add([]byte{0x20, 0x11, 0x0E, 0x01, 0x48, 0x00, 0x00, 0x19, 0x58, 0x10, 0x00, 0x00, 0x01, 0x03})
	f.Add([]byte{0x30, 0x11, 0x0E, 0x01, 0x48, 0x30, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF})
	f.Add([]byte{0x00, 0x11, 0x0E, 0x00, 0x48, 0x7C, 0x44, 0x00})
	f.Fuzz(func(t *testing.T, p []byte) {
		s := NewServer(&fakeVol{})
		s.handle(newFakeConn(), p)
	})
}

// dlConn is a fakeConn whose Read honours SetReadDeadline.
type dlConn struct {
	*fakeConn
	mu sync.Mutex
	dl time.Time
}

func (c *dlConn) SetReadDeadline(t time.Time) error { c.mu.Lock(); c.dl = t; c.mu.Unlock(); return nil }
func (c *dlConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	dl := c.dl
	c.mu.Unlock()
	select {
	case b, ok := <-c.in:
		if !ok {
			return 0, io.EOF
		}
		return copy(p, b), nil
	case <-time.After(time.Until(dl)):
		return 0, os.ErrDeadlineExceeded
	}
}

func TestIdleTimeouts(t *testing.T) {
	oldF, oldI := firstPDUTimeout, idleTimeout
	firstPDUTimeout, idleTimeout = 30*time.Millisecond, 30*time.Millisecond
	defer func() { firstPDUTimeout, idleTimeout = oldF, oldI }()
	ping := []byte{0x10, 0x11, 0x0E, 0x01, 0xFF, 0x30, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}

	run := func(sess bool) (*dlConn, chan struct{}) {
		s := NewServer(&fakeVol{})
		s.HasSession = func(bt.Addr) bool { return sess }
		c := &dlConn{fakeConn: newFakeConn()}
		done := make(chan struct{})
		go func() { s.HandleConn(c); close(done) }()
		c.in <- ping
		<-c.out
		return c, done
	}
	// No AVDTP session: closed after the idle timeout.
	_, done := run(false)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("idle channel not closed")
	}
	// AVDTP session up: stays open well past the timeout and still serves.
	c, done := run(true)
	time.Sleep(150 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("closed while AVDTP is up")
	default:
	}
	if got := exchange(t, c.fakeConn, ping); len(got) == 0 {
		t.Fatal("no response")
	}
	close(c.in)
	<-done
	// A peer that never sends a valid PDU is closed by the first-PDU deadline unless
	// it has an AVDTP session; junk bytes don't earn the (here long) idle timeout.
	idleTimeout = time.Hour
	silent := func(sess bool, junk []byte) (chan struct{}, *dlConn) {
		s := NewServer(&fakeVol{})
		s.HasSession = func(bt.Addr) bool { return sess }
		c := &dlConn{fakeConn: newFakeConn()}
		done := make(chan struct{})
		go func() { s.HandleConn(c); close(done) }()
		if junk != nil {
			c.in <- junk
		}
		return done, c
	}
	for _, junk := range [][]byte{nil, {0x42}, {0x00, 0x00, 0x01, 0x01, 0x48, 0x00}} {
		d, _ := silent(false, junk)
		select {
		case <-d:
		case <-time.After(2 * time.Second):
			t.Fatalf("silent peer not closed (junk %x)", junk)
		}
	}
	// Junk arriving more often than the first-PDU timeout must not re-arm it.
	d, jc := silent(false, nil)
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
				select {
				case jc.in <- []byte{0x42}:
				case <-stop:
					return
				}
			}
		}
	}()
	select {
	case <-d:
	case <-time.After(2 * time.Second):
		t.Fatal("junk kept the first-PDU deadline re-armed")
	}
	close(stop)
	d, sc := silent(true, nil)
	select {
	case <-d:
		t.Fatal("silent peer with an AVDTP session closed")
	case <-time.After(150 * time.Millisecond):
	}
	close(sc.in)
	<-d // before the deferred timeout restore
}

func TestSetVolumeNeedsOwner(t *testing.T) {
	v := &fakeVol{v: 10}
	s := NewServer(v)
	s.MayControlVolume = func(bt.Addr) bool { return false }
	got := s.handle(newFakeConn(), []byte{0x70, 0x11, 0x0E, 0x00, 0x48, 0x00, 0x00, 0x19, 0x58, 0x50, 0x00, 0x00, 0x01, 0x40})
	want := []byte{0x72, 0x11, 0x0E, 0x0A, 0x48, 0x00, 0x00, 0x19, 0x58, 0x50, 0x00, 0x00, 0x01, errInternal}
	if !bytes.Equal(got, want) || v.Volume() != 10 {
		t.Fatalf("got %x vol %d", got, v.Volume())
	}
}

func TestRegisterNotificationNeedsNotifyCtype(t *testing.T) {
	s := NewServer(&fakeVol{v: 10})
	c := newFakeConn()
	for _, ct := range []byte{0x0, 0x1} { // CONTROL, STATUS
		req := []byte{0x70, 0x11, 0x0E, ct, 0x48, 0x00, 0x00, 0x19, 0x58, 0x31, 0x00, 0x00, 0x05, 0x0D, 0, 0, 0, 0}
		got := s.handle(c, req)
		if len(got) < 4 || got[3] != 0x08 {
			t.Fatalf("ctype %d: got %x", ct, got)
		}
	}
	if len(s.regs) != 0 {
		t.Fatal("registered")
	}
}

func TestSetVolumeNeedsControlCtype(t *testing.T) {
	v := &fakeVol{v: 10}
	s := NewServer(v)
	for _, ct := range []byte{0x1, 0x3} { // STATUS, NOTIFY
		req := []byte{0x70, 0x11, 0x0E, ct, 0x48, 0x00, 0x00, 0x19, 0x58, 0x50, 0x00, 0x00, 0x01, 0x40}
		got := s.handle(newFakeConn(), req)
		want := []byte{0x72, 0x11, 0x0E, 0x08, 0x48, 0x00, 0x00, 0x19, 0x58, 0x50, 0x00, 0x00, 0x01, 0x40}
		if !bytes.Equal(got, want) || v.Volume() != 10 {
			t.Fatalf("ctype %d: got %x vol %d", ct, got, v.Volume())
		}
	}
}

// stuckConn never drains writes: Write blocks until the write deadline.
type stuckConn struct {
	*fakeConn
	mu     sync.Mutex
	wdl    time.Time
	closed chan struct{}
	once   sync.Once
}

func (c *stuckConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.wdl = t
	c.mu.Unlock()
	return nil
}
func (c *stuckConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	dl := c.wdl
	c.mu.Unlock()
	if dl.IsZero() {
		select {} // no deadline: blocks forever
	}
	time.Sleep(time.Until(dl))
	return 0, os.ErrDeadlineExceeded
}
func (c *stuckConn) Close() error {
	c.once.Do(func() { close(c.closed); close(c.in) })
	return nil
}

func TestWriteTimeout(t *testing.T) {
	old := writeTimeout
	writeTimeout = 30 * time.Millisecond
	defer func() { writeTimeout = old }()
	s := NewServer(&fakeVol{})
	c := &stuckConn{fakeConn: newFakeConn(), closed: make(chan struct{})}
	done := make(chan struct{})
	go func() { s.HandleConn(c); close(done) }()
	c.in <- []byte{0x10, 0x11, 0x0E, 0x01, 0xFF, 0x30, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("HandleConn stuck on a peer that never reads")
	}
	<-c.closed
}

func TestPassThroughVolume(t *testing.T) {
	v := &fakeVol{v: 60}
	s := NewServer(v)
	pt := func(ctype, op byte) []byte {
		return s.handle(newFakeConn(), []byte{0x70, 0x11, 0x0E, ctype, 0x48, 0x7C, op, 0x00})
	}
	resp := func(code, op byte) []byte { return []byte{0x72, 0x11, 0x0E, code, 0x48, 0x7C, op, 0x00} }
	for _, c := range []struct {
		ctype, op, code, vol byte
	}{
		{0x0, 0x41, 0x09, 68}, // up press
		{0x0, 0xC1, 0x09, 68}, // up release: no change
		{0x0, 0x42, 0x09, 60}, // down press
		{0x0, 0xC2, 0x09, 60}, // down release
		{0x0, 0x44, 0x08, 60}, // play: not implemented
		{0x1, 0x41, 0x08, 60}, // STATUS ctype
	} {
		if got := pt(c.ctype, c.op); !bytes.Equal(got, resp(c.code, c.op)) || v.Volume() != c.vol {
			t.Fatalf("ctype %d op %#x: got %x vol %d", c.ctype, c.op, got, v.Volume())
		}
	}
	v.SetVolume(125)
	pt(0, 0x41)
	if v.Volume() != 127 {
		t.Fatalf("up clamp: %d", v.Volume())
	}
	v.SetVolume(3)
	pt(0, 0x42)
	if v.Volume() != 0 {
		t.Fatalf("down clamp: %d", v.Volume())
	}
	s.MayControlVolume = func(bt.Addr) bool { return false }
	if got := pt(0, 0x41); !bytes.Equal(got, resp(0x0A, 0x41)) || v.Volume() != 0 {
		t.Fatalf("not owner: got %x vol %d", got, v.Volume())
	}
}

func TestPassThroughVolumeNotifies(t *testing.T) {
	s := NewServer(&fakeVol{v: 60})
	c := newFakeConn()
	done := make(chan struct{})
	go func() { s.HandleConn(c); close(done) }()
	exchange(t, c, []byte{0x60, 0x11, 0x0E, 0x03, 0x48, 0x00, 0x00, 0x19, 0x58, 0x31, 0x00, 0x00, 0x05, 0x0D, 0, 0, 0, 0})
	got := exchange(t, c, []byte{0x30, 0x11, 0x0E, 0x00, 0x48, 0x7C, 0x41, 0x00})
	if want := []byte{0x32, 0x11, 0x0E, 0x09, 0x48, 0x7C, 0x41, 0x00}; !bytes.Equal(got, want) {
		t.Fatalf("accepted: got %x", got)
	}
	got = <-c.out
	want := []byte{0x62, 0x11, 0x0E, 0x0D, 0x48, 0x00, 0x00, 0x19, 0x58, 0x31, 0x00, 0x00, 0x02, 0x0D, 68}
	if !bytes.Equal(got, want) {
		t.Fatalf("changed: got %x want %x", got, want)
	}
	close(c.in)
	<-done
}

func TestGetCapsNeedsStatusCtype(t *testing.T) {
	s := NewServer(&fakeVol{})
	got := s.handle(newFakeConn(), []byte{0x40, 0x11, 0x0E, 0x00, 0x48, 0x00, 0x00, 0x19, 0x58, 0x10, 0x00, 0x00, 0x01, 0x02})
	if len(got) < 4 || got[3] != 0x08 {
		t.Fatalf("got %x", got)
	}
}

func (s *Server) handle(c bt.Conn, p []byte) []byte {
	rsp, _ := s.process(c, p)
	return rsp
}

// blockConn's Write blocks until release is closed.
type blockConn struct {
	*fakeConn
	release chan struct{}
	writes  atomic.Int32
}

func (b *blockConn) Write(p []byte) (int, error) {
	b.writes.Add(1)
	<-b.release
	return len(p), nil
}

func TestNotifyVolumeStuckPeer(t *testing.T) {
	s := NewServer(&fakeVol{})
	stuck := &blockConn{fakeConn: newFakeConn(), release: make(chan struct{})}
	good := newFakeConn()
	s.mu.Lock()
	s.regs[stuck] = 1
	s.regs[good] = 2
	s.mu.Unlock()

	done := make(chan struct{})
	go func() { s.NotifyVolume(0x10); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("NotifyVolume blocked on a stuck peer")
	}
	select {
	case <-good.out:
	case <-time.After(2 * time.Second):
		t.Fatal("healthy peer not notified")
	}
	// Re-register the stuck peer: no second writer while the first is in flight.
	for range 3 {
		s.mu.Lock()
		s.regs[stuck] = 1
		s.mu.Unlock()
		s.NotifyVolume(0x11)
	}
	time.Sleep(50 * time.Millisecond)
	if n := stuck.writes.Load(); n != 1 {
		t.Fatalf("stuck peer writers = %d, want 1", n)
	}
	s.mu.Lock()
	label, kept := s.regs[stuck]
	s.mu.Unlock()
	if !kept || label != 1 {
		t.Fatal("busy peer lost its registration")
	}
	close(stuck.release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.mu.Lock()
		n := len(s.busy)
		s.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("busy flag never cleared")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
