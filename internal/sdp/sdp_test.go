package sdp

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"

	"btspeaker/internal/bt"
)

type fakeConn struct {
	in     chan []byte
	out    chan []byte
	outMTU int
}

func newFakeConn(mtu int) *fakeConn {
	return &fakeConn{in: make(chan []byte, 16), out: make(chan []byte, 16), outMTU: mtu}
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
func (f *fakeConn) Close() error        { return nil }
func (f *fakeConn) RemoteAddr() bt.Addr { return bt.Addr{1, 2, 3, 4, 5, 6} }
func (f *fakeConn) PSM() uint16         { return 1 }
func (f *fakeConn) InMTU() int          { return 672 }
func (f *fakeConn) OutMTU() int         { return f.outMTU }

func ssaReq(tid uint16, uuid16 uint16, maxBytes uint16, cont []byte) []byte {
	var p []byte
	p = append(p, 0x35, 0x03, 0x19, byte(uuid16>>8), byte(uuid16))
	p = binary.BigEndian.AppendUint16(p, maxBytes)
	p = append(p, 0x35, 0x05, 0x0A, 0x00, 0x00, 0xFF, 0xFF) // all attributes
	p = append(p, cont...)
	return pdu(pduSearchAttrReq, tid, p)
}

func TestSearchAttrContinuation(t *testing.T) {
	s := NewServer()
	var full []byte
	cont := []byte{0}
	rounds := 0
	for {
		rsp := s.handle(ssaReq(uint16(rounds+1), 0x110B, 0xFFFF, cont), 48)
		if rsp[0] != pduSearchAttrRsp {
			t.Fatalf("pdu %x", rsp)
		}
		if binary.BigEndian.Uint16(rsp[1:]) != uint16(rounds+1) {
			t.Fatal("tid")
		}
		if len(rsp) > 48 {
			t.Fatalf("response %d > mtu", len(rsp))
		}
		n := int(binary.BigEndian.Uint16(rsp[5:]))
		full = append(full, rsp[7:7+n]...)
		cont = rsp[7+n:]
		rounds++
		if cont[0] == 0 {
			break
		}
		if rounds > 50 {
			t.Fatal("loop")
		}
	}
	if rounds < 2 {
		t.Fatal("expected continuation")
	}
	e, n, err := decode(full)
	if err != nil || n != len(full) {
		t.Fatalf("decode %v", err)
	}
	if len(e.kids) != 1 {
		t.Fatalf("want 1 record, got %d", len(e.kids))
	}
	attrs := e.kids[0].kids
	var gotName string
	for i := 0; i+1 < len(attrs); i += 2 {
		if attrs[i].u == 0x0100 {
			gotName = string(attrs[i+1].b)
		}
	}
	if gotName != "Audio Sink" {
		t.Fatalf("name %q", gotName)
	}
	if !bytes.Contains(full, []byte{0x19, 0x11, 0x0B}) {
		t.Fatal("missing class id")
	}
}

func TestBrowseGroupSearch(t *testing.T) {
	s := NewServer()
	p := []byte{0x35, 0x03, 0x19, 0x10, 0x02, 0x00, 0x10, 0x00}
	rsp := s.handle(pdu(pduSearchReq, 7, p), 672)
	if rsp[0] != pduSearchRsp {
		t.Fatalf("%x", rsp)
	}
	if total := binary.BigEndian.Uint16(rsp[5:]); total != 4 {
		t.Fatalf("total %d", total)
	}
	// MaximumServiceRecordCount limits.
	p = []byte{0x35, 0x03, 0x19, 0x10, 0x02, 0x00, 0x02, 0x00}
	rsp = s.handle(pdu(pduSearchReq, 7, p), 672)
	if binary.BigEndian.Uint16(rsp[7:]) != 2 {
		t.Fatalf("%x", rsp)
	}
}

func TestServiceAttributeAndErrors(t *testing.T) {
	s := NewServer()
	p := binary.BigEndian.AppendUint32(nil, 0x00010002)
	p = append(p, 0x00, 0x40, 0x35, 0x03, 0x09, 0x00, 0x01, 0x00)
	rsp := s.handle(pdu(pduAttrReq, 1, p), 672)
	if rsp[0] != pduAttrRsp {
		t.Fatalf("%x", rsp)
	}
	e, _, err := decode(rsp[7 : 7+int(binary.BigEndian.Uint16(rsp[5:]))])
	if err != nil || len(e.kids) != 2 || e.kids[1].kids[0].u != 0x110C {
		t.Fatalf("%v %+v", err, e)
	}
	// bad handle
	p = binary.BigEndian.AppendUint32(nil, 0x99)
	p = append(p, 0x00, 0x40, 0x35, 0x03, 0x09, 0x00, 0x01, 0x00)
	rsp = s.handle(pdu(pduAttrReq, 1, p), 672)
	if rsp[0] != pduError || binary.BigEndian.Uint16(rsp[5:]) != errBadHandle {
		t.Fatalf("%x", rsp)
	}
	// bad continuation
	rsp = s.handle(ssaReq(2, 0x110B, 100, []byte{2, 0xFF, 0xFF}), 672)
	if rsp[0] != pduError || binary.BigEndian.Uint16(rsp[5:]) != errBadContinue {
		t.Fatalf("%x", rsp)
	}
	// bad PDU size and unknown PDU
	rsp = s.handle([]byte{pduSearchReq, 0, 3, 0, 9, 1}, 672)
	if rsp[0] != pduError || binary.BigEndian.Uint16(rsp[5:]) != errBadPDUSize {
		t.Fatalf("%x", rsp)
	}
	rsp = s.handle(pdu(0x55, 3, nil), 672)
	if rsp[0] != pduError {
		t.Fatalf("%x", rsp)
	}
}

func TestHandleConn(t *testing.T) {
	c := newFakeConn(672)
	done := make(chan struct{})
	go func() { NewServer().HandleConn(c); close(done) }()
	c.in <- ssaReq(9, 0x110C, 0xFFFF, []byte{0})
	rsp := <-c.out
	if rsp[0] != pduSearchAttrRsp {
		t.Fatalf("%x", rsp)
	}
	close(c.in)
	<-done
}

func TestElementRejectsSmallVarSize(t *testing.T) {
	for _, h := range []byte{0x20, 0x40, 0x30, 0x38} { // str, url, seq, alt with size index 0
		if _, _, err := decode([]byte{h, 0}); err == nil {
			t.Fatalf("header %#x: want error", h)
		}
	}
	if _, _, err := decode([]byte{0x25, 0x01, 'a'}); err != nil {
		t.Fatal(err)
	}
}

func TestElementRejectsBadSizeIndex(t *testing.T) {
	for _, b := range [][]byte{
		{0x01, 0},                // NIL with size index 1
		{0x0D, 1, 0, 0, 0, 0, 0}, // uint with size index 5
		{0x15, 1, 0},             // int, index 5
		{0x1D, 1, 0},             // UUID, index 5
	} {
		if _, _, err := decode(b); err == nil {
			t.Fatalf("%x: want error", b)
		}
	}
}

func TestElementRoundTrip(t *testing.T) {
	e := seqE(uuidE(0x1234), uintE(4, 0xDEADBEEF), strE("hi"), seqE(), elem{t: tBool, u: 1})
	b := e.encode()
	d, n, err := decode(b)
	if err != nil || n != len(b) || !bytes.Equal(d.encode(), b) {
		t.Fatalf("%v", err)
	}
	if _, _, err := decode([]byte{0x35, 0x05, 0x19}); err == nil {
		t.Fatal("want error")
	}
}

func TestElementKidsCap(t *testing.T) {
	seq := func(n int) []byte { return append([]byte{0x35, byte(n)}, make([]byte, n)...) } // n nil elements
	if _, _, err := decode(seq(maxKids)); err != nil {
		t.Fatalf("%d kids: %v", maxKids, err)
	}
	if _, _, err := decode(seq(100)); err == nil {
		t.Fatal("100 kids: want error")
	}
}

func FuzzSDP(f *testing.F) {
	f.Add(ssaReq(1, 0x110B, 0xFFFF, []byte{0}), 48)
	f.Add(ssaReq(2, 0x1002, 0x0010, []byte{2, 0, 5}), 672)
	f.Add(pdu(pduSearchReq, 3, []byte{0x35, 0x03, 0x19, 0x11, 0x0B, 0x00, 0x10, 0x00}), 48)
	f.Add(pdu(pduAttrReq, 4, []byte{0, 0, 0, 1, 0xFF, 0xFF, 0x35, 0x05, 0x0A, 0, 0, 0xFF, 0xFF, 0}), 100)
	f.Add([]byte{0x02, 0, 1, 0, 0}, 0)
	s := NewServer()
	f.Fuzz(func(t *testing.T, req []byte, mtu int) {
		if rsp := s.handle(req, mtu); rsp != nil && len(rsp) < 5 {
			t.Fatalf("short response %x", rsp)
		}
	})
}

func TestElementBoolSizeIndex(t *testing.T) {
	if _, _, err := decode([]byte{0x28, 1}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := decode([]byte{0x29, 0, 1}); err == nil {
		t.Fatal("bool with size index 1 accepted")
	}
}

func TestMalformedContinuationState(t *testing.T) {
	s := NewServer()
	for _, cont := range [][]byte{{1, 0}, {2, 0}, {0, 0}} {
		rsp := s.handle(ssaReq(2, 0x110B, 100, cont), 672)
		if rsp[0] != pduError || binary.BigEndian.Uint16(rsp[5:]) != errBadContinue {
			t.Fatalf("%x: %x", cont, rsp)
		}
	}
}
