package sbc

import (
	"encoding/binary"
	"errors"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// decodeAll decodes a whole raw .sbc stream.
func decodeAll(t testing.TB, data []byte) (pcm []int16, infos []FrameInfo) {
	t.Helper()
	d := NewDecoder()
	buf := make([]int16, 256)
	for len(data) > 0 {
		c, n, info, err := d.Decode(data, buf)
		if err != nil {
			t.Fatalf("decode at frame %d: %v", len(infos), err)
		}
		pcm = append(pcm, buf[:n]...)
		infos = append(infos, info)
		data = data[c:]
	}
	return
}

func snrDB(ref, got []int16) (snr float64, maxDiff int) {
	var sig, noise float64
	for i := range ref {
		r, g := float64(ref[i]), float64(got[i])
		sig += r * r
		noise += (r - g) * (r - g)
		if d := int(math.Abs(r - g)); d > maxDiff {
			maxDiff = d
		}
	}
	if noise == 0 {
		return math.Inf(1), 0
	}
	return 10 * math.Log10(sig/noise), maxDiff
}

func TestFFmpegVectors(t *testing.T) {
	files, _ := filepath.Glob("testdata/*.sbc")
	if len(files) == 0 {
		t.Fatal("no test vectors")
	}
	modes := map[ChannelMode]bool{}
	subbands := map[int]bool{}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".sbc")
		t.Run(name, func(t *testing.T) {
			sbcData, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(strings.TrimSuffix(f, ".sbc") + ".pcm")
			if err != nil {
				t.Fatal(err)
			}
			ref := make([]int16, len(raw)/2)
			for i := range ref {
				ref[i] = int16(binary.LittleEndian.Uint16(raw[2*i:]))
			}
			got, infos := decodeAll(t, sbcData)
			if len(got) != len(ref) {
				t.Fatalf("got %d samples, ffmpeg %d", len(got), len(ref))
			}
			modes[infos[0].Mode] = true
			subbands[infos[0].Subbands] = true
			snr, maxd := snrDB(ref, got)
			t.Logf("%+v frames=%d SNR=%.1f dB maxdiff=%d", infos[0], len(infos), snr, maxd)
			// libsbc and ffmpeg are both 16-bit fixed point with different
			// intermediate rounding: the difference is zero-gain, near zero-mean
			// noise of a few LSB (sigma ~2.5, max ~12) regardless of level, so
			// SNR is capped around 70-75 dB on these -12 dBFS..-6 dBFS signals.
			if snr < 65 || maxd > 16 {
				t.Errorf("SNR %.1f dB (want >= 65), maxdiff %d (want <= 16)", snr, maxd)
			}
		})
	}
	for _, m := range []ChannelMode{Mono, Stereo, JointStereo, DualChannel} {
		if !modes[m] {
			t.Errorf("no vector with mode %d", m)
		}
	}
	if !subbands[4] || !subbands[8] {
		t.Errorf("subbands covered: %v", subbands)
	}
}

func loadFrames(t *testing.T, name string) [][]byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var out [][]byte
	for len(data) > 0 {
		fi, ok := parseHeader(data)
		if !ok || len(data) < fi.FrameLen {
			t.Fatal("bad vector")
		}
		out = append(out, data[:fi.FrameLen])
		data = data[fi.FrameLen:]
	}
	return out
}

type bitWriter struct {
	b   []byte
	pos int
}

func (w *bitWriter) put(v, n int) {
	for i := n - 1; i >= 0; i-- {
		if w.pos&7 == 0 {
			w.b = append(w.b, 0)
		}
		if v>>uint(i)&1 != 0 {
			w.b[w.pos>>3] |= 0x80 >> uint(w.pos&7)
		}
		w.pos++
	}
}

