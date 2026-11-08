// Package alsa is a pure-Go ALSA PCM playback client using kernel ioctls on
// /dev/snd (no alsa-lib: only hw: devices, no plug/dmix).
package alsa

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ErrBusy is returned by Open when another process holds the device (EBUSY).
var ErrBusy = errors.New("alsa: device busy")

type Format int

const (
	S16LE Format = iota
	S24LE        // 24 bits in the low 3 bytes of a 4-byte little-endian word
	S32LE
)

// BytesPerSample is the container size of one sample.
func (f Format) BytesPerSample() int {
	if f == S16LE {
		return 2
	}
	return 4
}

func (f Format) String() string {
	switch f {
	case S16LE:
		return "S16_LE"
	case S24LE:
		return "S24_LE"
	case S32LE:
		return "S32_LE"
	}
	return "Format(" + strconv.Itoa(int(f)) + ")"
}

// kernel format numbers (SNDRV_PCM_FORMAT_*)
func (f Format) kernel() uint {
	switch f {
	case S16LE:
		return 2
	case S24LE:
		return 6
	default:
		return 10
	}
}

// Caps is what the hw device accepts.
type Caps struct {
	Rates       []int // among 16000,32000,44100,48000 (the SBC rates)
	Formats     []Format
	MinChannels int
	MaxChannels int
}

// HasFormat reports whether f is supported.
func (c Caps) HasFormat(f Format) bool {
	for _, x := range c.Formats {
		if x == f {
			return true
		}
	}
	return false
}

// HasRate reports whether r is supported.
func (c Caps) HasRate(r int) bool {
	for _, x := range c.Rates {
		if x == r {
			return true
		}
	}
	return false
}

// Card is one playback PCM for -list-devices.
type Card struct {
	Device string // "hw:N,D"
	Name   string // card id e.g. "Spdif"
	Desc   string
}

// Overridable for tests.
var (
	procRoot = "/proc/asound"
	devRoot  = "/dev/snd"
)

var cardLine = regexp.MustCompile(`^\s*(\d+)\s+\[([^\]]*?)\s*\]:\s*(.*)$`)

type cardInfo struct {
	num      int
	id, desc string
}

func readCards() ([]cardInfo, error) {
	b, err := os.ReadFile(filepath.Join(procRoot, "cards"))
	if err != nil {
		return nil, err
	}
	var cards []cardInfo
	lines := strings.Split(string(b), "\n")
	for i, l := range lines {
		m := cardLine.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		c := cardInfo{num: n, id: m[2], desc: m[3]}
		if i+1 < len(lines) && cardLine.FindStringSubmatch(lines[i+1]) == nil {
			if s := strings.TrimSpace(lines[i+1]); s != "" {
				c.desc += " (" + s + ")"
			}
		}
		cards = append(cards, c)
	}
	return cards, nil
}

func cardNumber(card string) (int, error) {
	if n, err := strconv.Atoi(card); err == nil && n >= 0 {
		return n, nil
	}
	cards, err := readCards()
	if err != nil {
		return 0, err
	}
	for _, c := range cards {
		if c.id == card {
			return c.num, nil
		}
	}
	return 0, fmt.Errorf("alsa: no card with id %q", card)
}

// ParseDevice resolves "hw:CARD,DEV" (CARD numeric or card id from /proc/asound/cards) to a /dev/snd path.
// Also accepts "hw:CARD=ID,DEV=D" and "hw:CARD" (device 0).
func ParseDevice(dev string) (path string, err error) {
	rest, ok := strings.CutPrefix(dev, "hw:")
	if !ok {
		return "", fmt.Errorf("alsa: unsupported device %q (only hw:CARD,DEV)", dev)
	}
	card, sub, _ := strings.Cut(rest, ",")
	card = strings.TrimPrefix(card, "CARD=")
	sub = strings.TrimPrefix(sub, "DEV=")
	d := 0
	if sub != "" {
		d, err = strconv.Atoi(sub)
		if err != nil || d < 0 {
			return "", fmt.Errorf("alsa: bad device number in %q", dev)
		}
	}
	if card == "" {
		return "", fmt.Errorf("alsa: bad device %q", dev)
	}
	n, err := cardNumber(card)
	if err != nil {
		return "", err
	}
	return filepath.Join(devRoot, fmt.Sprintf("pcmC%dD%dp", n, d)), nil
}

