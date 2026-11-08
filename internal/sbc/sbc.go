// Copyright 2022 Google LLC (original libsbc C implementation)
// Licensed under the Apache License, Version 2.0 (the "License"); you may not
// use this file except in compliance with the License. You may obtain a copy
// of the License at http://www.apache.org/licenses/LICENSE-2.0
//
// This Go port follows google/libsbc (src/sbc.c): 16-bit fixed-point subband
// samples with 32-bit integer arithmetic, allocation-free per frame.

// Package sbc is a pure-Go SBC (A2DP low-complexity subband codec) decoder,
// ported from google/libsbc (Apache-2.0).
package sbc

import "errors"

// ChannelMode as encoded in the SBC frame header.
type ChannelMode uint8

const (
	Mono ChannelMode = iota
	DualChannel
	Stereo
	JointStereo
)

// FrameInfo describes the most recently decoded frame header.
type FrameInfo struct {
	SampleRate int // 16000, 32000, 44100 or 48000
	Channels   int // 1 or 2
	Mode       ChannelMode
	Blocks     int  // 4, 8, 12, 16
	Subbands   int  // 4 or 8
	Loudness   bool // allocation method: true=loudness, false=SNR
	Bitpool    int
	FrameLen   int // bytes, header included
}

var (
	ErrSync      = errors.New("sbc: bad syncword or header")
	ErrTruncated = errors.New("sbc: truncated frame")
	ErrCRC       = errors.New("sbc: CRC mismatch")
)

const headerSize = 4

// dstate is the per-channel synthesis state (circular buffer of DCT output).
type dstate struct {
	idx int
	v   [2][8][10]int16
}

// Decoder holds the synthesis filter state; one per stream, not safe for concurrent use.
type Decoder struct {
	st   [2]dstate
	last FrameInfo // parameters of the previous frame, to detect changes
}

func NewDecoder() *Decoder { return &Decoder{} }

// Reset clears filter state (call when a stream restarts).
func (d *Decoder) Reset() { *d = Decoder{} }

var sampleRates = [4]int{16000, 32000, 44100, 48000}

func parseHeader(b []byte) (fi FrameInfo, ok bool) {
	if len(b) < headerSize || b[0] != 0x9c {
		return fi, false
	}
	fi.SampleRate = sampleRates[b[1]>>6]
	fi.Blocks = (1 + int(b[1]>>4&3)) << 2
	fi.Mode = ChannelMode(b[1] >> 2 & 3)
	fi.Loudness = b[1]&2 == 0
	fi.Subbands = (1 + int(b[1]&1)) << 2
	fi.Bitpool = int(b[2])
	fi.Channels = 1
	if fi.Mode != Mono {
		fi.Channels = 2
	}
	two := 0
	if fi.Channels == 2 {
		two = 1
	}
	dual := 0
	if fi.Mode == DualChannel {
		dual = 1
	}
	joint := 0
	if fi.Mode == JointStereo {
		joint = fi.Subbands
	}
	stereo := 0
	if fi.Mode == Stereo || fi.Mode == JointStereo {
		stereo = 1
	}
	maxBits := (16*fi.Subbands*fi.Blocks)<<two - headerSize*8 - (4*fi.Subbands)<<two - joint
	maxPool := min(maxBits/(fi.Blocks<<dual), (16<<stereo)*fi.Subbands)
	if fi.Bitpool > maxPool {
		return fi, false
	}
	nbits := (4*fi.Subbands)<<two + (fi.Blocks*fi.Bitpool)<<dual + joint
	fi.FrameLen = headerSize + (nbits+7)>>3
	return fi, true
}

func crc8(fi *FrameInfo, data []byte) uint8 {
	nbit := fi.Channels * fi.Subbands * 4
	if fi.Mode == JointStereo {
		nbit += fi.Subbands
	}
	crc := uint8(0x0f)
	crc = crcTable[crc^data[1]]
	crc = crcTable[crc^data[2]]
	i := 4
	for ; i < 4+nbit/8; i++ {
		crc = crcTable[crc^data[i]]
	}
	if nbit%8 != 0 {
		crc = crc<<4 ^ crcTable[crc>>4^data[i]>>4]
	}
	return crc
}

type bitReader struct {
	b    []byte
	idx  int    // next byte to load
	acc  uint32 // low nacc bits are unread
	nacc uint
}

// get returns n (0..16) bits, MSB first; bits past the end read as zero.
func (r *bitReader) get(n int) int {
	for r.nacc < uint(n) {
		var cur byte
		if r.idx < len(r.b) {
			cur = r.b[r.idx]
		}
		r.idx++
		r.acc = r.acc<<8 | uint32(cur)
		r.nacc += 8
	}
	r.nacc -= uint(n)
	return int(r.acc>>r.nacc) & (1<<uint(n) - 1)
}

