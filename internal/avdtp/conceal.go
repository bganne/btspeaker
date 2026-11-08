package avdtp

// fadeFrames is the length (in sample frames, i.e. per channel) of the ramp used
// to enter and leave concealed silence: ~0.7 ms at 44.1 kHz, enough to avoid a
// click, short enough not to be audible as a dip.
const fadeFrames = 32

// concealer tracks the RTP timeline of one stream and produces gap-filling
// silence with fades. All buffers are caller-owned; nothing here allocates
// beyond the caller's append growth.
type concealer struct {
	ch      int
	haveSeq bool
	seq     uint16 // last accepted sequence number
	haveTS  bool
	nextTS  uint32 // timestamp we expect the next packet to carry
	last    [2]int16
	fadeIn  int // sample frames of fade-in still to apply
}

func (c *concealer) reset(ch int) {
	*c = concealer{ch: ch, fadeIn: fadeFrames}
}

// silence appends frames sample frames of concealment to out: the last PCM
// value decays to zero over (up to) fadeFrames, then zeros; the next decoded
// audio is faded in.
func (c *concealer) silence(out []int16, frames int) []int16 {
	if frames <= 0 {
		return out
	}
	last := c.last
	if len(out) >= c.ch {
		copy(last[:], out[len(out)-c.ch:])
	}
	l := min(frames, fadeFrames)
	for i := 0; i < frames; i++ {
		for k := 0; k < c.ch; k++ {
			var v int16
			if i < l {
				v = int16(int32(last[k]) * int32(l-1-i) / int32(l))
			}
			out = append(out, v)
		}
	}
	c.last = [2]int16{}
	c.fadeIn = fadeFrames
	return out
}

// fade applies a pending fade-in to freshly decoded pcm and records its tail.
func (c *concealer) fade(pcm []int16) {
	for i := 0; c.fadeIn > 0 && i+c.ch <= len(pcm); i += c.ch {
		g := int32(fadeFrames - c.fadeIn + 1)
		for k := 0; k < c.ch; k++ {
			pcm[i+k] = int16(int32(pcm[i+k]) * g / (fadeFrames + 1))
		}
		c.fadeIn--
	}
	if len(pcm) >= c.ch {
		copy(c.last[:], pcm[len(pcm)-c.ch:])
	}
}