func ListPlayback() ([]Card, error) {
	cards, err := readCards()
	if err != nil {
		return nil, err
	}
	var out []Card
	for _, c := range cards {
		dir := filepath.Join(procRoot, fmt.Sprintf("card%d", c.num))
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			var d int
			if n, _ := fmt.Sscanf(e.Name(), "pcm%dp", &d); n != 1 || e.Name() != fmt.Sprintf("pcm%dp", d) {
				continue
			}
			desc := c.desc
			if b, err := os.ReadFile(filepath.Join(dir, e.Name(), "info")); err == nil {
				for _, l := range strings.Split(string(b), "\n") {
					if v, ok := strings.CutPrefix(l, "name: "); ok && v != "" {
						desc = v + " - " + c.desc
					}
				}
			}
			out = append(out, Card{Device: fmt.Sprintf("hw:%d,%d", c.num, d), Name: c.id, Desc: desc})
		}
	}
	return out, nil
}

// ---- kernel structures (include/uapi/sound/asound.h) ----

const (
	paramAccess    = 0
	paramFormat    = 1
	paramSubformat = 2
	paramFirstIvl  = 8 // SAMPLE_BITS
	paramChannels  = 10
	paramRate      = 11
	paramPeriodSz  = 13
	paramBufferSz  = 17
	paramLastIvl   = 19 // TICK_TIME

	accessRWInterleaved = 3
	subformatStd        = 0
	hwParamsNoResample  = 1 << 0
)

type mask struct{ bits [8]uint32 } // SNDRV_MASK_MAX 256

func (m *mask) any() {
	for i := range m.bits {
		m.bits[i] = ^uint32(0)
	}
}
func (m *mask) only(bit uint) {
	*m = mask{}
	m.bits[bit>>5] = 1 << (bit & 31)
}
func (m *mask) test(bit uint) bool { return m.bits[bit>>5]&(1<<(bit&31)) != 0 }

// interval mirrors struct snd_interval; flags holds the bitfield
// (openmin:1, openmax:1, integer:1, empty:1, little-endian bit order).
type interval struct {
	min, max uint32
	flags    uint32
}

const (
	ivlInteger = 1 << 2
	ivlEmpty   = 1 << 3
)

func (i *interval) any()                { *i = interval{min: 0, max: ^uint32(0)} }
func (i *interval) set(min, max uint32) { *i = interval{min: min, max: max, flags: ivlInteger} }
func (i *interval) empty() bool         { return i.flags&ivlEmpty != 0 || i.min > i.max }

type hwParams struct {
	flags     uint32
	masks     [3]mask
	mres      [5]mask
	intervals [paramLastIvl - paramFirstIvl + 1]interval
	ires      [9]interval
	rmask     uint32
	cmask     uint32
	info      uint32
	msbits    uint32
	rateNum   uint32
	rateDen   uint32
	fifoSize  uintptr // snd_pcm_uframes_t
	sync      [16]byte
	reserved  [48]byte
}

func (h *hwParams) ivl(p int) *interval { return &h.intervals[p-paramFirstIvl] }

func (h *hwParams) setAny() {
	*h = hwParams{}
	for i := range h.masks {
		h.masks[i].any()
	}
	for i := range h.intervals {
		h.intervals[i].any()
	}
	h.rmask = ^uint32(0)
}

type swParams struct {
	tstampMode       int32
	periodStep       uint32
	sleepMin         uint32
	availMin         uintptr
	xferAlign        uintptr
	startThreshold   uintptr
	stopThreshold    uintptr
	silenceThreshold uintptr
	silenceSize      uintptr
	boundary         uintptr
	proto            uint32
	tstampType       uint32
	reserved         [56]byte
}

type xferi struct {
	result uintptr // snd_pcm_sframes_t (long)
	buf    unsafe.Pointer
	frames uintptr
}

// _IOC encoding (x86/arm/arm64 layout).
func ioc(dir, nr, size uintptr) uintptr { return dir<<30 | size<<16 | 'A'<<8 | nr }

const (
	iocWrite = 1
	iocRead  = 2
)

var (
	ioctlPVersion    = ioc(iocRead, 0x00, 4)
	ioctlHWRefine    = ioc(iocRead|iocWrite, 0x10, unsafe.Sizeof(hwParams{}))
	ioctlHWParams    = ioc(iocRead|iocWrite, 0x11, unsafe.Sizeof(hwParams{}))
	ioctlSWParams    = ioc(iocRead|iocWrite, 0x13, unsafe.Sizeof(swParams{}))
	ioctlPrepare     = ioc(0, 0x40, 0)
	ioctlStart       = ioc(0, 0x42, 0)
	ioctlDrop        = ioc(0, 0x43, 0)
	ioctlDrain       = ioc(0, 0x44, 0)
	ioctlResume      = ioc(0, 0x47, 0)
	ioctlWriteFrames = ioc(iocWrite, 0x50, unsafe.Sizeof(xferi{}))
)

