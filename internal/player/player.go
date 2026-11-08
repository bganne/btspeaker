// Package player turns decoded PCM into ALSA output, holding the hw device only
// while non-silent audio flows (see CLAUDE.md "mpd coexistence").
//
// The device is acquired lazily on the first non-silent chunk and released (DRAIN +
// close) on Stop, when Write has not been called for 1 s, or after cfg.IdleRelease of
// continuous digital silence. While the device is busy or fails to open, audio is
// dropped and the open is retried at most every 3 s.
package player

import (
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"btspeaker/internal/alsa"
)

type Config struct {
	Device      string        // "hw:CARD,DEV"
	Buffer      time.Duration // ALSA buffer target, e.g. 200ms
	IdleRelease time.Duration // close device after this much digital silence, e.g. 5s
	SoftVolume  bool          // apply AVRCP volume as gain; false = bit-perfect
	Preroll     time.Duration // zeros written after each fresh open so a S/PDIF receiver can relock; 0 = none
}

type Status struct {
	Open      bool // device currently held
	Rate      int
	Channels  int
	Volume    uint8  // 0..127 (AVRCP scale)
	LastError string // e.g. "ALSA device busy (mpd playing?)"
	Xruns     int64  // ALSA underruns recovered since start (clock drift / starvation indicator)
	QueueDrop int64  // frames dropped since start because more than maxQueue of audio was queued
}

// PCM is the subset of *alsa.PCM the player uses.
type PCM interface {
	Write([]byte) error
	Drain() error
	// Abort drops pending frames and wakes a Write/Drain blocked in the driver. It may
	// be called concurrently with Write/Drain/Close.
	Abort() error
	Close() error
}

// xrunCounter is optionally implemented by a PCM (*alsa.PCM does).
type xrunCounter interface{ Xruns() int64 }

// msbitsReporter is optionally implemented by a PCM (*alsa.PCM does): the significant
// bits per sample the driver reported (0 = unknown).
type msbitsReporter interface{ MSBits() int }

// xrunHooker is optionally implemented by a PCM (*alsa.PCM does): the hook gets the
// unwritten remainder of a Write buffer after an xrun recovery, to fade it in.
type xrunHooker interface{ SetAfterXrun(func(rest []byte)) }

// Backend abstracts the ALSA device so tests can use a fake.
type Backend interface {
	Probe(dev string) (alsa.Caps, error)
	Open(dev string, rate, channels int, f alsa.Format, bufferFrames int) (PCM, error)
}

type alsaBackend struct{}

func (alsaBackend) Probe(dev string) (alsa.Caps, error) { return alsa.Probe(dev) }
func (alsaBackend) Open(dev string, rate, ch int, f alsa.Format, n int) (PCM, error) {
	p, err := alsa.Open(dev, rate, ch, f, n)
	if err != nil {
		return nil, err
	}
	return p, nil
}

const (
	defaultVolume  = 100                    // ≈ -10.6 dB until the remote sends SetAbsoluteVolume
	queueChunks    = 64                     // channel capacity; the real bound is maxQueue of audio
	maxQueue       = 100 * time.Millisecond // queued audio before the oldest chunks are dropped; > alsa.MaxPeriod (a write blocks up to a period)
	freeChunks     = 32                     // recycled byte buffers
	busyMessage    = "ALSA device busy (mpd playing?)"
	maxVolumeDB    = 0.0
	minVolumeDB    = -50.0
	unity          = 1 << 15 // Q15 gain of 1.0
	closeWaitLimit = 3 * time.Second
	defaultBuffer  = 200 * time.Millisecond
	defaultIdle    = 5 * time.Second
)

// gainTable maps AVRCP volume 0..127 to a Q15 linear gain. 0 is mute, 127 is 0 dB and
// everything between is linear in dB from -50 dB (v=1) to 0 dB, i.e.
// dB = -50*(1-v/127): v=64 ≈ -25 dB, v=100 ≈ -10.6 dB.
var gainTable = func() (t [128]int32) {
	for v := 1; v < 128; v++ {
		db := minVolumeDB * (1 - float64(v)/127)
		t[v] = int32(math.Round(unity * math.Pow(10, db/20)))
	}
	t[127] = unity
	return
}()

