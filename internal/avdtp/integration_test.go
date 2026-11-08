package avdtp

import (
	"os"
	"slices"
	"testing"

	"btspeaker/internal/sbc"
)

// TestRealSBCStream packs ffmpeg-encoded SBC into RTP media packets and checks
// the sink receives exactly what a direct decode of the file produces.
func TestRealSBCStream(t *testing.T) {
	data, err := os.ReadFile("../sbc/testdata/joint_44k_chirp.sbc")
	if err != nil {
		t.Skip(err)
	}
	// Reference decode and frame boundaries.
	ref := sbc.NewDecoder()
	pcm := make([]int16, 256)
	var want []int16
	var frames [][]byte
	for off := 0; off < len(data); {
		c, n, _, err := ref.Decode(data[off:], pcm)
		if err != nil {
			t.Fatalf("ref decode at %d: %v", off, err)
		}
		frames = append(frames, data[off:off+c])
		want = append(want, pcm[:n]...)
		off += c
	}

	sink := &fakeSink{}
	s := NewServer(Config{SampleRates: []int{44100, 48000}}, sink)
	sig := newConn(1)
	go s.HandleConn(sig)
	expect(t, sig, "setconf", append([]byte{0x30, 0x03}, setConf...), []byte{0x32, 0x03})
	expect(t, sig, "open", []byte{0x60, 0x06, 0x04}, []byte{0x62, 0x06})
	media := newConn(1)
	go s.HandleConn(media)
	waitFor(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.devs[sig.addr].media != nil })
	expect(t, sig, "start", []byte{0x70, 0x07, 0x04}, []byte{0x72, 0x07})

	// Fill packets up to the 672-byte MTU like a phone would.
	seq := uint16(0)
	for i := 0; i < len(frames); {
		pkt := []byte{0x80, 0x60, byte(seq >> 8), byte(seq), 0, 0, 0, 0, 0, 0, 0, 1, 0}
		n := 0
		for i < len(frames) && len(pkt)+len(frames[i]) <= 672 && n < 15 {
			pkt = append(pkt, frames[i]...)
			i++
			n++
		}
		pkt[12] = byte(n)
		media.in <- pkt
		seq++
	}
	waitFor(t, func() bool { _, n := sink.get(); return n == len(want) })
	sink.mu.Lock()
	got := sink.pcm
	sink.mu.Unlock()
	// The stream start is faded in (fadeFrames sample frames); the rest is bit-exact.
	skip := 2 * fadeFrames
	if len(got) != len(want) || !slices.Equal(got[skip:], want[skip:]) {
		t.Fatal("decoded stream differs from reference decode")
	}
	sig.Close()
}