// ioctl is a variable so tests can fake the kernel.
var ioctl = sysIoctl

func sysIoctl(fd int, req uintptr, arg unsafe.Pointer) error {
	_, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
	if e != 0 {
		return e
	}
	return nil
}

func openNode(path string) (int, error) {
	// Non-blocking open so a held device fails with EBUSY immediately.
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		if err == unix.EBUSY {
			return -1, ErrBusy
		}
		return -1, fmt.Errorf("alsa: open %s: %w", path, err)
	}
	fl, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err == nil {
		_, err = unix.FcntlInt(uintptr(fd), unix.F_SETFL, fl&^unix.O_NONBLOCK)
	}
	if err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("alsa: clear O_NONBLOCK: %w", err)
	}
	var ver int32
	if err := ioctl(fd, ioctlPVersion, unsafe.Pointer(&ver)); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("alsa: PVERSION: %w", err)
	}
	if ver>>16 != 2 {
		unix.Close(fd)
		return -1, fmt.Errorf("alsa: incompatible PCM protocol version %#x", ver)
	}
	return fd, nil
}

// realBits is a best-effort probe-time check: it reports whether the SAMPLE_BITS
// interval refined with f fixed leaves more than 16 significant bits for a wide
// format. Many drivers do not narrow it until HW_PARAMS, so the authoritative check is
// PCM.MSBits after Open (the player falls back to S16 with dither).
func realBits(h *hwParams, f Format) bool {
	if f == S16LE {
		return true
	}
	sb := h.ivl(paramFirstIvl)
	return sb.empty() || sb.max > 16
}

func refine(fd int, h *hwParams) error {
	h.rmask = ^uint32(0)
	h.cmask = 0
	if err := ioctl(fd, ioctlHWRefine, unsafe.Pointer(h)); err != nil {
		return err
	}
	return nil
}

// Probe opens the device briefly and returns its capabilities (HW_REFINE).
func Probe(dev string) (Caps, error) {
	path, err := ParseDevice(dev)
	if err != nil {
		return Caps{}, err
	}
	fd, err := openNode(path)
	if err != nil {
		return Caps{}, err
	}
	defer unix.Close(fd)
	return probeFD(fd)
}

func probeFD(fd int) (Caps, error) {
	var base hwParams
	base.setAny()
	base.masks[paramAccess].only(accessRWInterleaved)
	base.masks[paramSubformat].only(subformatStd)
	var c Caps
	h := base
	if err := refine(fd, &h); err != nil {
		return c, fmt.Errorf("alsa: HW_REFINE: %w", err)
	}
	ch := h.ivl(paramChannels)
	c.MinChannels, c.MaxChannels = int(ch.min), int(ch.max)
	for _, r := range []int{16000, 32000, 44100, 48000} {
		h := base
		h.ivl(paramRate).set(uint32(r), uint32(r))
		if refine(fd, &h) == nil && !h.ivl(paramRate).empty() {
			c.Rates = append(c.Rates, r)
		}
	}
	for _, f := range []Format{S16LE, S32LE, S24LE} {
		h := base
		h.masks[paramFormat].only(f.kernel())
		if refine(fd, &h) == nil && h.masks[paramFormat].test(f.kernel()) && realBits(&h, f) {
			c.Formats = append(c.Formats, f)
		}
	}
	return c, nil
}

// PCM is an open playback stream.
//
// Write, Drain and Close must be called from one goroutine (or serialised by the
// caller); Abort alone may be called concurrently with them.
type PCM struct {
	mu         sync.Mutex // guards fd against Abort/Close racing (fd number reuse)
	fd         int
	frameBytes int
	Rate       int
	Channels   int
	Format     Format
	Buffer     int // negotiated buffer size, frames
	Period     int // negotiated period size, frames
	xruns      atomic.Int64
	msbits     int
	afterXrun  func(rest []byte)
	silence    []byte      // zeroed scratch for writeSilence
	needStart  bool        // set after xrun PREPARE; the next successful write issues START
	startThr   int         // sw start_threshold, frames: silence cushion written after an xrun
	aborted    atomic.Bool // set by Abort; a PCM is single-use, so never reset
}

var errAborted = errors.New("alsa: aborted")