// monoFields extracts scale factors and raw quantized samples of a mono frame.
func monoFields(fi FrameInfo, f []byte) (sf [8]int, nbits [8]int, samples [16][8]int) {
	br := bitReader{b: f, idx: 4}
	for sb := 0; sb < fi.Subbands; sb++ {
		sf[sb] = br.get(4)
	}
	var s, n [2][8]int
	s[0] = sf
	computeNbits(&fi, s[:1], n[:1], 1)
	nbits = n[0]
	for blk := 0; blk < fi.Blocks; blk++ {
		for sb := 0; sb < fi.Subbands; sb++ {
			samples[blk][sb] = br.get(nbits[sb])
		}
	}
	return
}

// buildDual packs two mono frames (same parameters) into one dual channel frame.
func buildDual(t *testing.T, a, b []byte) []byte {
	t.Helper()
	fa, _ := parseHeader(a)
	sfA, nbA, smA := monoFields(fa, a)
	sfB, nbB, smB := monoFields(fa, b)

	w := &bitWriter{}
	w.put(0x9c, 8)
	w.put(int(a[1]&^0x0c|0x04), 8) // dual channel
	w.put(fa.Bitpool, 8)
	w.put(0, 8)
	for _, sf := range [][8]int{sfA, sfB} {
		for sb := 0; sb < fa.Subbands; sb++ {
			w.put(sf[sb], 4)
		}
	}
	for blk := 0; blk < fa.Blocks; blk++ {
		for sb := 0; sb < fa.Subbands; sb++ {
			w.put(smA[blk][sb], nbA[sb])
		}
		for sb := 0; sb < fa.Subbands; sb++ {
			w.put(smB[blk][sb], nbB[sb])
		}
	}
	fd, ok := parseHeader(w.b[:4])
	if !ok {
		t.Fatal("bad dual header")
	}
	if len(w.b) != fd.FrameLen {
		t.Fatalf("dual len %d want %d", len(w.b), fd.FrameLen)
	}
	w.b[3] = crc8(&fd, w.b)
	return w.b
}

// Dual channel can't be produced by ffmpeg: build dual frames from two mono
// frames (same bitpool/params) and require each channel to match the mono decode.
func TestDualChannel(t *testing.T) {
	for _, vec := range []string{"mono_48k_noise.sbc", "mono4_32k_chirp.sbc", "mono_16k_chirp.sbc"} {
		frames := loadFrames(t, vec)
		for _, loud := range []bool{true, false} {
			dm, ds := NewDecoder(), NewDecoder()
			dd := NewDecoder()
			var mA, mB [256]int16
			dual := make([]int16, 256)
			for i := 0; i+3 < len(frames); i += 2 {
				a, b := append([]byte(nil), frames[i]...), append([]byte(nil), frames[i+3]...)
				if !loud { // SNR allocation, still a valid mono frame
					a[1] |= 2
					b[1] |= 2
				}
				fix := func(f []byte) {
					fi, _ := parseHeader(f)
					f[3] = crc8(&fi, f)
				}
				fix(a)
				fix(b)
				dualFrame := buildDual(t, a, b)

				_, na, _, err := dm.Decode(a, mA[:])
				if err != nil {
					t.Fatal(err)
				}
				if _, _, _, err = ds.Decode(b, mB[:]); err != nil {
					t.Fatal(err)
				}
				_, n, info, err := dd.Decode(dualFrame, dual)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode != DualChannel || n != 2*na {
					t.Fatalf("info %+v n=%d", info, n)
				}
				for k := 0; k < na; k++ {
					if dual[2*k] != mA[k] || dual[2*k+1] != mB[k] {
						t.Fatalf("%s loud=%v frame %d sample %d: dual (%d,%d) mono (%d,%d)",
							vec, loud, i, k, dual[2*k], dual[2*k+1], mA[k], mB[k])
					}
				}
			}
		}
	}
}