var loudnessOffset4 = [4][4]int{
	{-1, 0, 0, 0}, {-2, 0, 0, 1}, {-2, 0, 0, 1}, {-2, 0, 0, 1},
}

var loudnessOffset8 = [4][8]int{
	{-2, 0, 0, 0, 0, 0, 0, 1},
	{-3, 0, 0, 0, 0, 0, 1, 2},
	{-4, 0, 0, 0, 0, 0, 1, 2},
	{-4, 0, 0, 0, 0, 0, 1, 2},
}

// computeNbits distributes the bitpool over nch channels of scale factors sf.
func computeNbits(fi *FrameInfo, sf []([8]int), nbits [][8]int, nch int) {
	var off [8]int
	freq := 0
	switch fi.SampleRate {
	case 32000:
		freq = 1
	case 44100:
		freq = 2
	case 48000:
		freq = 3
	}
	if fi.Subbands == 4 {
		copy(off[:], loudnessOffset4[freq][:])
	} else {
		off = loudnessOffset8[freq]
	}

	nsb := fi.Subbands
	var bitneeds [2][8]int
	maxNeed := 0
	for ch := 0; ch < nch; ch++ {
		for sb := 0; sb < nsb; sb++ {
			scf := sf[ch][sb]
			var bn int
			if fi.Loudness {
				if scf != 0 {
					bn = scf - off[sb]
				} else {
					bn = -5
				}
				if bn > 0 {
					bn >>= 1
				}
			} else {
				bn = scf
			}
			if bn > maxNeed {
				maxNeed = bn
			}
			bitneeds[ch][sb] = bn
		}
	}

	bitpool := fi.Bitpool
	bitcount := 0
	bitslice := maxNeed + 1
	for bc := 0; bc < bitpool; {
		bs := bitslice
		bitslice--
		bitcount = bc
		for ch := 0; ch < nch; ch++ {
			for sb := 0; sb < nsb; sb++ {
				bn := bitneeds[ch][sb]
				if bn >= bs && bn < bs+15 {
					bc++
				}
				if bn == bs {
					bc++
				}
			}
		}
	}

	for ch := 0; ch < nch; ch++ {
		for sb := 0; sb < nsb; sb++ {
			n := bitneeds[ch][sb] - bitslice
			if n < 2 {
				n = 0
			} else if n > 16 {
				n = 16
			}
			nbits[ch][sb] = n
		}
	}

	for sb := 0; sb < nsb && bitcount < bitpool; sb++ {
		for ch := 0; ch < nch && bitcount < bitpool; ch++ {
			n := 0
			if nbits[ch][sb] != 0 && nbits[ch][sb] < 16 {
				n = 1
			} else if bitneeds[ch][sb] == bitslice+1 && bitpool > bitcount+1 {
				n = 2
			}
			nbits[ch][sb] += n
			bitcount += n
		}
	}

	for sb := 0; sb < nsb && bitcount < bitpool; sb++ {
		for ch := 0; ch < nch && bitcount < bitpool; ch++ {
			if nbits[ch][sb] < 16 {
				nbits[ch][sb]++
				bitcount++
			}
		}
	}
}

var rangeScale = [16]int32{
	0xFFFFFFF, 0x5555556, 0x2492492, 0x1111111,
	0x0842108, 0x0410410, 0x0204081, 0x0101010,
	0x0080402, 0x0040100, 0x0020040, 0x0010010,
	0x0008004, 0x0004001, 0x0002000, 0x0001000,
}

