package avdtp

import "testing"

// FuzzSignal feeds arbitrary signalling packets (fragments included) through
// reassembly and dispatch on one device; it must never panic.
func FuzzSignal(f *testing.F) {
	f.Add(append([]byte{0x00, 0x03}, setConf...))
	f.Add([]byte{0x00, 0x01})
	f.Add([]byte{0x10, 0x0C, 0x04})
	f.Add([]byte{0x20, 0x06, 0x04})
	f.Add([]byte{0x30, 0x07, 0x04})
	f.Add([]byte{0x04, 0x03, 0x03, 0x04, 0x08})
	f.Add([]byte{0x08, 0x01, 0x02})
	f.Add([]byte{0x0C, 0x01, 0x02})
	f.Fuzz(func(t *testing.T, a []byte) {
		s := newTestServer(&fakeSink{})
		d := &device{addr: [6]byte{1}, sig: newConn(1)}
		s.devs[d.addr] = d
		// Split input into two packets so fragment state is exercised.
		for _, p := range [][]byte{a, a[len(a)/2:], a[:len(a)/2]} {
			label, sig, params, msg, ok := s.reassemble(d, p)
			if ok && msg == msgCommand {
				s.dispatch(d, label, sig, params)
			}
		}
		s.release(d)
	})
}

// FuzzRTP feeds arbitrary media packets to the RTP header parser; it must
// never panic and the payload must lie within the packet.
func FuzzRTP(f *testing.F) {
	f.Add([]byte{0x80, 0x60, 0, 1, 0, 0, 0, 1, 0, 0, 0, 1, 0x02, 1, 2, 3, 4, 5, 6, 7, 8})
	f.Add([]byte{0x90, 0x60, 0, 1, 0, 0, 0, 1, 0, 0, 0, 1, 0xBE, 0xDE, 0, 1, 9, 9, 9, 9, 0x01, 1, 2, 3, 4})
	f.Add([]byte{0xA2, 0x60, 0, 1, 0, 0, 0, 1, 0, 0, 0, 1, 1, 2, 3, 4, 5, 6, 7, 8, 9, 3})
	f.Add([]byte{0x80})
	f.Fuzz(func(t *testing.T, b []byte) {
		p := rtpPayload(b)
		if len(p) > len(b) {
			t.Fatal("payload longer than packet")
		}
	})
}
