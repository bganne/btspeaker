package avdtp

import (
	"io"
	"testing"
	"time"

	"btspeaker/internal/sbc"
)

func rtp(seq uint16, ts uint32, frames ...byte) []byte {
	p := []byte{0x80, 0x60, byte(seq >> 8), byte(seq), byte(ts >> 24), byte(ts >> 16), byte(ts >> 8), byte(ts), 0, 0, 0, 1, byte(len(frames) / 4)}
	return append(p, frames...)
}

// A lost packet and a corrupt frame are replaced by silence so that the output
// length follows the RTP timestamps, with no large step at the boundaries.
func TestConcealment(t *testing.T) {
	sink := &fakeSink{}
	s := newTestServer(sink)
	s.newDecoder = func() decoder { return &stepDec{} }
	sig := newConn(1)
	go s.HandleConn(sig)
	expect(t, sig, "setconf", append([]byte{0x30, 0x03}, setConf...), []byte{0x32, 0x03})
	expect(t, sig, "open", []byte{0x60, 0x06, 0x04}, []byte{0x62, 0x06})
	media := newConn(1)
	go s.HandleConn(media)
	waitFor(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.devs[sig.addr].media != nil })
	expect(t, sig, "start", []byte{0x70, 0x07, 0x04}, []byte{0x72, 0x07})

	// A good frame is 4 bytes decoding to 128 sample frames (16 blocks x 8
	// subbands, as configured) of constant level 10000; 2 frames = 256 ticks.
	good := []byte{1, 1, 1, 1, 1, 1, 1, 1}
	media.in <- rtp(1, 1000, good...)
	media.in <- rtp(2, 1256, good...)
	media.in <- rtp(4, 1768, good...)                            // seq 3 (ts 1512..1767) lost
	media.in <- rtp(5, 2024, 1, 1, 1, 1, 0xFF, 0xFF, 0xFF, 0xFF) // second frame corrupt
	media.in <- rtp(6, 2280, good...)
	// ts spans 1000..2535 = 1536 sample frames = 3072 int16 (stereo).
	waitFor(t, func() bool { _, n := sink.get(); return n >= 3072 })
	time.Sleep(20 * time.Millisecond)
	sink.mu.Lock()
	pcm := append([]int16(nil), sink.pcm...)
	sink.mu.Unlock()
	if len(pcm) != 3072 {
		t.Fatalf("got %d samples want 3072", len(pcm))
	}
	zeros := 0
	for _, v := range pcm {
		if v == 0 {
			zeros++
		}
	}
	if zeros < 2*(200+90) {
		t.Fatalf("only %d zero samples: gaps not concealed", zeros)
	}
	maxStep := 0
	for i := 2; i < len(pcm); i++ {
		d := int(pcm[i]) - int(pcm[i-2])
		if d < 0 {
			d = -d
		}
		maxStep = max(maxStep, d)
	}
	if maxStep > 1000 {
		t.Fatalf("step %d at a boundary", maxStep)
	}
	if st := s.Stats(); st.LostPackets != 1 || st.BadFrames != 1 || st.ConcealedMs == 0 {
		t.Fatalf("stats %+v", st)
	}
	sig.Close()
}

// stepDec decodes 4-byte frames: bytes all 1 -> constant 10000; else error.
type stepDec struct{}

func (stepDec) Decode(f []byte, pcm []int16) (int, int, sbc.FrameInfo, error) {
	if len(f) < 4 || f[0] != 1 {
		return 0, 0, sbc.FrameInfo{}, io.ErrUnexpectedEOF
	}
	for i := 0; i < 256; i++ {
		pcm[i] = 10000
	}
	return 4, 256, sbc.FrameInfo{SampleRate: 44100, Channels: 2, Blocks: 16, Subbands: 8}, nil
}
func (stepDec) Reset() {}
