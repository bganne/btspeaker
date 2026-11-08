package player

import (
	"encoding/binary"
	"testing"

	"btspeaker/internal/alsa"
)

func benchConvert(b *testing.B, in, out int, f alsa.Format, gain int32) {
	pcm := make([]int16, 1024*in)
	for i := range pcm {
		pcm[i] = int16(i * 37)
	}
	dst := make([]byte, 1024*out*f.BytesPerSample())
	b.SetBytes(int64(len(pcm) * 2))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		Convert(dst, pcm, in, out, f, gain)
	}
}

func BenchmarkConvertS16Unity(b *testing.B) { benchConvert(b, 2, 2, alsa.S16LE, unity) }
func BenchmarkConvertS16Gain(b *testing.B)  { benchConvert(b, 2, 2, alsa.S16LE, unity/2) }
func BenchmarkConvertS32Unity(b *testing.B) { benchConvert(b, 2, 2, alsa.S32LE, unity) }
func BenchmarkConvertS24Gain(b *testing.B)  { benchConvert(b, 2, 2, alsa.S24LE, unity/2) }
func BenchmarkConvertMonoS16(b *testing.B)  { benchConvert(b, 1, 2, alsa.S16LE, unity) }

func TestNegotiateOrder(t *testing.T) {
	c := alsa.Caps{Rates: []int{44100}, Formats: []alsa.Format{alsa.S16LE, alsa.S24LE, alsa.S32LE}, MinChannels: 2, MaxChannels: 2}
	for _, tc := range []struct {
		soft    bool
		formats []alsa.Format
		want    alsa.Format
	}{
		{false, c.Formats, alsa.S16LE},
		{true, c.Formats, alsa.S32LE},
		{true, []alsa.Format{alsa.S16LE, alsa.S24LE}, alsa.S24LE},
		{true, []alsa.Format{alsa.S16LE}, alsa.S16LE},
		{false, []alsa.Format{alsa.S24LE, alsa.S32LE}, alsa.S32LE},
	} {
		cc := c
		cc.Formats = tc.formats
		if f, _, err := Negotiate(cc, 44100, 2, tc.soft); err != nil || f != tc.want {
			t.Errorf("soft=%v %v: got %v, %v want %v", tc.soft, tc.formats, f, err, tc.want)
		}
	}
}

func s16s(b []byte) []int16 {
	o := make([]int16, len(b)/2)
	for i := range o {
		o[i] = int16(binary.LittleEndian.Uint16(b[2*i:]))
	}
	return o
}

func TestDither(t *testing.T) {
	const n = 20000
	in := make([]int16, n)
	for i := range in {
		in[i] = 1001
	}
	dst := make([]byte, 2*n)
	var c converter
	c.convert(dst, in, 1, 1, alsa.S16LE, unity/2, 44100) // 500.5 ideal
	var sum float64
	distinct := map[int16]bool{}
	for _, v := range s16s(dst) {
		if v < 499 || v > 502 {
			t.Fatalf("dither out of range: %d", v)
		}
		distinct[v] = true
		sum += float64(v)
	}
	if len(distinct) < 2 {
		t.Fatalf("no dither applied: %v", distinct)
	}
	if m := sum / n; m < 500.4 || m > 500.6 {
		t.Fatalf("mean %v, want ~500.5", m)
	}
	// unity: bit-exact; mute: exact zero
	var u converter
	u.convert(dst, in, 1, 1, alsa.S16LE, unity, 44100)
	for _, v := range s16s(dst) {
		if v != 1001 {
			t.Fatalf("unity altered: %d", v)
		}
	}
	var m converter
	m.convert(dst, in, 1, 1, alsa.S16LE, 0, 44100)
	for _, v := range s16s(dst) {
		if v != 0 {
			t.Fatalf("mute dithered: %d", v)
		}
	}
}

func TestGainRamp(t *testing.T) {
	const rate = 44100
	var c converter
	in := make([]int16, 100)
	for i := range in {
		in[i] = 10000
	}
	dst := make([]byte, 200)
	var out []int16
	c.convert(dst, in, 1, 1, alsa.S16LE, unity, rate)
	out = append(out, s16s(dst)...)
	for i := 0; i < 8; i++ { // unity -> mute, then stays muted
		g := int32(0)
		c.convert(dst, in, 1, 1, alsa.S16LE, g, rate)
		out = append(out, s16s(dst)...)
	}
	if out[99] != 10000 {
		t.Fatal("pre-ramp not unity")
	}
	maxStep := 0
	for i := 1; i < len(out); i++ {
		d := int(out[i]) - int(out[i-1])
		if d < 0 {
			d = -d
		}
		if d > maxStep {
			maxStep = d
		}
	}
	// 10000 over 441 frames = ~23 per frame, plus dither jitter
	if maxStep > 28 {
		t.Fatalf("max step %d", maxStep)
	}
	if out[100] == 0 || out[len(out)-1] != 0 || out[100+441+5] != 0 {
		t.Fatalf("ramp shape: first=%d last=%d", out[100], out[len(out)-1])
	}
	// S32 ramp is monotonic too and unchanged gain takes the plain path
	var c2 converter
	d4 := make([]byte, 4*100)
	c2.convert(d4, in, 1, 1, alsa.S32LE, unity/2, rate)
	c2.convert(d4, in, 1, 1, alsa.S32LE, unity/2, rate)
	if int32(binary.LittleEndian.Uint32(d4)) != 5000<<16 {
		t.Fatal("steady gain altered")
	}
}