// Bit allocation invariants over all modes (SNR allocation can't be produced by ffmpeg).
func TestAllocationInvariants(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for iter := 0; iter < 20000; iter++ {
		fi := FrameInfo{
			SampleRate: sampleRates[rng.Intn(4)],
			Subbands:   4 << uint(rng.Intn(2)),
			Loudness:   rng.Intn(2) == 0,
			Channels:   1 + rng.Intn(2),
		}
		fi.Bitpool = rng.Intn(16*fi.Subbands*fi.Channels + 1) // reachable bitpools only
		var sf, nb [2][8]int
		for c := range sf {
			for s := range sf[c] {
				sf[c][s] = rng.Intn(16)
			}
		}
		computeNbits(&fi, sf[:], nb[:], fi.Channels)
		total := 0
		for c := 0; c < fi.Channels; c++ {
			for s := 0; s < fi.Subbands; s++ {
				n := nb[c][s]
				if n < 0 || n > 16 {
					t.Fatalf("bad nbits %d", n)
				}
				total += n
			}
		}
		if total > fi.Bitpool {
			t.Fatalf("allocated %d > bitpool %d (%+v)", total, fi.Bitpool, fi)
		}
	}
}

// Random payloads are valid for every header: decode must not panic or err.
func TestRandomFrames(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	d := NewDecoder()
	pcm := make([]int16, 256)
	ok := 0
	for iter := 0; iter < 20000; iter++ {
		f := make([]byte, 600)
		rng.Read(f)
		f[0] = 0x9c
		f[2] = byte(rng.Intn(256))
		fi, valid := parseHeader(f)
		if !valid {
			if _, _, _, err := d.Decode(f, pcm); !errors.Is(err, ErrSync) {
				t.Fatalf("want ErrSync, got %v", err)
			}
			continue
		}
		f[3] = crc8(&fi, f)
		c, n, _, err := d.Decode(f, pcm)
		if err != nil || c != fi.FrameLen || n != fi.Channels*fi.Blocks*fi.Subbands {
			t.Fatalf("%+v: c=%d n=%d err=%v", fi, c, n, err)
		}
		ok++
	}
	if ok < 1000 {
		t.Fatalf("only %d valid", ok)
	}
}

func TestErrors(t *testing.T) {
	frames := loadFrames(t, "stereo_44k_noise.sbc")
	pcm := make([]int16, 256)
	d := NewDecoder()
	f := frames[0]

	check := func(name string, in []byte, want error) {
		t.Helper()
		wantC := 0
		if want == ErrCRC {
			wantC = len(f) // a CRC error still reports the frame length so it can be skipped
		}
		c, n, _, err := d.Decode(in, pcm)
		if !errors.Is(err, want) || c != wantC || n != 0 {
			t.Errorf("%s: c=%d n=%d err=%v, want c=%d %v", name, c, n, err, wantC, want)
		}
	}
	bad := append([]byte(nil), f...)
	bad[10] ^= 0x10
	check("crc payload", bad, ErrCRC)
	bad = append([]byte(nil), f...)
	bad[3] ^= 1
	check("crc byte", bad, ErrCRC)
	bad = append([]byte(nil), f...)
	bad[0] = 0xad
	check("sync", bad, ErrSync)
	check("truncated", f[:len(f)-1], ErrTruncated)
	check("header only", f[:4], ErrTruncated)
	check("short header", f[:2], ErrTruncated)
	check("empty", nil, ErrTruncated)
	bad = append([]byte(nil), f...)
	bad[2] = 255 // bitpool too large
	check("bitpool", bad, ErrSync)

	// Extra trailing data is not consumed.
	two := append(append([]byte(nil), frames[0]...), frames[1]...)
	c, _, info, err := d.Decode(two, pcm)
	if err != nil || c != len(frames[0]) || info.FrameLen != c {
		t.Errorf("trailing: c=%d err=%v", c, err)
	}
	if _, _, _, err := d.Decode(f, pcm[:10]); !errors.Is(err, ErrPCMSize) {
		t.Error("short pcm buffer accepted")
	}
}

func TestReset(t *testing.T) {
	frames := loadFrames(t, "joint_44k_chirp.sbc")
	a, b := make([]int16, 256), make([]int16, 256)
	d := NewDecoder()
	d.Decode(frames[0], a)
	d.Decode(frames[1], a)
	d.Reset()
	d.Decode(frames[0], a)
	NewDecoder().Decode(frames[0], b)
	if *(*[256]int16)(a) != *(*[256]int16)(b) {
		t.Error("Reset did not clear state")
	}
}