// session is one open device plus its writer goroutine.
type session struct {
	pcm  PCM
	q    chan []byte
	done chan struct{}
	// queued frames (written by Write, drained by run), bounding latency
	queued atomic.Int64
	// UnixNano of the start of the in-progress pcm.Write, 0 when idle
	busy    atomic.Int64
	aborted atomic.Bool // skip pending writes and the final drain
	refade  atomic.Bool // Write dropped queued audio: restart the fade-in in run
	inCh    int
	outCh   int
	fmt     alsa.Format
	conv    converter // used by Write under Player.mu
	xruns   int64     // last Xruns() seen by run
	fadePos int       // xrun fade-in progress in frames (fadeN = none); run only
	fadeN   int       // fade length in frames (fadeLen of the rate)
	last    []byte    // last frame written, for fade-outs; run only
	preroll []byte    // zeros written before the first chunk; run only
}

type Player struct {
	cfg     Config
	backend Backend

	retryEvery time.Duration // between open attempts (3 s)
	noWrite    time.Duration // release when Write is silent this long (1 s)

	mu        sync.Mutex
	rate      int
	channels  int
	volume    uint8
	lastErr   string
	sess      *session
	closing   *session  // released session whose goroutine may still be draining
	closedAt  time.Time // when closing was released
	free      chan []byte
	nextTry   time.Time
	lastWrite time.Time
	silentFor time.Duration
	caps      *alsa.Caps
	narrow16  bool // device reported msbits ≤16: use S16LE even with soft volume
	wd        *time.Timer
	closed    bool
	xruns     atomic.Int64
	queueDrop atomic.Int64 // frames dropped on queue overflow
}

func New(cfg Config) *Player { return NewWithBackend(cfg, alsaBackend{}) }

func NewWithBackend(cfg Config, b Backend) *Player {
	if cfg.Buffer <= 0 {
		cfg.Buffer = defaultBuffer
	}
	if cfg.IdleRelease <= 0 {
		cfg.IdleRelease = defaultIdle
	}
	p := &Player{cfg: cfg, backend: b, volume: defaultVolume, free: make(chan []byte, freeChunks), retryEvery: 3 * time.Second, noWrite: time.Second}
	p.wd = time.AfterFunc(time.Hour, p.watchdog)
	p.wd.Stop()
	return p
}

// Start announces a new stream format (AVDTP START). Does not open the device yet.
func (p *Player) Start(rate, channels int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sess != nil && (p.rate != rate || p.channels != channels) {
		p.releaseLocked()
	}
	p.rate, p.channels = rate, channels
	p.silentFor = 0
	p.nextTry = time.Time{}
}

func isSilent(pcm []int16) bool {
	for _, s := range pcm {
		if s > 1 || s < -1 {
			return false
		}
	}
	return true
}

