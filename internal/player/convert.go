package player

import (
	"encoding/binary"
	"errors"
	"math"
	"time"

	"btspeaker/internal/alsa"
)

// Convert writes pcm (inCh interleaved S16) to dst as interleaved samples in format f
// with outCh channels, applying a Q15 gain (unity = 1<<15 is a bit-exact passthrough).
// outCh == inCh, or inCh == 1 and outCh == 2 (duplicate). len(dst) must be
// frames*outCh*f.BytesPerSample(). Samples are scaled in int32/int64 and clipped.
func Convert(dst []byte, pcm []int16, inCh, outCh int, f alsa.Format, gain int32) {
	convert(dst, pcm, inCh, outCh, f, gain, nil)
}

// convert is Convert with optional TPDF dither (rng != nil) for the S16 path at
// non-unity gain; rng is an xorshift32 state.
func convert(dst []byte, pcm []int16, inCh, outCh int, f alsa.Format, gain int32, rng *uint32) {
	if inCh == 1 && outCh == 2 {
		convertDup(dst, pcm, f, gain, rng)
		return
	}
	// same channel count: interleaving is preserved, so convert as a flat sample run
	convertFlat(dst, pcm[:len(pcm)/inCh*inCh], f, gain, rng)
}

// converter adds per-stream state to convert: dither noise and a gain ramp. When the
// gain changes it is interpolated linearly over rampMs, continuing across chunks.
type converter struct {
	rng     uint32
	started bool
	target  int32
	cur     int64 // current gain, Q15<<8
	rem     int   // ramp frames left
	silent  int   // frames of silent chunks in a row
}

// ditherHoldMs is how long dither stays on into silence, so a fading tail does not
// toggle between dithered and undithered chunks.
const ditherHoldMs = 100

const rampMs = 10

func (c *converter) convert(dst []byte, pcm []int16, inCh, outCh int, f alsa.Format, gain int32, rate int) {
	c.convertSilent(dst, pcm, inCh, outCh, f, gain, rate, isSilent(pcm))
}

// convertSilent is convert with isSilent(pcm) already computed by the caller.
func (c *converter) convertSilent(dst []byte, pcm []int16, inCh, outCh int, f alsa.Format, gain int32, rate int, silent bool) {
	if !c.started {
		c.started, c.target, c.cur = true, gain, int64(gain)<<8
		c.rng = 0x9e3779b9
		c.silent = rate // a stream that starts silent is not dithered
	}
	if gain != c.target {
		c.target = gain
		c.rem = rate * rampMs / 1000
		if c.rem < 1 {
			c.rem = 1
		}
	}
	frames := len(pcm) / inCh
	if silent {
		c.silent = min(c.silent+frames, rate)
	} else {
		c.silent = 0
	}
	rng := &c.rng
	if c.silent > rate*ditherHoldMs/1000 { // keep digital silence exact; quiet audio still gets TPDF
		rng = nil
	}
	off := 0
	if c.rem > 0 {
		bps := outCh * f.BytesPerSample()
		end := int64(c.target) << 8
		for off < frames && c.rem > 0 {
			c.cur += (end - c.cur) / int64(c.rem)
			c.rem--
			convert(dst[off*bps:(off+1)*bps], pcm[off*inCh:(off+1)*inCh], inCh, outCh, f, int32(c.cur>>8), rng)
			off++
		}
	}
	if off < frames {
		bps := outCh * f.BytesPerSample()
		convert(dst[off*bps:frames*bps], pcm[off*inCh:frames*inCh], inCh, outCh, f, c.target, rng)
	}
}

// next advances an xorshift32 state.
func next(r *uint32) uint32 {
	x := *r
	x ^= x << 13
	x ^= x >> 17
	x ^= x << 5
	*r = x
	return x
}

// tpdf returns triangular noise of +-1 LSB (scaled for Q15: +-1<<15) as the sum of
// two 15-bit uniforms taken from the high and the middle bits of one draw.
func tpdf(r *uint32) int32 {
	x := next(r)
	return int32(x>>17) + int32((x>>2)&0x7FFF) - 32767
}

// scale16 applies gain with rounding (and dither if rng != nil) to one S16 sample.
func scale16(s int16, gain int32, rng *uint32) int16 {
	if rng == nil || gain == 0 {
		return int16(clip16((int32(s)*gain + unity/2) >> 15))
	}
	// int32 suffices: |s*gain| <= 2^30 (gain <= unity), rounding 2^14, dither < 2^15.
	v := (int32(s)*gain + unity/2 + tpdf(rng)) >> 15
	return int16(clip16(v))
}

// convertFlat converts len(pcm) samples one-to-one.
func convertFlat(dst []byte, pcm []int16, f alsa.Format, gain int32, rng *uint32) {
	switch f {
	case alsa.S16LE:
		dst = dst[:2*len(pcm)]
		if gain == unity {
			for i, s := range pcm {
				binary.LittleEndian.PutUint16(dst[2*i:], uint16(s))
			}
			return
		}
		for i, s := range pcm {
			binary.LittleEndian.PutUint16(dst[2*i:], uint16(scale16(s, gain, rng)))
		}
	case alsa.S24LE:
		dst = dst[:4*len(pcm)]
		for i, s := range pcm {
			v := (int32(s) * gain) >> 7 // s<<8 * gain>>15; gain <= unity so |v| <= 1<<23, no clip needed
			binary.LittleEndian.PutUint32(dst[4*i:], uint32(v))
		}
	default: // S32LE
		dst = dst[:4*len(pcm)]
		for i, s := range pcm {
			v := (int32(s) * gain) << 1 // s<<16 * gain>>15
			binary.LittleEndian.PutUint32(dst[4*i:], uint32(v))
		}
	}
}