// Decode decodes the single SBC frame at the start of frame and writes
// interleaved S16 samples (Blocks*Subbands per channel) into pcm, which must
// hold at least 2*16*8 = 256 samples. It returns the number of input bytes
// consumed (FrameLen), the number of int16 values written, and the header.
// Bad syncword or truncated input return an error with consumed 0 (the caller
// should drop the rest of the packet); a CRC mismatch returns ErrCRC with
// consumed = FrameLen so the caller can skip just that frame.
func (d *Decoder) Decode(frame []byte, pcm []int16) (consumed, n int, info FrameInfo, err error) {
	if len(frame) < headerSize {
		return 0, 0, info, ErrTruncated
	}
	fi, ok := parseHeader(frame)
	if !ok {
		return 0, 0, info, ErrSync
	}
	if len(frame) < fi.FrameLen {
		return 0, 0, info, ErrTruncated
	}
	if crc8(&fi, frame) != frame[3] {
		// The header parsed and the frame is fully present, so report its length:
		// the caller can skip it and keep decoding the rest of the packet.
		return fi.FrameLen, 0, info, ErrCRC
	}
	// Stream parameters changed: stale synthesis state would glitch.
	if cur := (FrameInfo{SampleRate: fi.SampleRate, Channels: fi.Channels, Mode: fi.Mode,
		Blocks: fi.Blocks, Subbands: fi.Subbands, Loudness: fi.Loudness}); cur != d.last {
		*d = Decoder{last: cur}
	}
	nch, nsb, nblk := fi.Channels, fi.Subbands, fi.Blocks
	if len(pcm) < nch*nsb*nblk {
		return 0, 0, info, errors.New("sbc: pcm buffer too small")
	}

	br := bitReader{b: frame[:fi.FrameLen], idx: headerSize}

	// Joint stereo flags.
	var mjoint uint
	if fi.Mode == JointStereo {
		if nsb == 4 {
			v := uint(br.get(4))
			mjoint = (v & 0x02 << 1) | (v & 0x04 >> 1) | (v & 0x08 >> 3)
		} else {
			v := uint(br.get(8))
			mjoint = (v & 0x02 << 5) | (v & 0x04 << 3) | (v & 0x08 << 1) |
				(v & 0x10 >> 1) | (v & 0x20 >> 3) | (v & 0x40 >> 5) | (v & 0x80 >> 7)
		}
	}

	var sf, nbits [2][8]int
	for ch := 0; ch < nch; ch++ {
		for sb := 0; sb < nsb; sb++ {
			sf[ch][sb] = br.get(4)
		}
	}
	if fi.Mode == DualChannel {
		computeNbits(&fi, sf[0:1], nbits[0:1], 1)
		computeNbits(&fi, sf[1:2], nbits[1:2], 1)
	} else if nch == 2 {
		computeNbits(&fi, sf[:], nbits[:], 2)
	} else {
		computeNbits(&fi, sf[:1], nbits[:1], 1)
	}

	var scale [2]int
	for ch := 0; ch < nch; ch++ {
		maxScf := 0
		for sb := 0; sb < nsb; sb++ {
			scf := sf[ch][sb] + int(mjoint>>uint(sb)&1)
			if scf > maxScf {
				maxScf = scf
			}
		}
		scale[ch] = 15 - maxScf - 1
	}
	if fi.Mode == JointStereo {
		scale[0] = min(scale[0], scale[1])
		scale[1] = scale[0]
	}

	var sbs [2][128]int16
	for blk := 0; blk < nblk; blk++ {
		for ch := 0; ch < nch; ch++ {
			p := sbs[ch][blk*nsb : blk*nsb+nsb]
			for sb := 0; sb < nsb; sb++ {
				nb := nbits[ch][sb]
				if nb == 0 {
					p[sb] = 0
					continue
				}
				s := int32(br.get(nb))
				s = (s<<1 | 1) * rangeScale[nb-1]
				p[sb] = int16((s - 1<<28) >> uint(28-(sf[ch][sb]+1+scale[ch])))
			}
		}
	}

	if fi.Mode == JointStereo {
		for sb := 0; sb < nsb; sb++ {
			if mjoint>>uint(sb)&1 == 0 {
				continue
			}
			for blk := 0; blk < nblk; blk++ {
				s0, s1 := sbs[0][blk*nsb+sb], sbs[1][blk*nsb+sb]
				sbs[0][blk*nsb+sb] = s0 + s1
				sbs[1][blk*nsb+sb] = s0 - s1
			}
		}
	}

	for ch := 0; ch < nch; ch++ {
		st := &d.st[ch]
		for blk := 0; blk < nblk; blk++ {
			in := sbs[ch][blk*nsb : blk*nsb+nsb]
			out := pcm[blk*nsb*nch+ch:]
			if nsb == 4 {
				synth4(st, in, scale[ch], out, nch)
			} else {
				synth8(st, in, scale[ch], out, nch)
			}
		}
	}
	return fi.FrameLen, nch * nsb * nblk, fi, nil
}

func sat16(v int32) int16 {
	if v > 32767 {
		return 32767
	}
	if v < -32768 {
		return -32768
	}
	return int16(v)
}