// Write queues interleaved S16 PCM in the format given to Start. Never blocks for
// long; drops audio while the device is busy/unavailable.
func (p *Player) Write(pcm []int16) {
	if len(pcm) == 0 {
		return
	}
	p.mu.Lock()
	if p.closed || p.rate <= 0 || p.channels <= 0 {
		p.mu.Unlock()
		return
	}
	now := time.Now()
	p.lastWrite = now
	silent := isSilent(pcm)
	if silent {
		p.silentFor += time.Duration(len(pcm)/p.channels) * time.Second / time.Duration(p.rate)
	} else {
		p.silentFor = 0
	}

	if p.sess == nil {
		if silent || now.Before(p.nextTry) {
			p.mu.Unlock()
			return
		}
		if prev := p.closing; prev != nil {
			// Our own previous session must finish releasing the device. Don't wait
			// for it (the caller may hold AVDTP signalling locks): drop this chunk,
			// the next one retries. releaseLocked aborts a hung drain after
			// closeWaitLimit; if even that does not free it, open anyway.
			select {
			case <-prev.done:
			default:
				if now.Sub(p.closedAt) < 2*closeWaitLimit {
					p.mu.Unlock()
					return
				}
			}
		}
		if !p.openLocked(now) {
			p.mu.Unlock()
			return
		}
	}
	s := p.sess
	var gain int32 = unity
	if p.cfg.SoftVolume {
		gain = gainTable[p.volume]
	}
	if t := s.busy.Load(); t != 0 && now.UnixNano()-t > int64(closeWaitLimit) {
		// The driver has not accepted a write for seconds (e.g. S/PDIF without clock).
		p.lastErr = "ALSA write stalled"
		p.nextTry = now.Add(p.retryEvery)
		p.abortLocked(s)
		p.releaseLocked()
		p.mu.Unlock()
		return
	}
	frames := int64(len(pcm) / s.inCh)
	limit := int64(maxQueue) * int64(p.rate) / int64(time.Second)
	for s.queued.Load()+frames > limit { // bound latency: drop the oldest chunks
		select {
		case old := <-s.q:
			n := int64(len(old) / (s.outCh * s.fmt.BytesPerSample()))
			s.queued.Add(-n)
			p.queueDrop.Add(n)
			s.refade.Store(true)
			p.putBuf(old)
			continue
		default:
		}
		break
	}
	buf := p.getBuf(len(pcm) / s.inCh * s.outCh * s.fmt.BytesPerSample())
	s.conv.convertSilent(buf, pcm, s.inCh, s.outCh, s.fmt, gain, p.rate, silent)
	s.queued.Add(frames)
	select {
	case s.q <- buf:
	default: // channel full (many tiny chunks): drop the new one
		s.queued.Add(-frames)
		p.queueDrop.Add(frames)
		p.putBuf(buf)
	}
	if silent && p.silentFor >= p.cfg.IdleRelease {
		// Only silence is queued: drop it rather than drain, so the device is
		// freed at once and the start of the next track is not lost.
		p.abortLocked(s)
		p.releaseLocked()
	} else {
		p.wd.Reset(p.noWrite)
	}
	p.mu.Unlock()
}

// maxChunkBytes is the largest converted chunk: 15 SBC frames x 128 samples x
// 2 channels x 4 bytes. Pooled buffers are all this size so none is discarded.
const maxChunkBytes = 15 * 128 * 2 * 4

func (p *Player) getBuf(n int) []byte {
	select {
	case b := <-p.free:
		if cap(b) >= n {
			return b[:n]
		}
	default:
	}
	if n > maxChunkBytes {
		return make([]byte, n)
	}
	return make([]byte, n, maxChunkBytes)
}

func (p *Player) putBuf(b []byte) {
	select {
	case p.free <- b:
	default:
	}
}