// convertDup converts mono pcm to stereo by duplicating each sample.
func convertDup(dst []byte, pcm []int16, f alsa.Format, gain int32, rng *uint32) {
	switch f {
	case alsa.S16LE:
		dst = dst[:4*len(pcm)]
		for i, s := range pcm {
			v := s
			if gain != unity {
				v = scale16(s, gain, rng)
			}
			d := dst[4*i : 4*i+4 : 4*i+4]
			d[0], d[1] = byte(v), byte(uint16(v)>>8)
			d[2], d[3] = d[0], d[1]
		}
	case alsa.S24LE:
		dst = dst[:8*len(pcm)]
		for i, s := range pcm {
			v := uint32((int32(s) * gain) >> 7)
			binary.LittleEndian.PutUint32(dst[8*i:], v)
			binary.LittleEndian.PutUint32(dst[8*i+4:], v)
		}
	default:
		dst = dst[:8*len(pcm)]
		for i, s := range pcm {
			v := uint32((int32(s) * gain) << 1)
			binary.LittleEndian.PutUint32(dst[8*i:], v)
			binary.LittleEndian.PutUint32(dst[8*i+4:], v)
		}
	}
}

func clip16(v int32) int32 {
	if v > math.MaxInt16 {
		return math.MaxInt16
	}
	if v < math.MinInt16 {
		return math.MinInt16
	}
	return v
}

// TestTone plays a 1 kHz sine at -12 dBFS for d on dev (44100 Hz if supported, else
// the first supported rate).
func TestTone(dev string, d time.Duration) error {
	return testTone(alsaBackend{}, dev, d)
}

func testTone(b Backend, dev string, d time.Duration) error {
	caps, err := b.Probe(dev)
	if err != nil {
		return err
	}
	rate := 44100
	if !caps.HasRate(rate) {
		if len(caps.Rates) == 0 {
			return errNoRate
		}
		rate = caps.Rates[0]
	}
	ch := 2
	if caps.MaxChannels < 2 {
		ch = 1
	}
	f, outCh, err := Negotiate(caps, rate, ch, false)
	if err != nil {
		return err
	}
	pcm, err := b.Open(dev, rate, outCh, f, rate/5)
	if err != nil {
		return err
	}
	total := int(int64(d) * int64(rate) / int64(time.Second))
	const amp = 0.2512 * 32767 // -12 dBFS
	chunk := rate / 100
	in := make([]int16, 0, chunk*ch)
	out := make([]byte, chunk*outCh*f.BytesPerSample())
	for n := 0; n < total; {
		in = in[:0]
		for i := 0; i < chunk && n < total; i, n = i+1, n+1 {
			s := int16(math.Round(amp * math.Sin(2*math.Pi*1000*float64(n)/float64(rate))))
			for c := 0; c < ch; c++ {
				in = append(in, s)
			}
		}
		frames := len(in) / ch
		if err := pcm.Write(sliceOf(out, frames*outCh*f.BytesPerSample(), func(dst []byte) { Convert(dst, in, ch, outCh, f, unity) })); err != nil {
			pcm.Close()
			return err
		}
	}
	err = pcm.Drain()
	if cerr := pcm.Close(); err == nil {
		err = cerr
	}
	return err
}

func sliceOf(b []byte, n int, fill func([]byte)) []byte {
	b = b[:n]
	fill(b)
	return b
}

var errNoRate = errors.New("device supports none of the SBC sample rates")

// fadeLen is the length in frames of the fades applied after an xrun or drop (10 ms).
func fadeLen(rate int) int { return max(rate/100, 1) }

// fadeIn ramps interleaved b (format f, ch channels) towards full level, continuing a
// fade that is pos frames in (0 starts from silence), hiding the discontinuity after
// an xrun recovery. n is the fade length (fadeLen). It returns the new position; n means the fade is done.
func fadeIn(b []byte, f alsa.Format, ch, pos, n int) int {
	bps := f.BytesPerSample()
	frames := len(b) / (bps * ch)
	for i := 0; i < frames && pos < n; i, pos = i+1, pos+1 {
		g := int64(pos) // gain = pos/n
		for c := 0; c < ch; c++ {
			o := (i*ch + c) * bps
			if f == alsa.S16LE {
				v := int64(int16(binary.LittleEndian.Uint16(b[o:])))
				binary.LittleEndian.PutUint16(b[o:], uint16(int16(v*g/int64(n))))
			} else { // S32LE, S24LE (sign-extended in 32 bits)
				v := int64(int32(binary.LittleEndian.Uint32(b[o:])))
				binary.LittleEndian.PutUint32(b[o:], uint32(int32(v*g/int64(n))))
			}
		}
	}
	return pos
}

// fadeTail returns n frames ramping linearly from the given last frame
// (format f, ch channels) to silence, so a stream can end without a step.
func fadeTail(last []byte, f alsa.Format, ch, n int) []byte {
	bps := f.BytesPerSample()
	out := make([]byte, n*ch*bps)
	for i := 0; i < n; i++ {
		g := int64(n - 1 - i) // gain = g/n, last frame is below 1
		for c := 0; c < ch; c++ {
			o := (i*ch + c) * bps
			if f == alsa.S16LE {
				v := int64(int16(binary.LittleEndian.Uint16(last[c*bps:])))
				binary.LittleEndian.PutUint16(out[o:], uint16(int16(v*g/int64(n))))
			} else {
				v := int64(int32(binary.LittleEndian.Uint32(last[c*bps:])))
				binary.LittleEndian.PutUint32(out[o:], uint32(int32(v*g/int64(n))))
			}
		}
	}
	return out
}