func benchFrame() []byte {
	// 44.1k, 16 blocks, joint stereo, loudness, 8 subbands, bitpool 53
	f := make([]byte, 0, 200)
	f = append(f, 0x9c, 0xbd, 53, 0)
	fi, _ := parseHeader(f)
	rng := rand.New(rand.NewSource(3))
	for len(f) < fi.FrameLen {
		f = append(f, byte(rng.Intn(256)))
	}
	f[3] = crc8(&fi, f)
	return f
}

func TestNoAllocs(t *testing.T) {
	f := benchFrame()
	d := NewDecoder()
	pcm := make([]int16, 256)
	if a := testing.AllocsPerRun(100, func() { d.Decode(f, pcm) }); a != 0 {
		t.Errorf("%v allocs per frame", a)
	}
}

func BenchmarkDecode(b *testing.B) {
	f := benchFrame()
	d := NewDecoder()
	pcm := make([]int16, 256)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, _, _, err := d.Decode(f, pcm); err != nil {
			b.Fatal(err)
		}
	}
}

// TestGenDerivedVectors writes the vectors ffmpeg's encoder can't produce
// (dual channel, SNR allocation) into $SBC_GEN_DIR, derived from the encoder's
// mono/joint vectors. testdata/gen.sh runs it, then has ffmpeg decode them.
func TestGenDerivedVectors(t *testing.T) {
	dir := os.Getenv("SBC_GEN_DIR")
	if dir == "" {
		t.Skip("SBC_GEN_DIR not set")
	}
	write := func(name string, frames [][]byte) {
		var out []byte
		for _, f := range frames {
			out = append(out, f...)
		}
		if err := os.WriteFile(filepath.Join(dir, name), out, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	snr := func(f []byte) []byte {
		f = append([]byte(nil), f...)
		f[1] |= 2
		fi, _ := parseHeader(f)
		f[3] = crc8(&fi, f)
		return f
	}
	dual := func(src string, useSNR bool) [][]byte {
		frames := loadFrames(t, src)
		var out [][]byte
		for i := 0; i+3 < len(frames); i += 2 {
			a, b := frames[i], frames[i+3]
			if useSNR {
				a, b = snr(a), snr(b)
			}
			out = append(out, buildDual(t, a, b))
		}
		return out
	}
	write("dual_48k_noise.sbc", dual("mono_48k_noise.sbc", false))
	write("dualsnr_16k_chirp.sbc", dual("mono_16k_chirp.sbc", true))
	// Not noise: garbage coefficients of a noise vector saturate, where the
	// decoders' intermediate rounding legitimately differs.
	for _, src := range []string{"joint_32k_sine", "stereo4_44k_sine"} {
		var frames [][]byte
		for _, f := range loadFrames(t, src+".sbc") {
			frames = append(frames, snr(f))
		}
		write("snr_"+src+".sbc", frames)
	}
}

func TestParamChangeResets(t *testing.T) {
	frames := loadFrames(t, "joint_44k_chirp.sbc")
	// Valid mono 16 kHz frame: different parameters from the chirp stream.
	other := []byte{0x9c, 0x1c, 20, 0}
	fi, ok := parseHeader(other)
	if !ok {
		t.Fatal("bad header")
	}
	for len(other) < fi.FrameLen {
		other = append(other, byte(len(other)*37))
	}
	other[3] = crc8(&fi, other)

	a, b := make([]int16, 256), make([]int16, 256)
	d := NewDecoder()
	d.Decode(frames[0], a)
	d.Decode(frames[1], a)
	if _, _, _, err := d.Decode(other, a); err != nil {
		t.Fatal(err)
	}
	d.Decode(frames[0], a)
	NewDecoder().Decode(frames[0], b)
	if *(*[256]int16)(a) != *(*[256]int16)(b) {
		t.Error("parameter change did not reset state")
	}
}