func BenchmarkConverterS16Gain(b *testing.B) {
	pcm := make([]int16, 2048)
	dst := make([]byte, 4096)
	var c converter
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		c.convert(dst, pcm, 2, 2, alsa.S16LE, unity/2, 44100)
	}
}

func TestDitherSilence(t *testing.T) {
	var c converter
	dst := make([]byte, 4096)
	c.convert(dst, make([]int16, 2048), 2, 2, alsa.S16LE, unity/2, 44100)
	for _, x := range dst {
		if x != 0 {
			t.Fatal("digital silence dithered")
		}
	}
	// Zero samples inside non-silent audio are dithered like any other.
	pcm := make([]int16, 2048)
	pcm[0] = 10000
	c.convert(dst, pcm, 2, 2, alsa.S16LE, unity/2, 44100)
	nz := 0
	for i := 2; i < 2048; i++ {
		if binary.LittleEndian.Uint16(dst[2*i:]) != 0 {
			nz++
		}
	}
	if nz == 0 {
		t.Fatal("zero samples of quiet audio not dithered")
	}
	// Silence right after audio stays dithered for ditherHoldMs, then is exact.
	zero := make([]int16, 2048) // 1024 frames, ~23 ms
	c.convert(dst, zero, 2, 2, alsa.S16LE, unity/2, 44100)
	if allZero(dst) {
		t.Fatal("dither dropped right after audio")
	}
	for i := 0; i < 5; i++ {
		c.convert(dst, zero, 2, 2, alsa.S16LE, unity/2, 44100)
	}
	if !allZero(dst) {
		t.Fatal("silence still dithered after the hold")
	}
}

// fadeFrames is the fade length at 44.1 kHz.
const fadeFrames = 441

func TestFadeIn(t *testing.T) {
	for _, f := range []alsa.Format{alsa.S16LE, alsa.S24LE, alsa.S32LE} {
		neg16, neg32 := int16(-20000), int32(-1<<20)
		const frames = 2 * fadeFrames
		b := make([]byte, frames*2*f.BytesPerSample())
		for i := 0; i < frames*2; i++ {
			if f == alsa.S16LE {
				binary.LittleEndian.PutUint16(b[2*i:], uint16(neg16))
			} else {
				binary.LittleEndian.PutUint32(b[4*i:], uint32(neg32))
			}
		}
		if pos := fadeIn(b, f, 2, 0, fadeFrames); pos != fadeFrames {
			t.Fatalf("pos %d", pos)
		}
		get := func(i int) int64 {
			if f == alsa.S16LE {
				return int64(int16(binary.LittleEndian.Uint16(b[2*i:])))
			}
			return int64(int32(binary.LittleEndian.Uint32(b[4*i:])))
		}
		full := int64(-20000)
		if f != alsa.S16LE {
			full = -1 << 20
		}
		if get(0) != 0 || get(1) != 0 {
			t.Errorf("%v: first frame %d, want 0", f, get(0))
		}
		if mid := get(2 * (fadeFrames / 2)); mid < full/2+full/100 || mid > full/2-full/100 {
			t.Errorf("%v: mid ramp %d", f, mid)
		}
		if get(2*fadeFrames) != full || get(2*frames-1) != full {
			t.Errorf("%v: tail altered", f)
		}
	}
}

func TestConvertS32Extremes(t *testing.T) {
	in := []int16{-32768, 32767, 0}
	var rng uint32
	dst := make([]byte, 24)
	convert(dst, in, 1, 1, alsa.S32LE, unity, &rng)
	want := []int32{-32768 << 16, 32767 << 16, 0}
	for i, w := range want {
		if g := int32(binary.LittleEndian.Uint32(dst[4*i:])); g != w {
			t.Errorf("flat[%d]=%d want %d", i, g, w)
		}
	}
	convert(dst, in, 1, 2, alsa.S32LE, unity, &rng)
	for i, w := range want {
		for c := 0; c < 2; c++ {
			if g := int32(binary.LittleEndian.Uint32(dst[8*i+4*c:])); g != w {
				t.Errorf("dup[%d][%d]=%d want %d", i, c, g, w)
			}
		}
	}
}

func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}