// MSBits returns the number of significant bits per sample the driver reported after
// HW_PARAMS, or 0 if unknown.
func (p *PCM) MSBits() int { return p.msbits }

// Xruns returns the number of underruns recovered by Write.
func (p *PCM) Xruns() int64 { return p.xruns.Load() }

func clamp(v, lo, hi uint32) uint32 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// Open configures the device for interleaved playback. Buffer is a target in frames.
func Open(dev string, rate, channels int, f Format, bufferFrames int) (*PCM, error) {
	path, err := ParseDevice(dev)
	if err != nil {
		return nil, err
	}
	fd, err := openNode(path)
	if err != nil {
		return nil, err
	}
	p, err := configure(fd, rate, channels, f, bufferFrames)
	if err != nil {
		unix.Close(fd)
		return nil, err
	}
	return p, nil
}

func configure(fd, rate, channels int, f Format, bufferFrames int) (*PCM, error) {
	var h hwParams
	h.setAny()
	h.flags = hwParamsNoResample
	h.masks[paramAccess].only(accessRWInterleaved)
	h.masks[paramFormat].only(f.kernel())
	h.masks[paramSubformat].only(subformatStd)
	h.ivl(paramChannels).set(uint32(channels), uint32(channels))
	h.ivl(paramRate).set(uint32(rate), uint32(rate))
	if err := refine(fd, &h); err != nil {
		return nil, fmt.Errorf("alsa: %d Hz %d ch %v not supported: %w", rate, channels, f, err)
	}
	// Buffer near the target, then period ~ buffer/4; each step falls back to the
	// unrestricted range if the exact value cannot be satisfied.
	narrow := func(param int, target uint32) {
		iv := h.ivl(param)
		t := h
		t.ivl(param).set(clamp(target, iv.min, iv.max), clamp(target, iv.min, iv.max))
		if refine(fd, &t) == nil && !t.ivl(param).empty() {
			h = t
		}
	}
	narrow(paramBufferSz, uint32(bufferFrames))
	narrow(paramPeriodSz, h.ivl(paramBufferSz).max/4)
	h.rmask = ^uint32(0) // refine cleared it; like alsa-lib, ask HW_PARAMS to refine everything
	if err := ioctl(fd, ioctlHWParams, unsafe.Pointer(&h)); err != nil {
		return nil, fmt.Errorf("alsa: HW_PARAMS: %w", err)
	}
	msbits := int(h.msbits)
	buffer := h.ivl(paramBufferSz).min
	period := h.ivl(paramPeriodSz).min

	sw := swParams{
		tstampMode:     0,
		periodStep:     1,
		availMin:       uintptr(period),
		startThreshold: uintptr(buffer / 2),
		stopThreshold:  uintptr(buffer),
		proto:          2<<16 | 0<<8 | 18,
	}
	if sw.startThreshold == 0 {
		sw.startThreshold = 1
	}
	// Boundary: largest multiple of the buffer size that fits in a signed long.
	boundary := uintptr(buffer)
	for boundary*2 <= uintptr(1)<<(unsafe.Sizeof(uintptr(0))*8-1)-1 {
		boundary *= 2
	}
	sw.boundary = boundary
	if err := ioctl(fd, ioctlSWParams, unsafe.Pointer(&sw)); err != nil {
		return nil, fmt.Errorf("alsa: SW_PARAMS: %w", err)
	}
	if err := ioctl(fd, ioctlPrepare, nil); err != nil {
		return nil, fmt.Errorf("alsa: PREPARE: %w", err)
	}
	return &PCM{
		fd:         fd,
		frameBytes: channels * f.BytesPerSample(),
		Rate:       rate,
		Channels:   channels,
		Format:     f,
		Buffer:     int(buffer),
		Period:     int(period),
		msbits:     msbits,
		startThr:   int(sw.startThreshold),
	}, nil
}

// SetAfterXrun sets a hook that Write calls after recovering from an xrun, with the
// not yet written remainder of its buffer, which the hook may modify in place (e.g.
// to fade it in). Not safe to call concurrently with Write.
func (p *PCM) SetAfterXrun(f func(rest []byte)) { p.afterXrun = f }

// maxXrunRecoveries bounds the xrun recoveries of one Write (guards a broken driver).
const maxXrunRecoveries = 5