// openLocked tries to acquire the device; on failure sets lastErr and the retry time.
func (p *Player) openLocked(now time.Time) bool {
	fail := func(err error) bool {
		if errors.Is(err, alsa.ErrBusy) {
			p.lastErr = busyMessage
		} else {
			p.lastErr = err.Error()
			p.caps, p.narrow16 = nil, false // device may have been replugged: re-probe next time
		}
		p.nextTry = now.Add(p.retryEvery)
		return false
	}
	if p.caps == nil {
		c, err := p.backend.Probe(p.cfg.Device)
		if err != nil {
			return fail(err)
		}
		p.caps = &c
	}
	f, outCh, err := Negotiate(*p.caps, p.rate, p.channels, p.cfg.SoftVolume)
	if err != nil {
		return fail(err)
	}
	if p.narrow16 && f != alsa.S16LE && p.caps.HasFormat(alsa.S16LE) {
		f = alsa.S16LE // msbits already found ≤16: skip the wide open/close cycle
	}
	frames := int(int64(p.cfg.Buffer) * int64(p.rate) / int64(time.Second))
	pcm, err := p.backend.Open(p.cfg.Device, p.rate, outCh, f, frames)
	if err != nil {
		return fail(err)
	}
	if mb, ok := pcm.(msbitsReporter); ok && p.cfg.SoftVolume && f != alsa.S16LE && p.caps.HasFormat(alsa.S16LE) {
		if n := mb.MSBits(); n > 0 && n <= 16 {
			// The driver only has 16 effective bits: wide samples would truncate the
			// attenuated audio without dither, so use S16 (dithered) instead.
			pcm.Close()
			p.narrow16 = true
			f = alsa.S16LE
			if pcm, err = p.backend.Open(p.cfg.Device, p.rate, outCh, f, frames); err != nil {
				return fail(err)
			}
		}
	}
	p.lastErr = ""
	s := &session{pcm: pcm, q: make(chan []byte, queueChunks), done: make(chan struct{}), inCh: p.channels, outCh: outCh, fmt: f, fadePos: 0, fadeN: fadeLen(p.rate)}
	if p.cfg.Preroll > 0 {
		s.preroll = make([]byte, int(int64(p.cfg.Preroll)*int64(p.rate)/int64(time.Second))*outCh*f.BytesPerSample())
	}
	if h, ok := pcm.(xrunHooker); ok {
		// Called by pcm.Write in run: the discontinuity is inside the chunk being
		// written, so fade its remainder in and carry the fade into later chunks.
		h.SetAfterXrun(func(rest []byte) { s.fadePos = fadeIn(rest, f, outCh, 0, s.fadeN) })
	}
	p.sess = s
	p.closing = nil
	go p.run(s)
	return true
}

// Negotiate picks the hw sample format and channel count. With soft volume the
// gain output needs headroom beyond 16 bits, so S32, S24, S16 are preferred in that
// order; otherwise S16 (bit-exact), S32, S24.
func Negotiate(c alsa.Caps, rate, ch int, soft bool) (alsa.Format, int, error) {
	if !c.HasRate(rate) {
		return 0, 0, errors.New("sample rate not supported by device")
	}
	out := ch
	if out < c.MinChannels {
		if ch == 1 && c.MinChannels == 2 {
			out = 2
		} else {
			return 0, 0, errors.New("channel count not supported by device")
		}
	}
	if out > c.MaxChannels {
		return 0, 0, errors.New("channel count not supported by device")
	}
	order := []alsa.Format{alsa.S16LE, alsa.S32LE, alsa.S24LE}
	if soft {
		order = []alsa.Format{alsa.S32LE, alsa.S24LE, alsa.S16LE}
	}
	for _, f := range order {
		if c.HasFormat(f) {
			return f, out, nil
		}
	}
	return 0, 0, errors.New("no supported sample format")
}

// run is the writer goroutine: it owns the PCM until the queue is closed.
func (p *Player) run(s *session) {
	failed := false
	if s.preroll != nil {
		// Zeros first: S/PDIF receivers mute while relocking to a restarted clock.
		failed = p.writeChunk(s, s.preroll, false)
		s.preroll = nil
	}
	var lastFrame [64]byte // last frame successfully written, for the fade-outs
	s.last = lastFrame[:0]
	for b := range s.q {
		if !failed && !s.aborted.Load() {
			failed = p.writeChunk(s, b, true)
			if fb := s.outCh * s.fmt.BytesPerSample(); !failed && len(b) >= fb && fb <= len(lastFrame) {
				s.last = append(lastFrame[:0], b[len(b)-fb:]...)
			}
		}
		s.queued.Add(-int64(len(b) / (s.outCh * s.fmt.BytesPerSample())))
		p.putBuf(b)
	}
	if !failed && !s.aborted.Load() {
		if fb := s.outCh * s.fmt.BytesPerSample(); len(s.last) == fb {
			// Graceful end: continue from the last frame with a ~10 ms ramp to zero
			// so the stream does not stop on a step (once per session).
			s.pcm.Write(fadeTail(s.last, s.fmt, s.outCh, s.fadeN))
		}
		s.pcm.Drain()
	}
	s.pcm.Close()
	close(s.done)
}

