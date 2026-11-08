package mgmt

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"btspeaker/internal/bt"
	"btspeaker/internal/store"
)

func TestPairingAllowed(t *testing.T) {
	now := time.Unix(1000, 0)
	cases := []struct {
		name     string
		deadline time.Time
		always   bool
		want     bool
	}{
		{"closed", time.Time{}, false, false},
		{"open", now.Add(time.Second), false, true},
		{"expired", now.Add(-time.Second), false, false},
		{"at deadline", now, false, false},
		{"always, closed", time.Time{}, true, true},
	}
	for _, c := range cases {
		if got := PairingAllowed(now, c.deadline, c.always); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}

func TestEncode(t *testing.T) {
	a := bt.Addr{0x11, 0x22, 0x33, 0x44, 0x55, 0x66}
	if got, want := EncodePacket(OpUserConfirmReply, 1, addrParam(a)),
		[]byte{0x1c, 0, 1, 0, 7, 0, 0x66, 0x55, 0x44, 0x33, 0x22, 0x11, 0}; !bytes.Equal(got, want) {
		t.Errorf("packet %x", got)
	}
	if got, want := EncodeSetDiscoverable(1, 120), []byte{1, 120, 0}; !bytes.Equal(got, want) {
		t.Errorf("discoverable %x", got)
	}
	want := []byte{0xFB, 0x34, 0x9B, 0x5F, 0x80, 0, 0, 0x80, 0, 0x10, 0, 0, 0x0B, 0x11, 0, 0, 0x24}
	if got := EncodeAddUUID(0x110B, 0x24); !bytes.Equal(got, want) {
		t.Errorf("uuid %x", got)
	}
	k := Key{Addr: a, Type: 8, PinLen: 0}
	k.Value[0] = 0xAA
	b := EncodeLoadLinkKeys([]Key{k})
	if len(b) != 3+25 || b[0] != 0 || b[1] != 1 || b[3] != 0x66 || b[10] != 8 || b[11] != 0xAA {
		t.Errorf("load keys %x", b)
	}
	if n := EncodeSetLocalName("Hi"); len(n) != 260 || n[0] != 'H' || n[1] != 'i' {
		t.Error("local name")
	}
	pin := EncodePINReply(a, "0000")
	if len(pin) != 7+1+16 || pin[7] != 4 || pin[8] != '0' {
		t.Errorf("pin %x", pin)
	}
}

func TestDecode(t *testing.T) {
	// Device Connected with EIR: flags(4), eir_len(2), eir: shortened name "ab" + complete name "Phone".
	eir := []byte{3, 0x08, 'a', 'b', 6, 0x09, 'P', 'h', 'o', 'n', 'e'}
	p := append([]byte{0x66, 0x55, 0x44, 0x33, 0x22, 0x11, 0, 0, 0, 0, 0, byte(len(eir)), 0}, eir...)
	d, err := DecodeDeviceConnected(p)
	if err != nil || d.Name != "Phone" || d.Addr.String() != "11:22:33:44:55:66" {
		t.Fatalf("%+v %v", d, err)
	}
	if EIRName([]byte{3, 0x08, 'a', 'b'}) != "ab" || EIRName([]byte{5, 0x09, 'a'}) != "" {
		t.Error("EIRName")
	}
	if _, err := DecodeDeviceConnected(p[:len(p)-1]); err == nil {
		t.Error("want truncated error")
	}

	nk := []byte{1, 0x66, 0x55, 0x44, 0x33, 0x22, 0x11, 0, 8}
	nk = append(nk, bytes.Repeat([]byte{7}, 16)...)
	nk = append(nk, 0)
	k, err := DecodeNewLinkKey(nk)
	if err != nil || !k.StoreHint || k.Key.Type != 8 || k.Key.Value[15] != 7 || k.Key.Addr[0] != 0x11 {
		t.Fatalf("%+v %v", k, err)
	}

	nk[8] = 0xFF // the decoder does not judge key types (Load Link Keys / init do)
	if k, err := DecodeNewLinkKey(nk); err != nil || k.Key.Type != 0xFF {
		t.Fatalf("%+v %v", k, err)
	}

	info := make([]byte, infoLen)
	copy(info, []byte{0x66, 0x55, 0x44, 0x33, 0x22, 0x11})
	binary.LittleEndian.PutUint32(info[13:], SettingPowered|SettingSSP)
	copy(info[20:], "hello")
	i, err := DecodeInfo(info)
	if err != nil || i.Name != "hello" || i.Current != SettingPowered|SettingSSP || i.Addr[5] != 0x66 {
		t.Fatalf("%+v %v", i, err)
	}

	idx, err := DecodeIndexList([]byte{2, 0, 0, 0, 1, 0})
	if err != nil || len(idx) != 2 || idx[1] != 1 {
		t.Fatal(idx, err)
	}
	cs, err := DecodeConnections([]byte{1, 0, 0x66, 0x55, 0x44, 0x33, 0x22, 0x11, 0})
	if err != nil || len(cs) != 1 || cs[0][0] != 0x11 {
		t.Fatal(cs, err)
	}
	h, params, err := DecodePacket([]byte{1, 0, 0, 0, 2, 0, 9, 9})
	if err != nil || h.Code != 1 || len(params) != 2 {
		t.Fatal(h, err)
	}
	if _, _, err := DecodePacket([]byte{1, 0, 0, 0, 3, 0, 9}); err == nil {
		t.Error("want length error")
	}
}

// fakeController answers mgmt commands over a net.Pipe and records them.
type fakeController struct {
	t           *testing.T
	conn        net.Conn
	mu          sync.Mutex
	cmds        []Header
	last        map[uint16][]byte
	fail        map[uint16]bool // ops answered with a failure status
	once        map[uint16]int  // ops answered with a failure status this many more times
	status      map[uint16]byte // ops answered with this status (overrides fail)
	onOp        func(op uint16) // called (unlocked) before answering, if set
	current     uint32          // extra current settings reported by Read Info
	connectable bool            // connectable flag; the kernel rejects Set Discoverable without it
}

// paramLens are the fixed parameter lengths of the commands we send (mgmt-api).
var paramLens = map[uint16]int{
	OpReadIndexList: 0, OpReadInfo: 0, OpGetConnections: 0,
	OpSetPowered: 1, OpSetDiscoverable: 3, OpSetConnectable: 1, OpSetBondable: 1,
	OpSetSSP: 1, OpSetLE: 1, OpSetBREDR: 1, OpSetSecureConn: 1,
	OpSetDevClass: 2, OpSetLocalName: 260, OpAddUUID: 17, OpRemoveUUID: 16,
	OpPINCodeReply: 24, OpPINCodeNegReply: 7, OpSetIOCapability: 1,
	OpUnpairDevice: 8, OpUserConfirmReply: 7, OpUserConfirmNeg: 7, OpDisconnect: 7,
}

func newFake(t *testing.T) (*fakeController, net.Conn) {
	a, b := net.Pipe()
	f := &fakeController{t: t, conn: b, last: map[uint16][]byte{}}
	go f.serve()
	t.Cleanup(func() { a.Close(); b.Close() })
	return f, a
}

func (f *fakeController) serve() {
	buf := make([]byte, 4096)
	for {
		n, err := f.conn.Read(buf)
		if err != nil {
			return
		}
		h, p, err := DecodePacket(buf[:n])
		if err != nil {
			continue
		}
		if want, ok := paramLens[h.Code]; ok && len(p) != want {
			f.t.Errorf("opcode 0x%04x: %d param bytes, want %d", h.Code, len(p), want)
		}
		if h.Code == OpLoadLinkKeys && (len(p) < 3 || len(p) != 3+25*int(binary.LittleEndian.Uint16(p[1:]))) {
			f.t.Errorf("Load Link Keys: bad length %d", len(p))
		}
		f.mu.Lock()
		f.cmds = append(f.cmds, h)
		f.last[h.Code] = append([]byte(nil), p...)
		status := byte(0)
		if f.fail[h.Code] {
			status = 0x0c // Rejected
		}
		if f.once[h.Code] > 0 {
			f.once[h.Code]--
			status = 0x0c
		}
		if st, ok := f.status[h.Code]; ok {
			status = st
		}
		switch h.Code {
		case OpSetConnectable:
			if status == 0 && len(p) > 0 {
				f.connectable = p[0] != 0
			}
		case OpSetDiscoverable:
			if !f.connectable {
				status = 0x0b // Rejected, as the kernel does when not connectable
			}
		}
		hook := f.onOp
		f.mu.Unlock()
		if hook != nil {
			hook(h.Code)
		}
		var data []byte
		switch h.Code {
		case OpReadIndexList:
			data = []byte{1, 0, 0, 0}
		case OpReadInfo:
			data = make([]byte, infoLen)
			copy(data, []byte{0x66, 0x55, 0x44, 0x33, 0x22, 0x11})
			binary.LittleEndian.PutUint32(data[13:], SettingPowered|f.current)
		case OpGetConnections:
			data = []byte{0, 0}
		}
		rp := append([]byte{byte(h.Code), byte(h.Code >> 8), status}, data...)
		f.conn.Write(EncodePacket(EvCmdComplete, h.Index, rp))
	}
}

func (f *fakeController) event(code, index uint16, params []byte) {
	f.conn.Write(EncodePacket(code, index, params))
}

func (f *fakeController) count(op uint16) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, h := range f.cmds {
		if h.Code == op {
			n++
		}
	}
	return n
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func TestControllerPairingFlow(t *testing.T) {
	fake, conn := newFake(t)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := New(Config{Name: "Test", Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Open: func() (io.ReadWriteCloser, error) { return conn, nil }})
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown()
	waitFor(t, "ready", func() bool { return c.Status().Present && fake.count(OpGetConnections) == 1 })
	if s := c.Status(); s.Addr.String() != "11:22:33:44:55:66" || !s.Powered {
		t.Fatalf("status %+v", s)
	}
	if fake.count(OpAddUUID) != 3 || fake.count(OpLoadLinkKeys) != 1 {
		t.Fatalf("init sequence incomplete: %+v", fake.cmds)
	}

	phone := bt.Addr{0xA0, 1, 2, 3, 4, 5}
	req := append(addrParam(phone), 0, 1, 0, 0, 0)

	// Window closed: refuse.
	fake.event(EvUserConfirmReq, 0, req)
	waitFor(t, "neg reply", func() bool { return fake.count(OpUserConfirmNeg) == 1 })
	if fake.count(OpUserConfirmReply) != 0 {
		t.Fatal("must not accept with window closed")
	}

	// Open the window: accept.
	if err := c.OpenPairing(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	if r := c.Status().PairingRemaining; r <= 0 || r > 30*time.Second {
		t.Fatalf("remaining %v", r)
	}
	fake.event(EvUserConfirmReq, 0, req)
	waitFor(t, "reply", func() bool { return fake.count(OpUserConfirmReply) == 1 })

	// Name arrives via connect, key via New Link Key; window then closes.
	eir := []byte{6, 0x09, 'P', 'h', 'o', 'n', 'e'}
	dc := append(addrParam(phone), 0, 0, 0, 0, byte(len(eir)), 0)
	fake.event(EvDeviceConnected, 0, append(dc, eir...))
	waitFor(t, "connected", func() bool { return rawConns(c) == 1 })
	if len(c.Status().Connected) != 0 {
		t.Fatal("unadmitted peer listed as connected")
	}
	nk := append([]byte{1}, addrParam(phone)...)
	nk = append(nk, 8)
	nk = append(nk, bytes.Repeat([]byte{9}, 16)...)
	nk = append(nk, 0)
	fake.event(EvNewLinkKey, 0, nk)
	waitFor(t, "key stored", func() bool { return len(st.Devices()) == 1 })
	waitFor(t, "listed connected", func() bool { return len(c.Status().Connected) == 1 })
	d := st.Devices()[0]
	if d.Name != "Phone" || d.LinkKey == nil || d.LinkKey.Type != 8 {
		t.Fatalf("stored %+v", d)
	}
	waitFor(t, "window closed", func() bool { return c.Status().PairingRemaining == 0 && fake.count(OpSetDiscoverable) >= 3 })

	// Legacy PIN with closed window is refused.
	fake.event(EvPINCodeRequest, 0, append(addrParam(phone), 0))
	waitFor(t, "pin neg", func() bool { return fake.count(OpPINCodeNegReply) == 1 })

	// Adapter unplug and return triggers re-init.
	before := fake.count(OpLoadLinkKeys)
	fake.event(EvIndexRemoved, 0, nil)
	waitFor(t, "absent", func() bool { return !c.Status().Present })
	fake.event(EvIndexAdded, 0, nil)
	waitFor(t, "reinit", func() bool { return c.Status().Present && fake.count(OpLoadLinkKeys) == before+1 })
	fake.mu.Lock()
	keys := fake.last[OpLoadLinkKeys]
	fake.mu.Unlock()
	if len(keys) != 3+25 {
		t.Fatalf("stored key not reloaded: %x", keys)
	}
}

func TestClientStatusError(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go func() {
		buf := make([]byte, 100)
		n, _ := b.Read(buf)
		h, _, _ := DecodePacket(buf[:n])
		b.Write(EncodePacket(EvCmdStatus, h.Index, []byte{byte(h.Code), byte(h.Code >> 8), 0x0d}))
	}()
	c := NewClient(a, slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, err := c.Do(0, OpSetPowered, u8(true))
	se, ok := err.(*StatusError)
	if !ok || se.Status != 0x0d {
		t.Fatalf("err %v", err)
	}
}

type closeCounter struct {
	net.Conn
	mu sync.Mutex
	n  int
}

func (c *closeCounter) Close() error {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return c.Conn.Close()
}

func (c *closeCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func TestReadLoopClosesFD(t *testing.T) {
	a, b := net.Pipe()
	cc := &closeCounter{Conn: a}
	cl := NewClient(cc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	b.Close() // read error ends the loop
	<-cl.Done()
	waitFor(t, "close", func() bool { return cc.count() >= 1 })
	cl.Close()
	cl.Close()
	if n := cc.count(); n != 1 {
		t.Fatalf("closed %d times", n)
	}
}

func TestClientCriticalEventNotDropped(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	cl := NewClient(a, slog.New(slog.NewTextHandler(io.Discard, nil)))
	const n = 400 // more than the 256-event queue
	go func() {
		for i := 0; i < n; i++ {
			b.Write(EncodePacket(EvNewLinkKey, 0, []byte{byte(i)}))
		}
	}()
	for i := 0; i < n; i++ {
		select {
		case ev := <-cl.Events():
			if ev.Params[0] != byte(i) {
				t.Fatalf("event %d out of order/lost", i)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timeout at %d", i)
		}
		if i == 0 {
			time.Sleep(100 * time.Millisecond) // let the queue fill
		}
	}
	cl.Close()
}

func TestRefuseLegacyPIN(t *testing.T) {
	fake, conn := newFake(t)
	st, _ := store.Open(t.TempDir())
	c := New(Config{Name: "Test", Store: st, AlwaysPairable: true, RefuseLegacyPIN: true,
		Log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Open: func() (io.ReadWriteCloser, error) { return conn, nil }})
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown()
	waitFor(t, "ready", func() bool { return c.Status().Present && fake.count(OpGetConnections) == 1 })
	fake.event(EvPINCodeRequest, 0, append(addrParam(bt.Addr{0xA0, 1, 2, 3, 4, 5}), 0))
	waitFor(t, "pin neg", func() bool { return fake.count(OpPINCodeNegReply) == 1 })
	if fake.count(OpPINCodeReply) != 0 {
		t.Fatal("answered legacy PIN despite RefuseLegacyPIN")
	}
}

// A consumer that calls Do without draining Events must still get its reply
// while more critical events are queued than the old 256-event channel held.
func TestClientDoWithFullEventQueue(t *testing.T) {
	f, conn := newFake(t)
	cl := NewClient(conn, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer cl.Close()
	for i := 0; i < 300; i++ {
		f.conn.Write(EncodePacket(EvNewLinkKey, 0, []byte{byte(i)}))
	}
	done := make(chan error, 1)
	go func() { _, err := cl.Do(0xFFFF, OpReadIndexList, nil); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Do stalled behind the event queue")
	}
	for i := 0; i < 300; i++ {
		if ev := <-cl.Events(); ev.Params[0] != byte(i) {
			t.Fatalf("event %d out of order/lost", i)
		}
	}
}

func startFake(t *testing.T, always bool) (*fakeController, *Controller, *store.Store) {
	t.Helper()
	return startFakeDir(t, always, t.TempDir())
}

func startFakeDir(t *testing.T, always bool, dir string) (*fakeController, *Controller, *store.Store) {
	t.Helper()
	fake, conn := newFake(t)
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := New(Config{Name: "Test", Store: st, AlwaysPairable: always,
		Log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Open: func() (io.ReadWriteCloser, error) { return conn, nil }})
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Shutdown)
	waitFor(t, "ready", func() bool { return c.Status().Present && fake.count(OpGetConnections) == 1 })
	return fake, c, st
}

func (f *fakeController) lastParams(op uint16) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last[op]
}

// seedKey stores a key for addr in dir before the controller starts, so
// initAdapter loads it.
func seedKey(t *testing.T, dir string, addr bt.Addr, typ uint8) {
	t.Helper()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutKey(addr, store.NewLinkKey(typ, [16]byte{1}, 0), time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestAdmit(t *testing.T) {
	known, stranger := bt.Addr{0xA0, 1}, bt.Addr{0xB0, 2}
	dir := t.TempDir()
	seedKey(t, dir, known, 8)
	_, c, _ := startFakeDir(t, false, dir)
	if !c.Admit(known) || c.Admit(stranger) {
		t.Fatal("window closed: want only the known peer")
	}
	if err := c.OpenPairing(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	if !c.Admit(stranger) {
		t.Fatal("window open: want stranger admitted")
	}
	if err := c.ClosePairing(); err != nil {
		t.Fatal(err)
	}
	if c.Admit(stranger) || !c.Admit(known) {
		t.Fatal("window closed again")
	}

	_, ca, _ := startFake(t, true)
	if !ca.Admit(stranger) {
		t.Fatal("always pairable: want admitted")
	}
}

func TestDebugKeyRefusedInWindow(t *testing.T) {
	peer := bt.Addr{0xB0, 5}
	fake, c, st := startFake(t, false)
	if err := c.OpenPairing(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	ev := newLinkKeyEvent(peer, true)
	ev[8] = debugLinkKeyType
	fake.event(EvNewLinkKey, 0, ev)
	waitFor(t, "unpair", func() bool { return fake.count(OpUnpairDevice) == 1 })
	if st.Has(peer) || c.isLoaded(peer) {
		t.Fatal("debug link key stored or trusted")
	}
}

func TestActionsFailFastDuringInit(t *testing.T) {
	_, c, _ := startFake(t, false)
	c.mu.Lock()
	c.initialising = true
	c.mu.Unlock()
	c.winMu.Lock() // as initAdapter does: the actions must not wait on it
	defer c.winMu.Unlock()
	if c.Status().Present {
		t.Error("Status: present while initialising")
	}
	for name, err := range map[string]error{
		"open":   c.OpenPairing(time.Second),
		"close":  c.ClosePairing(),
		"forget": c.Forget(bt.Addr{1}),
		"name":   c.SetName("x"),
	} {
		if !errors.Is(err, ErrNoAdapter) {
			t.Errorf("%s: err = %v, want ErrNoAdapter", name, err)
		}
	}
	c.mu.Lock()
	c.initialising = false
	c.mu.Unlock()
}

func TestBondableFollowsWindow(t *testing.T) {
	fake, c, _ := startFake(t, false)
	if p := fake.lastParams(OpSetBondable); !bytes.Equal(p, []byte{0}) {
		t.Fatalf("init bondable %x", p)
	}
	if err := c.OpenPairing(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	if p := fake.lastParams(OpSetBondable); !bytes.Equal(p, []byte{1}) {
		t.Fatalf("open bondable %x", p)
	}
	if err := c.ClosePairing(); err != nil {
		t.Fatal(err)
	}
	if p := fake.lastParams(OpSetBondable); !bytes.Equal(p, []byte{0}) {
		t.Fatalf("close bondable %x", p)
	}

	fa, _, _ := startFake(t, true)
	if p := fa.lastParams(OpSetBondable); !bytes.Equal(p, []byte{1}) {
		t.Fatalf("always pairable init bondable %x", p)
	}
}

func TestNewLinkKeyWindowClosed(t *testing.T) {
	fake, c, st := startFake(t, false)
	stranger := bt.Addr{0xB0, 2, 3, 4, 5, 6}
	nk := append([]byte{1}, addrParam(stranger)...)
	nk = append(nk, 8)
	nk = append(nk, bytes.Repeat([]byte{9}, 16)...)
	nk = append(nk, 0)
	fake.event(EvNewLinkKey, 0, nk)
	waitFor(t, "unpair", func() bool { return fake.count(OpUnpairDevice) == 1 })
	if p := fake.lastParams(OpUnpairDevice); !bytes.Equal(p, append(addrParam(stranger), 1)) {
		t.Fatalf("unpair params %x", p)
	}
	if st.Has(stranger) || len(st.Devices()) != 0 {
		t.Fatal("key from a stranger stored with the window closed")
	}
	if e := c.Status().LastError; !strings.Contains(e, "after the window closed") {
		t.Fatalf("no UI reason: %q", e)
	}
}

func (f *fakeController) setFail(op uint16, on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail == nil {
		f.fail = map[uint16]bool{}
	}
	f.fail[op] = on
}

func newLinkKeyEvent(addr bt.Addr, storeHint bool) []byte {
	hint := byte(0)
	if storeHint {
		hint = 1
	}
	nk := append([]byte{hint}, addrParam(addr)...)
	nk = append(nk, 8)
	nk = append(nk, bytes.Repeat([]byte{9}, 16)...)
	return append(nk, 0)
}

func pairingOpen(c *Controller) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.deadline.IsZero()
}

func TestNoStoreKeyClosesWindow(t *testing.T) {
	fake, c, st := startFake(t, false)
	if err := c.OpenPairing(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	addr := bt.Addr{0xC0, 1, 2, 3, 4, 5}
	fake.event(EvNewLinkKey, 0, newLinkKeyEvent(addr, false))
	waitFor(t, "window closed", func() bool { return !pairingOpen(c) })
	if st.Has(addr) {
		t.Fatal("no-bonding key stored")
	}
}

func TestKeySaveFailureClosesWindow(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	fake, c, _ := startFakeDir(t, false, dir)
	if err := c.OpenPairing(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	fake.event(EvNewLinkKey, 0, newLinkKeyEvent(bt.Addr{0xC0, 1, 2, 3, 4, 5}, true))
	waitFor(t, "window closed", func() bool {
		return bytes.Equal(fake.lastParams(OpSetBondable), []byte{0}) &&
			len(fake.lastParams(OpSetDiscoverable)) > 0 && fake.lastParams(OpSetDiscoverable)[0] == 0
	})
	if e := c.Status().LastError; !strings.Contains(e, "saving link key failed") {
		t.Fatalf("no UI error: %q", e)
	}
}

func TestKnownRekeyKeepsWindowOpen(t *testing.T) {
	known := bt.Addr{0xA0, 1, 2, 3, 4, 5}
	dir := t.TempDir()
	seedKey(t, dir, known, 8)
	fake, c, st := startFakeDir(t, false, dir)
	if err := c.OpenPairing(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	fake.event(EvNewLinkKey, 0, newLinkKeyEvent(known, true))
	waitFor(t, "key updated", func() bool {
		d := st.Devices()
		return len(d) == 1 && d[0].LinkKey != nil && d[0].LinkKey.Value == strings.Repeat("09", 16)
	})
	time.Sleep(50 * time.Millisecond) // a close would be async
	if !pairingOpen(c) {
		t.Fatal("re-key of a known device closed the window")
	}
}

func TestOpenPairingRevertsBondable(t *testing.T) {
	fake, c, _ := startFake(t, false)
	fake.setFail(OpSetDiscoverable, true)
	if err := c.OpenPairing(30 * time.Second); err == nil {
		t.Fatal("want error")
	}
	if p := fake.lastParams(OpSetBondable); !bytes.Equal(p, []byte{0}) {
		t.Fatalf("bondable left %x", p)
	}
	if pairingOpen(c) {
		t.Fatal("deadline set after failure")
	}
}

func TestEncodeSetLocalNameRuneBoundary(t *testing.T) {
	name := strings.Repeat("x", 247) + "é" // 249 bytes, é straddles the cut
	b := EncodeSetLocalName(name)
	if len(b) != 260 || !utf8.Valid(bytes.TrimRight(b[:249], "\x00")) || b[247] != 0 {
		t.Fatalf("bad truncation: %q", b[240:249])
	}
}

func TestPendingNameCommittedOnlyWhenAuthenticated(t *testing.T) {
	fake, conn := newFake(t)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := New(Config{Name: "Test", Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Open: func() (io.ReadWriteCloser, error) { return conn, nil }})
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown()
	waitFor(t, "ready", func() bool { return c.Status().Present && fake.count(OpGetConnections) == 1 })

	phone := bt.Addr{0xA0, 1, 2, 3, 4, 5}
	if err := st.PutKeyNamed(phone, store.NewLinkKey(8, [16]byte{1}, 0), "Phone", time.Now()); err != nil {
		t.Fatal(err)
	}
	connect := func(name string) {
		eir := append([]byte{byte(len(name) + 1), 0x09}, name...)
		dc := append(addrParam(phone), 0, 0, 0, 0, byte(len(eir)), 0)
		fake.event(EvDeviceConnected, 0, append(dc, eir...))
	}
	connect("Spoof")
	waitFor(t, "connected", func() bool { return rawConns(c) == 1 })
	if n := st.Devices()[0].Name; n != "Phone" {
		t.Fatalf("name rewritten before auth: %q", n)
	}
	// Disconnect drops the pending name.
	fake.event(EvDeviceDisconn, 0, append(addrParam(phone), 0))
	waitFor(t, "disconnected", func() bool { return rawConns(c) == 0 })
	c.Authenticated(phone)
	if n := st.Devices()[0].Name; n != "Phone" {
		t.Fatalf("stale pending name committed: %q", n)
	}
	connect("Real")
	waitFor(t, "connected", func() bool { return rawConns(c) == 1 })
	c.Authenticated(phone)
	waitFor(t, "name committed", func() bool { return st.Devices()[0].Name == "Real" })
}

func TestDeviceCapEvictsOldest(t *testing.T) {
	fake, c, st := startFake(t, true)
	_ = c
	for i := 1; i <= store.MaxDevices; i++ {
		a := bt.Addr{0xA0, byte(i)}
		if err := st.PutKey(a, store.NewLinkKey(8, [16]byte{1}, 0), time.Unix(int64(1000+i), 0)); err != nil {
			t.Fatal(err)
		}
	}
	newcomer := bt.Addr{0xB0, 1}
	fake.event(EvNewLinkKey, 0, newLinkKeyEvent(newcomer, true))
	waitFor(t, "unpair oldest", func() bool { return fake.count(OpUnpairDevice) == 1 })
	oldest := bt.Addr{0xA0, 1}
	if p := fake.lastParams(OpUnpairDevice); !bytes.Equal(p, append(addrParam(oldest), 1)) {
		t.Fatalf("unpair params %x", p)
	}
	waitFor(t, "evicted", func() bool { return !st.Has(oldest) })
	if !st.Has(newcomer) || len(st.Devices()) != store.MaxDevices {
		t.Fatalf("devices = %d, newcomer stored = %v", len(st.Devices()), st.Has(newcomer))
	}
}

func TestInitSkipsInvalidKeyType(t *testing.T) {
	fake, conn := newFake(t)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	good, bad := bt.Addr{0xA0, 1}, bt.Addr{0xA0, 2}
	st.PutKey(good, store.NewLinkKey(4, [16]byte{1}, 0), time.Now())
	st.PutKey(bad, store.NewLinkKey(9, [16]byte{2}, 0), time.Now())
	c := New(Config{Name: "Test", Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Open: func() (io.ReadWriteCloser, error) { return conn, nil }})
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Shutdown)
	waitFor(t, "ready", func() bool { return c.Status().Present && fake.count(OpGetConnections) == 1 })
	p := fake.lastParams(OpLoadLinkKeys)
	if want := EncodeLoadLinkKeys([]Key{{Addr: good, Type: 4, Value: [16]byte{1}}}); !bytes.Equal(p, want) {
		t.Fatalf("load link keys %x, want %x", p, want)
	}
	// The kernel rejects fixed-size commands of the wrong length (write returns 0).
	if p := fake.lastParams(OpRemoveUUID); len(p) != 16 {
		t.Fatalf("remove uuid params %d bytes, want 16", len(p))
	}
}

func TestUnloadedKnownDeviceNotAdmitted(t *testing.T) {
	bad, debug, failed := bt.Addr{0xA0, 2}, bt.Addr{0xA0, 4}, bt.Addr{0xA0, 3}
	dir := t.TempDir()
	seedKey(t, dir, bad, 9)   // invalid type: skipped at init
	seedKey(t, dir, debug, 3) // debug combination: skipped at init
	_, c, st := startFakeDir(t, false, dir)
	if !st.Has(bad) || c.Admit(bad) {
		t.Fatal("stored device with an unloaded key admitted with the window closed")
	}
	if !st.Has(debug) || c.Admit(debug) {
		t.Fatal("stored device with a debug key admitted with the window closed")
	}

	// Load Link Keys fails (batch, then the single key): none loads, so no stored device is admitted.
	dir2 := t.TempDir()
	seedKey(t, dir2, failed, 4)
	fake, conn := newFake(t)
	fake.once = map[uint16]int{OpLoadLinkKeys: 2}
	st2, err := store.Open(dir2)
	if err != nil {
		t.Fatal(err)
	}
	c2 := New(Config{Name: "Test", Store: st2, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Open: func() (io.ReadWriteCloser, error) { return conn, nil }})
	if err := c2.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c2.Shutdown)
	waitFor(t, "ready", func() bool { return c2.Status().Present && fake.count(OpGetConnections) == 1 })
	if c2.Admit(failed) {
		t.Fatal("device admitted after Load Link Keys failed")
	}
	// A key re-established with the window closed is revoked, not stored.
	fake.event(EvNewLinkKey, 0, newLinkKeyEvent(failed, true))
	waitFor(t, "unpair", func() bool { return fake.count(OpUnpairDevice) == 1 })
	if c2.Admit(failed) {
		t.Fatal("admitted after revoked re-key")
	}
}

func rawConns(c *Controller) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.conns)
}

func TestNewKeyAdmittedOnWindowClose(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	// The save fails (read-only dir), but the kernel holds the key: the
	// device must be admitted as soon as the window closed, not after the save.
	dir := t.TempDir()
	fake, c, _ := startFakeDir(t, false, dir)
	if err := c.OpenPairing(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	addr := bt.Addr{0xC0, 1, 2, 3, 4, 5}
	fake.event(EvNewLinkKey, 0, newLinkKeyEvent(addr, true))
	waitFor(t, "window closed", func() bool { return !pairingOpen(c) })
	if !c.Admit(addr) {
		t.Fatal("new device not admitted right after the window closed")
	}
	// Also without a stored key (no-bonding pairing).
	c2fake, c2, _ := startFake(t, false)
	if err := c2.OpenPairing(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	c2fake.event(EvNewLinkKey, 0, newLinkKeyEvent(addr, false))
	waitFor(t, "window closed", func() bool { return !pairingOpen(c2) })
	if !c2.Admit(addr) {
		t.Fatal("no-store device not admitted")
	}
}

func TestForgetUnpairFailureSurfaces(t *testing.T) {
	known := bt.Addr{0xA0, 1}
	dir := t.TempDir()
	seedKey(t, dir, known, 8)
	fake, c, st := startFakeDir(t, false, dir)
	fake.setFail(OpUnpairDevice, true)
	if err := c.Forget(known); err == nil {
		t.Fatal("unpair failure hidden")
	}
	if c.Admit(known) {
		t.Fatal("still admitted after failed forget")
	}
	if !st.Has(known) {
		t.Fatal("store entry dropped although the kernel kept the key")
	}
	fake.setFail(OpUnpairDevice, false)
	if err := c.Forget(known); err != nil || st.Has(known) {
		t.Fatalf("retry: err=%v has=%v", err, st.Has(known))
	}
}

func TestInitSkipsForgottenDevice(t *testing.T) {
	gone := bt.Addr{0xA0, 7}
	dir := t.TempDir()
	seedKey(t, dir, gone, 8)
	fake, conn := newFake(t)
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Forget lands between the snapshot and the load: emulate it from the
	// fake's Load Link Keys handling by removing the entry first.
	c := New(Config{Name: "Test", Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Open: func() (io.ReadWriteCloser, error) { return conn, nil }})
	fake.onOp = func(op uint16) {
		if op == OpLoadLinkKeys {
			st.Remove(gone)
		}
	}
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Shutdown)
	waitFor(t, "ready", func() bool { return c.Status().Present && fake.count(OpGetConnections) == 1 })
	if c.isLoaded(gone) {
		t.Fatal("forgotten device admitted")
	}
	if fake.count(OpUnpairDevice) != 1 {
		t.Fatal("forgotten device not unpaired")
	}
}

func TestForgetSerialisedWithReinit(t *testing.T) {
	known := bt.Addr{0xA0, 9}
	dir := t.TempDir()
	seedKey(t, dir, known, 8)
	fake, c, st := startFakeDir(t, false, dir)
	var once sync.Once
	fake.mu.Lock()
	fake.onOp = func(op uint16) {
		if op != OpUnpairDevice {
			return
		}
		// Hotplug re-init while Forget is in flight.
		once.Do(func() {
			go func() {
				fake.event(EvIndexRemoved, 0, nil)
				fake.event(EvIndexAdded, 0, nil)
			}()
			time.Sleep(50 * time.Millisecond)
		})
	}
	fake.mu.Unlock()
	if err := c.Forget(known); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "re-init", func() bool { return c.Status().Present && fake.count(OpGetConnections) == 2 })
	if c.Admit(known) || st.Has(known) {
		t.Fatalf("forgotten device back: admit=%v store=%v", c.Admit(known), st.Has(known))
	}
}

func TestShutdownAlwaysPairableClearsBondable(t *testing.T) {
	fake, c, _ := startFake(t, true)
	c.Shutdown()
	if p := fake.lastParams(OpSetBondable); len(p) != 1 || p[0] != 0 {
		t.Fatalf("bondable left %v", p)
	}
}

func TestClientDoRejectsHugeParams(t *testing.T) {
	_, c, _ := startFake(t, false)
	if _, err := c.client().Do(0, OpLoadLinkKeys, make([]byte, 0x10000)); err == nil {
		t.Fatal("oversized command accepted")
	}
}

func TestForgetAdapterAbsent(t *testing.T) {
	known := bt.Addr{0xA0, 1}
	dir := t.TempDir()
	seedKey(t, dir, known, 8)
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := New(Config{Name: "Test", Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err := c.Forget(known); err != nil || st.Has(known) {
		t.Fatalf("err=%v has=%v", err, st.Has(known))
	}
}

func TestOpenPairingAfterShutdown(t *testing.T) {
	fake, c, _ := startFake(t, false)
	c.Shutdown()
	n := fake.count(OpSetDiscoverable)
	if err := c.OpenPairing(30 * time.Second); err == nil {
		t.Fatal("OpenPairing succeeded after Shutdown")
	}
	if fake.count(OpSetDiscoverable) != n {
		t.Fatal("discoverable reopened after Shutdown")
	}
}

func TestInitSkipsSettingsAlreadyOn(t *testing.T) {
	for _, on := range []bool{false, true} {
		fake, conn := newFake(t)
		if on {
			fake.mu.Lock()
			fake.current = SettingBREDR | SettingSecureConn
			fake.mu.Unlock()
		}
		st, err := store.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		c := New(Config{Name: "Test", Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
			Open: func() (io.ReadWriteCloser, error) { return conn, nil }})
		if err := c.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "ready", func() bool { return c.Status().Present && fake.count(OpGetConnections) == 1 })
		c.Shutdown()
		want := 1
		if on {
			want = 0
		}
		if n, m := fake.count(OpSetBREDR), fake.count(OpSetSecureConn); n != want || m != want {
			t.Fatalf("already on=%v: br/edr sent %d, sc sent %d, want %d", on, n, m, want)
		}
	}
}

func TestNoBondingKeyWindowClosedRefused(t *testing.T) {
	known := bt.Addr{0xA0, 1, 2, 3, 4, 5}
	dir := t.TempDir()
	seedKey(t, dir, known, 8)
	fake, c, _ := startFakeDir(t, false, dir)
	if !c.Admit(known) {
		t.Fatal("known device not admitted")
	}
	fake.event(EvNewLinkKey, 0, newLinkKeyEvent(known, false))
	waitFor(t, "unpair", func() bool { return fake.count(OpUnpairDevice) == 1 })
	// The real key is restored in the kernel and the device stays admitted.
	waitFor(t, "reload", func() bool { return fake.count(OpLoadLinkKeys) == 2 })
	if !c.Admit(known) {
		t.Fatal("bonded device refused after a refused no-bonding key")
	}
	if p := fake.lastParams(OpLoadLinkKeys); len(p) != 3+25 {
		t.Fatalf("reload params %d bytes, want one key", len(p))
	}
}

func TestNoBondingKeyAdmittedForSessionOnly(t *testing.T) {
	fake, c, st := startFake(t, false)
	if err := c.OpenPairing(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	addr := bt.Addr{0xC0, 1, 2, 3, 4, 5}
	fake.event(EvNewLinkKey, 0, newLinkKeyEvent(addr, false))
	waitFor(t, "window closed", func() bool { return !pairingOpen(c) })
	if !c.Admit(addr) || st.Has(addr) {
		t.Fatal("want admitted for the session, nothing stored")
	}
	fake.event(EvDeviceDisconn, 0, append(addrParam(addr), 0))
	waitFor(t, "not admitted", func() bool { return !c.Admit(addr) })
}

func TestStoredRekeyWindowClosedAccepted(t *testing.T) {
	known := bt.Addr{0xA0, 1, 2, 3, 4, 5}
	dir := t.TempDir()
	seedKey(t, dir, known, 8)
	fake, c, st := startFakeDir(t, false, dir)
	fake.event(EvNewLinkKey, 0, newLinkKeyEvent(known, true))
	waitFor(t, "key updated", func() bool {
		d := st.Devices()
		return len(d) == 1 && d[0].LinkKey != nil && d[0].LinkKey.Value == strings.Repeat("09", 16)
	})
	if fake.count(OpUnpairDevice) != 0 || !c.Admit(known) {
		t.Fatal("re-key with StoreHint refused")
	}
}

func TestSecondOpenPairingFailureClosesWindow(t *testing.T) {
	fake, c, _ := startFake(t, false)
	if err := c.OpenPairing(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	fake.setFail(OpSetDiscoverable, true)
	if err := c.OpenPairing(30 * time.Second); err == nil {
		t.Fatal("want error")
	}
	if pairingOpen(c) || c.Admit(bt.Addr{0xB0, 2}) {
		t.Fatal("old window left open after a failed reopen")
	}
}

func TestShutdownAbortsInit(t *testing.T) {
	old := shutdownLockWait
	shutdownLockWait = 100 * time.Millisecond
	t.Cleanup(func() { shutdownLockWait = old })
	fake, conn := newFake(t)
	block := make(chan struct{})
	fake.onOp = func(op uint16) {
		if op == OpAddUUID {
			<-block
		}
	}
	t.Cleanup(func() { close(block) })
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := New(Config{Name: "Test", Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Open: func() (io.ReadWriteCloser, error) { return conn, nil }})
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "init running", func() bool { return fake.count(OpAddUUID) == 1 })
	done := make(chan struct{})
	go func() { c.Shutdown(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Shutdown blocked behind initAdapter")
	}
}

func TestSanitizeNameCaps248(t *testing.T) {
	n := strings.Repeat("a", 247) + "é" // 249 bytes; the cut falls inside the rune
	got := SanitizeName(n)
	if len(got) != 247 || !utf8.ValidString(got) {
		t.Fatalf("len %d valid %v", len(got), utf8.ValidString(got))
	}
	if got := SanitizeName(strings.Repeat("b", 300)); len(got) != 248 {
		t.Fatalf("len %d", len(got))
	}
}

func TestLoadLinkKeysPerKeyRetry(t *testing.T) {
	a, b := bt.Addr{0xA0, 1}, bt.Addr{0xA0, 2}
	dir := t.TempDir()
	seedKey(t, dir, a, 4)
	seedKey(t, dir, b, 4)
	fake, conn := newFake(t)
	// The batch and the first single key are rejected; the second key loads.
	fake.once = map[uint16]int{OpLoadLinkKeys: 2}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := New(Config{Name: "Test", Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Open: func() (io.ReadWriteCloser, error) { return conn, nil }})
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Shutdown)
	waitFor(t, "ready", func() bool { return c.Status().Present && fake.count(OpGetConnections) == 1 })
	if n := fake.count(OpLoadLinkKeys); n != 4 {
		t.Fatalf("%d load commands, want 4 (batch, 2 singles, final)", n)
	}
	if p := fake.lastParams(OpLoadLinkKeys); len(p) != 3+25 {
		t.Fatalf("final load %d bytes, want one key", len(p))
	}
	if c.Admit(a) == c.Admit(b) {
		t.Fatalf("exactly one key should be admitted: a=%v b=%v", c.Admit(a), c.Admit(b))
	}
}

func TestRevokeReloadSerialisedWithForget(t *testing.T) {
	known := bt.Addr{0xA0, 1, 2, 3, 4, 5}
	dir := t.TempDir()
	seedKey(t, dir, known, 8)
	fake, c, st := startFakeDir(t, false, dir)
	var once sync.Once
	fake.mu.Lock()
	fake.onOp = func(op uint16) {
		if op != OpUnpairDevice {
			return
		}
		// Forget lands while the revoke path is between Unpair and reload.
		once.Do(func() {
			go c.Forget(known)
			time.Sleep(50 * time.Millisecond)
		})
	}
	fake.mu.Unlock()
	fake.event(EvNewLinkKey, 0, newLinkKeyEvent(known, false))
	waitFor(t, "forget", func() bool { return !st.Has(known) && fake.count(OpUnpairDevice) == 2 })
	time.Sleep(50 * time.Millisecond)
	if c.isLoaded(known) || c.Admit(known) {
		t.Fatal("forgotten device re-admitted by the key reload")
	}
}

func TestNoBondingRepairOfBondedDeviceRestoresKey(t *testing.T) {
	known := bt.Addr{0xA0, 1, 2, 3, 4, 5}
	dir := t.TempDir()
	seedKey(t, dir, known, 8)
	fake, c, _ := startFakeDir(t, false, dir)
	if err := c.OpenPairing(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	fake.event(EvNewLinkKey, 0, newLinkKeyEvent(known, false))
	waitFor(t, "unloaded", func() bool { return !c.isLoaded(known) })
	if !c.Admit(known) {
		t.Fatal("session device not admitted")
	}
	if fake.count(OpUnpairDevice) != 0 {
		t.Fatal("in-window key must not be revoked")
	}
	before := fake.count(OpLoadLinkKeys)
	fake.event(EvDeviceDisconn, 0, append(addrParam(known), 0))
	waitFor(t, "reload", func() bool { return fake.count(OpLoadLinkKeys) == before+1 })
	waitFor(t, "loaded", func() bool { return c.isLoaded(known) })
}

// A no-bonding re-key after the window closed must still restore the stored
// key that an earlier in-window no-bonding re-pair flushed from the kernel.
func TestRevokeAfterNoBondingRepairRestoresKey(t *testing.T) {
	known := bt.Addr{0xA0, 1, 2, 3, 4, 5}
	dir := t.TempDir()
	seedKey(t, dir, known, 8)
	fake, c, _ := startFakeDir(t, false, dir)
	if err := c.OpenPairing(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	fake.event(EvNewLinkKey, 0, newLinkKeyEvent(known, false))
	waitFor(t, "unloaded", func() bool { return !c.isLoaded(known) })
	if err := c.ClosePairing(); err != nil {
		t.Fatal(err)
	}
	before := fake.count(OpLoadLinkKeys)
	fake.event(EvNewLinkKey, 0, newLinkKeyEvent(known, false))
	waitFor(t, "unpair", func() bool { return fake.count(OpUnpairDevice) == 1 })
	waitFor(t, "reload", func() bool { return fake.count(OpLoadLinkKeys) == before+1 })
	if p := fake.lastParams(OpLoadLinkKeys); len(p) != 3+25 || !bytes.Equal(p[3:9], addrParam(known)[:6]) {
		t.Fatalf("reload params %x, want the stored key", p)
	}
	waitFor(t, "loaded", func() bool { return c.isLoaded(known) })
}

func TestLEAddressEventsIgnored(t *testing.T) {
	p := addrParam(bt.Addr{1})
	p[6] = 1 // LE public
	if _, _, _, err := DecodeAddrEvent(p); !errors.Is(err, ErrNotBREDR) {
		t.Fatalf("err %v", err)
	}
	nk := newLinkKeyEvent(bt.Addr{1}, true)
	nk[7] = 2
	if _, err := DecodeNewLinkKey(nk); !errors.Is(err, ErrNotBREDR) {
		t.Fatalf("err %v", err)
	}
	dc := append(append([]byte(nil), p...), 0, 0, 0, 0, 0, 0)
	if _, err := DecodeDeviceConnected(dc); !errors.Is(err, ErrNotBREDR) {
		t.Fatalf("err %v", err)
	}
}

func TestFirstRefusalFullSetNotRearmed(t *testing.T) {
	_, c, _ := startFake(t, false)
	for i := range maxRefused {
		if !c.firstRefusal(bt.Addr{0xD0, byte(i)}) {
			t.Fatalf("addr %d: want first refusal", i)
		}
	}
	// Full and cleared recently (refusedT zero means never: the first fill
	// clears once), so a flood of new addresses must not re-arm warnings.
	if !c.firstRefusal(bt.Addr{0xD1}) {
		t.Fatal("first overflow clears the set")
	}
	for i := 1; i < maxRefused; i++ {
		c.firstRefusal(bt.Addr{0xD2, byte(i)})
	}
	if c.firstRefusal(bt.Addr{0xD3}) || c.firstRefusal(bt.Addr{0xD0, 0}) {
		t.Fatal("full set re-armed before refusedReset")
	}
	c.mu.Lock()
	c.refusedT = time.Now().Add(-refusedReset)
	c.mu.Unlock()
	if !c.firstRefusal(bt.Addr{0xD3}) {
		t.Fatal("set not cleared after refusedReset")
	}
}

func TestInitBackoff(t *testing.T) {
	want := []time.Duration{0, 5, 10, 20, 40, 60, 60, 60}
	for n, w := range want {
		if got := initBackoff(n); got != w*time.Second {
			t.Errorf("initBackoff(%d) = %v, want %v", n, got, w*time.Second)
		}
	}
}

func TestDisconnectNotConnected(t *testing.T) {
	fake, c, _ := startFake(t, false)
	fake.mu.Lock()
	fake.status = map[uint16]byte{OpDisconnect: byte(StatusNotConnected)}
	fake.mu.Unlock()
	if err := c.Disconnect(bt.Addr{0xA0, 1}); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("err = %v, want ErrNotConnected", err)
	}
	fake.mu.Lock()
	fake.status[OpDisconnect] = 0x05 // Authentication Failed
	fake.mu.Unlock()
	if err := c.Disconnect(bt.Addr{0xA0, 1}); err == nil || errors.Is(err, ErrNotConnected) {
		t.Fatalf("err = %v, want a generic failure", err)
	}
}

func TestNoBondingSessionRekeyKeepsConnection(t *testing.T) {
	fake, c, st := startFake(t, false)
	if err := c.OpenPairing(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	addr := bt.Addr{0xC0, 1, 2, 3, 4, 5}
	fake.event(EvNewLinkKey, 0, newLinkKeyEvent(addr, false))
	waitFor(t, "window closed", func() bool { return !pairingOpen(c) })
	fake.event(EvNewLinkKey, 0, newLinkKeyEvent(addr, false))
	// A later event is handled after the re-key: sync on it.
	fake.event(EvDeviceDisconn, 0, append(addrParam(addr), 0))
	waitFor(t, "not admitted", func() bool { return !c.Admit(addr) })
	if n := fake.count(OpUnpairDevice); n != 0 {
		t.Fatalf("%d unpairs, want 0 for a session re-key", n)
	}
	if st.Has(addr) {
		t.Fatal("session key stored")
	}
}