// Write writes interleaved frames (len(buf) = frames*channels*bytesPerSample), recovering from xruns.
func (p *PCM) Write(buf []byte) error {
	if p.fd < 0 {
		return errors.New("alsa: closed")
	}
	recoveries := 0
	for len(buf) >= p.frameBytes {
		if p.aborted.Load() {
			return errAborted
		}
		x := xferi{buf: unsafe.Pointer(&buf[0]), frames: uintptr(len(buf) / p.frameBytes)}
		err := ioctl(p.fd, ioctlWriteFrames, unsafe.Pointer(&x))
		n := int(x.result)
		if n > 0 {
			buf = buf[n*p.frameBytes:]
		}
		if n > 0 && p.needStart && (err == nil || err == unix.EINTR) {
			// After PREPARE the stream waits for start_threshold; data only arrives
			// in realtime, so start now. Already running (EBADFD) etc. is harmless.
			p.needStart = false
			_ = ioctl(p.fd, ioctlStart, nil)
		}
		switch err {
		case nil:
			if n <= 0 { // the kernel never does this on a blocking fd; don't spin
				return errors.New("alsa: write: no progress")
			}
		case unix.EINTR:
		case unix.EPIPE: // xrun
			p.xruns.Add(1)
			if p.aborted.Load() { // a concurrent Abort must not be undone by PREPARE
				return errAborted
			}
			if recoveries++; recoveries > maxXrunRecoveries {
				return errors.New("alsa: write: repeated xruns")
			}
			if err := ioctl(p.fd, ioctlPrepare, nil); err != nil {
				return fmt.Errorf("alsa: xrun recovery: %w", err)
			}
			p.needStart = true
			// Restore the ~start_threshold cushion a normal start has: with
			// realtime input the buffer never refills, so without it the next
			// jitter would xrun again. This may auto-start the stream.
			p.writeSilence(p.startThr)
			if p.afterXrun != nil && len(buf) > 0 {
				p.afterXrun(buf)
			}
		case unix.ESTRPIPE: // suspended
			if err := p.resume(); err != nil {
				return err
			}
		default:
			if p.aborted.Load() {
				return errAborted
			}
			return fmt.Errorf("alsa: write: %w", err)
		}
	}
	return nil
}

// writeSilence queues frames of zeros (silence for all supported formats), best effort.
func (p *PCM) writeSilence(frames int) {
	if frames <= 0 {
		return
	}
	if need := frames * p.frameBytes; len(p.silence) < need {
		p.silence = make([]byte, need) // zeroed; reused by later recoveries
	}
	z := p.silence[:frames*p.frameBytes]
	for len(z) >= p.frameBytes && !p.aborted.Load() {
		x := xferi{buf: unsafe.Pointer(&z[0]), frames: uintptr(len(z) / p.frameBytes)}
		err := ioctl(p.fd, ioctlWriteFrames, unsafe.Pointer(&x))
		n := int(x.result)
		if n > 0 {
			z = z[n*p.frameBytes:]
		}
		if (err != nil && err != unix.EINTR) || (err == nil && n <= 0) {
			return
		}
	}
}

func (p *PCM) resume() error {
	for i := 0; i < 50; i++ {
		err := ioctl(p.fd, ioctlResume, nil)
		if err == unix.EAGAIN || err == unix.EINTR {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		if err == nil {
			return nil
		}
		break
	}
	if err := ioctl(p.fd, ioctlPrepare, nil); err != nil {
		return fmt.Errorf("alsa: resume: %w", err)
	}
	return nil
}

// Drain blocks until queued frames have been played.
func (p *PCM) Drain() error {
	if p.fd < 0 {
		return nil
	}
	for {
		err := ioctl(p.fd, ioctlDrain, nil)
		switch err {
		case nil, unix.EPIPE:
			return nil
		case unix.EINTR:
			continue
		}
		return fmt.Errorf("alsa: drain: %w", err)
	}
}

// Abort drops pending frames (SNDRV_PCM_IOCTL_DROP) and wakes a Write or Drain blocked
// in the kernel (e.g. S/PDIF without a clock), which then fail. Safe to call from any
// goroutine, any number of times; it never closes the fd. After Abort, Write fails (a PCM is
// single-use: the player opens a new one for the next session).
func (p *PCM) Abort() error {
	p.aborted.Store(true) // before DROP, so a Write that sees EPIPE can't PREPARE over it
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fd < 0 {
		return nil
	}
	if err := ioctl(p.fd, ioctlDrop, nil); err != nil {
		return fmt.Errorf("alsa: drop: %w", err)
	}
	return nil
}

func (p *PCM) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fd < 0 {
		return nil
	}
	fd := p.fd
	p.fd = -1
	return unix.Close(fd)
}