// writeChunk writes b to the PCM (fading it in after an xrun if fade) and reports
// whether the write failed, in which case the session has been released.
func (p *Player) writeChunk(s *session, b []byte, fade bool) (failed bool) {
	if s.aborted.Load() {
		return false
	}
	if fade && s.refade.Swap(false) { // queue overflow dropped audio: hide the step
		if len(s.last) > 0 && s.fadePos >= s.fadeN { // else already ramping
			// Ramp the audio already written down to zero (rare, allocates).
			s.busy.Store(time.Now().UnixNano())
			s.pcm.Write(fadeTail(s.last, s.fmt, s.outCh, s.fadeN))
			s.busy.Store(0)
		}
		s.fadePos = 0
	}
	if fade && s.fadePos < s.fadeN { // continue a fade-in begun by an xrun or drop
		s.fadePos = fadeIn(b, s.fmt, s.outCh, s.fadePos, s.fadeN)
	}
	s.busy.Store(time.Now().UnixNano())
	err := s.pcm.Write(b)
	s.busy.Store(0)
	if xc, ok := s.pcm.(xrunCounter); ok {
		n := xc.Xruns()
		p.xruns.Add(n - s.xruns)
		s.xruns = n
	}
	if err != nil && !s.aborted.Load() {
		p.mu.Lock()
		p.lastErr = err.Error()
		if p.sess == s {
			p.nextTry = time.Now().Add(p.retryEvery)
			p.releaseLocked() // closes s.q
		}
		p.mu.Unlock()
		return true
	}
	return false
}

// abortLocked forces the session's writer out of any blocking driver call and makes
// it discard what is queued. Idempotent; the writer goroutine still closes the device.
func (p *Player) abortLocked(s *session) {
	s.aborted.Store(true)
	s.pcm.Abort()
}

// releaseLocked detaches the session; its goroutine drains and closes the device.
// If that takes longer than closeWaitLimit (hung driver) the PCM is aborted.
func (p *Player) releaseLocked() {
	s := p.sess
	if s == nil {
		return
	}
	p.sess = nil
	p.closing = s
	p.closedAt = time.Now()
	p.wd.Stop()
	close(s.q)
	t := time.AfterFunc(closeWaitLimit, func() {
		select {
		case <-s.done:
		default:
			s.aborted.Store(true)
			s.pcm.Abort()
		}
	})
	go func() { <-s.done; t.Stop() }()
}

func (p *Player) watchdog() {
	p.mu.Lock()
	if p.sess != nil && time.Since(p.lastWrite) >= p.noWrite {
		p.releaseLocked()
	}
	p.mu.Unlock()
}

// Stop ends the stream gracefully (SUSPEND): plays out what is queued, then releases
// the device. Waits up to closeWaitLimit, then aborts a hung driver.
func (p *Player) Stop() { p.stop(false) }

// Drop ends the stream without draining (CLOSE/ABORT/disconnect/shutdown): pending
// audio is discarded and the device released promptly.
func (p *Player) Drop() { p.stop(true) }

func (p *Player) stop(drop bool) {
	p.mu.Lock()
	if drop && p.sess != nil {
		p.abortLocked(p.sess)
	}
	p.releaseLocked()
	p.lastErr = ""
	p.silentFor = 0
	s := p.closing
	p.mu.Unlock()
	if s != nil {
		select {
		case <-s.done:
		case <-time.After(closeWaitLimit):
			p.mu.Lock()
			p.abortLocked(s)
			p.mu.Unlock()
			select {
			case <-s.done:
			case <-time.After(closeWaitLimit):
			}
		}
	}
}

// SetVolume sets the AVRCP absolute volume 0..127.
func (p *Player) SetVolume(v uint8) {
	if v > 127 {
		v = 127
	}
	p.mu.Lock()
	p.volume = v
	p.mu.Unlock()
}

func (p *Player) Volume() uint8 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.volume
}

func (p *Player) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Status{Open: p.sess != nil, Rate: p.rate, Channels: p.channels, Volume: p.volume, LastError: p.lastErr, Xruns: p.xruns.Load(), QueueDrop: p.queueDrop.Load()}
}

// Close releases everything (shutdown).
func (p *Player) Close() {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	p.Drop()
	p.wd.Stop()
}