func dct4(in []int16, scale int, out0, out1 *[8][10]int16, idx int) {
	const c1, c2, c3 = 7568, 5793, 3135
	i0, i1, i2, i3 := int32(in[0]), int32(in[1]), int32(in[2]), int32(in[3])
	s03, d03 := int32(int16((i0+i3)>>1)), int32(int16((i0-i3)>>1))
	s12, d12 := int32(int16((i1+i2)>>1)), int32(int16((i1-i2)>>1))

	a0 := (s03 - s12) * c2
	b1 := -(s03 + s12) << 13
	a1 := d03*c3 - d12*c1
	b0 := -d03*c1 - d12*c3

	shr := uint(12 + scale)
	r := int32(1) << (shr - 1)
	a0 = (a0 + r) >> shr
	b0 = (b0 + r) >> shr
	a1 = (a1 + r) >> shr
	b1 = (b1 + r) >> shr

	out0[0][idx], out0[3][idx] = sat16(a0), sat16(-a1)
	out0[1][idx], out0[2][idx] = sat16(a1), 0
	out1[0][idx], out1[3][idx] = sat16(-a0), sat16(b0)
	out1[1][idx], out1[2][idx] = sat16(b0), sat16(b1)
}

func dct8(in []int16, scale int, out0, out1 *[8][10]int16, idx int) {
	const c1, c2, c3, c4, c5, c6, c7 = 8035, 7568, 6811, 5793, 4551, 3135, 1598
	// cos16[i] = 8192, 8035, 7568, 6811, 5793, 4551, 3135, 1598
	var x [8]int32
	for i := range x {
		x[i] = int32(in[i])
	}
	s07, d07 := int32(int16((x[0]+x[7])>>1)), int32(int16((x[0]-x[7])>>1))
	s16, d16 := int32(int16((x[1]+x[6])>>1)), int32(int16((x[1]-x[6])>>1))
	s25, d25 := int32(int16((x[2]+x[5])>>1)), int32(int16((x[2]-x[5])>>1))
	s34, d34 := int32(int16((x[3]+x[4])>>1)), int32(int16((x[3]-x[4])>>1))

	a0 := ((s07 + s34) - (s25 + s16)) * c4
	b3 := -((s07 + s34) + (s25 + s16)) << 13
	a2 := (s07-s34)*c6 + (s25-s16)*c2
	b1 := (s34-s07)*c2 + (s25-s16)*c6
	a1 := d07*c5 - d16*c1 + d25*c7 + d34*c3
	b2 := -d07*c1 - d16*c3 - d25*c5 - d34*c7
	a3 := d07*c7 - d16*c5 + d25*c3 - d34*c1
	b0 := -d07*c3 + d16*c7 + d25*c1 + d34*c5

	shr := uint(12 + scale)
	r := int32(1) << (shr - 1)
	a0 = (a0 + r) >> shr
	b0 = (b0 + r) >> shr
	a1 = (a1 + r) >> shr
	b1 = (b1 + r) >> shr
	a2 = (a2 + r) >> shr
	b2 = (b2 + r) >> shr
	a3 = (a3 + r) >> shr
	b3 = (b3 + r) >> shr

	out0[0][idx], out0[7][idx] = sat16(a0), sat16(-a1)
	out0[1][idx], out0[6][idx] = sat16(a1), sat16(-a2)
	out0[2][idx], out0[5][idx] = sat16(a2), sat16(-a3)
	out0[3][idx], out0[4][idx] = sat16(a3), 0

	out1[0][idx], out1[7][idx] = sat16(-a0), sat16(b0)
	out1[1][idx], out1[6][idx] = sat16(b0), sat16(b1)
	out1[2][idx], out1[5][idx] = sat16(b1), sat16(b2)
	out1[3][idx], out1[4][idx] = sat16(b2), sat16(b3)
}

func applyWindow(in *[8][10]int16, n int, window *[8][20]int16, offset int, out []int16, pitch int) {
	for i := 0; i < n; i++ {
		u := &in[i]
		w := window[i][offset : offset+10]
		var s int32
		for k := 0; k < 10; k++ {
			s += int32(u[k]) * int32(w[k])
		}
		out[i*pitch] = sat16((s + 1<<12) >> 13)
	}
}

func synth4(st *dstate, in []int16, scale int, out []int16, pitch int) {
	idx := 0
	if st.idx != 0 {
		idx = 10 - st.idx
	}
	odd := idx & 1
	dct4(in, scale, &st.v[odd], &st.v[1-odd], idx)
	applyWindow(&st.v[odd], 4, &window4, st.idx, out, pitch)
	if st.idx < 9 {
		st.idx++
	} else {
		st.idx = 0
	}
}

func synth8(st *dstate, in []int16, scale int, out []int16, pitch int) {
	idx := 0
	if st.idx != 0 {
		idx = 10 - st.idx
	}
	odd := idx & 1
	dct8(in, scale, &st.v[odd], &st.v[1-odd], idx)
	applyWindow(&st.v[odd], 8, &window8, st.idx, out, pitch)
	if st.idx < 9 {
		st.idx++
	} else {
		st.idx = 0
	}
}
