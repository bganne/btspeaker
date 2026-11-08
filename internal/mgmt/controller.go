package mgmt

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"btspeaker/internal/bt"
	"btspeaker/internal/hci"
	"btspeaker/internal/store"
)

const (
	classMajorAudio       = 0x04
	classMinorLoudspeaker = 0x14 // minor class 5 << 2: mgmt wants the CoD low byte (low 2 bits zero)
	svcHintAudioRendering = 0x24 // CoD bit 21 (Audio) | bit 18 (Rendering), as bits 16..23
	ioCapNoInputNoOutput  = 3
	legacyPIN             = "0000"
	retryInterval         = 5 * time.Second
	maxDiscoverableSecs   = 65535
)

// PairingAllowed is the pairing gate: a confirmation or PIN request from an
// unknown peer is accepted only while the pairing window is open (deadline in
// the future) or the adapter is configured always-pairable.
func PairingAllowed(now, deadline time.Time, alwaysPairable bool) bool {
	return alwaysPairable || (!deadline.IsZero() && now.Before(deadline))
}

// Config configures a Controller.
type Config struct {
	Adapter string // "", "hciN" or a MAC address; "" = first controller
	Name    string // default local name (the store's persisted alias wins)
	// AlwaysPairable keeps the adapter permanently discoverable and accepting
	// pairing: anyone in range can pair (open, Just Works pairing), no window.
	AlwaysPairable bool
	// RefuseLegacyPIN rejects legacy PIN pairing requests instead of answering
	// with the fixed PIN "0000" (which is trivially guessable).
	RefuseLegacyPIN bool
	PairingTimeout  time.Duration // default window length for OpenPairing(0)
	Store           *store.Store
	Log             *slog.Logger
	Open            func() (io.ReadWriteCloser, error) // default: hci.OpenMgmt
}

// Status is a snapshot of the adapter state.
type Status struct {
	Present          bool
	Addr             bt.Addr
	Name             string
	Powered          bool
	Discoverable     bool
	AlwaysPairable   bool
	PairingRemaining time.Duration // 0 when the window is closed
	Connected        []bt.Addr
	LastError        string
}

// Controller owns one adapter: startup configuration, pairing policy, link
// key persistence and connection tracking.
type Controller struct {
	cfg Config
	log *slog.Logger

	// winMu serialises OpenPairing/ClosePairing end to end (their mgmt
	// commands run outside mu) so the deadline matches the adapter state.
	winMu  sync.Mutex
	closed bool // set by Shutdown under winMu: OpenPairing must not reopen the window

	mu       sync.Mutex
	cl       *Client
	index    uint16
	present  bool
	info     Info
	conns    map[bt.Addr]bool
	names    map[bt.Addr]string // names learnt from connections
	pending  map[bt.Addr]string // names of known devices seen at ACL connect, not yet authenticated
	deadline time.Time
	timer    *time.Timer
	lastErr  string
	refused  map[bt.Addr]bool // strangers already warned about (bounded)
	refusedT time.Time        // last time the full refused set was cleared
	// loaded holds the devices whose key the kernel actually has: loaded at
	// init or stored from New Link Key. A stored device missing from it (bad
	// key, failed Load Link Keys) is treated as a stranger by Admit.
	loaded map[bt.Addr]bool
	// session holds devices admitted by a no-bonding pairing inside the
	// window: no key is stored, so they are admitted for that connection only
	// (cleared on Device Disconnected).
	session map[bt.Addr]bool
	// restore: bonded devices whose stored key the kernel dropped or will drop
	// (a no-bonding re-pair replaced it); reloaded from the store on disconnect.
	restore map[bt.Addr]bool

	// rfkill is the switch init soft-unblocked (by index and name), re-blocked
	// at Shutdown; rfkillSet is false if init found it unblocked.
	rfkill    rfkillSwitch
	rfkillSet bool

	// Init retry backoff; only touched from the serve goroutine.
	initFails   int       // consecutive failed inits
	initErr     string    // text of the last init failure
	initNextTry time.Time // earliest next retry

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// shutdownLockWait bounds how long Shutdown waits for winMu (var for tests).
var shutdownLockWait = 2 * time.Second

func New(cfg Config) *Controller {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Open == nil {
		cfg.Open = func() (io.ReadWriteCloser, error) { return hci.OpenMgmt() }
	}
	if cfg.PairingTimeout <= 0 {
		cfg.PairingTimeout = 2 * time.Minute
	}
	return &Controller{cfg: cfg, log: cfg.Log, conns: map[bt.Addr]bool{}, names: map[bt.Addr]string{}, pending: map[bt.Addr]string{}, refused: map[bt.Addr]bool{}, loaded: map[bt.Addr]bool{}, session: map[bt.Addr]bool{}, restore: map[bt.Addr]bool{}}
}

// Start opens the mgmt socket and runs the controller in the background until
// Shutdown or ctx is done. It fails only if the socket cannot be opened; a
// missing adapter is retried in the background (see Status.LastError).
func (c *Controller) Start(ctx context.Context) error {
	rw, err := c.cfg.Open()
	if err != nil {
		return err
	}
	ctx, c.cancel = context.WithCancel(ctx)
	c.setClient(NewClient(rw, c.log))
	c.wg.Add(1)
	go c.run(ctx)
	return nil
}

// Shutdown makes the adapter non-discoverable (best effort), stops the
// background goroutine and closes the socket.
func (c *Controller) Shutdown() {
	if c.cancel == nil {
		return
	}
	c.mu.Lock()
	if c.timer != nil {
		c.timer.Stop()
	}
	c.deadline = time.Time{}
	present := c.present
	c.mu.Unlock()
	// initAdapter holds winMu across many commands; if it does not release it
	// soon, abort it by closing the socket and skip the close-pairing commands
	// (still restoring rfkill below).
	locked := false
	for t := time.Now().Add(shutdownLockWait); ; time.Sleep(10 * time.Millisecond) {
		if locked = c.winMu.TryLock(); locked || !time.Now().Before(t) {
			break
		}
	}
	if locked {
		c.closed = true
		if present {
			// Discoverable and bondable off, so a restart/crash gap leaves no open
			// window (also with AlwaysPairable: nothing gates pairings once we exit).
			err := c.closePairingLocked()
			if c.cfg.AlwaysPairable {
				err = errors.Join(c.setDiscoverable(0, 0), c.setBondable(false))
			}
			if err != nil {
				c.log.Warn("shutdown: closing pairing window failed", "err", err)
			}
		}
		c.winMu.Unlock()
	} else {
		c.log.Warn("shutdown: adapter init still running, aborting it")
	}
	c.cancel()
	if cl := c.client(); cl != nil {
		cl.Close()
	}
	c.wg.Wait()
	// run may have reopened the socket between the read above and cancel.
	if cl := c.client(); cl != nil {
		cl.Close()
	}
	c.mu.Lock()
	sw, set := c.rfkill, c.rfkillSet
	c.rfkillSet = false
	c.mu.Unlock()
	if set {
		if done, err := rfkillRestore(sw); err != nil {
			c.log.Warn("shutdown: restoring rfkill block failed", "rfkill", sw.name, "err", err)
		} else if done {
			c.log.Info("rfkill soft block restored", "rfkill", sw.name)
		}
	}
}

func (c *Controller) client() *Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cl
}

func (c *Controller) setClient(cl *Client) {
	c.mu.Lock()
	c.cl = cl
	c.mu.Unlock()
}

// firstRefusal reports whether a is refused for the first time (bounded set:
// strangers must not grow it). A full set is cleared at most every
// refusedReset, so spoofed addresses cannot re-arm the warnings at will.
func (c *Controller) firstRefusal(a bt.Addr) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.refused[a] {
		return false
	}
	if len(c.refused) >= maxRefused {
		if now := time.Now(); now.Sub(c.refusedT) >= refusedReset {
			clear(c.refused)
			c.refusedT = now
		} else {
			return false
		}
	}
	c.refused[a] = true
	return true
}

const (
	maxRefused   = 64
	refusedReset = 10 * time.Minute
)

// logRefusal warns about a refused pairing request the first time a is seen
// (see firstRefusal), then logs at Debug: strangers must not flood the log.
func (c *Controller) logRefusal(msg string, a bt.Addr) {
	if c.firstRefusal(a) {
		c.log.Warn(msg, "addr", a.String())
	} else {
		c.log.Debug(msg, "addr", a.String())
	}
}

func (c *Controller) setErr(format string, a ...any) {
	c.mu.Lock()
	c.lastErr = fmt.Sprintf(format, a...)
	c.mu.Unlock()
}

// Status returns a snapshot.
func (c *Controller) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := Status{
		Present:        c.present,
		Addr:           c.info.Addr,
		Name:           c.info.Name,
		Powered:        c.info.Current&SettingPowered != 0,
		Discoverable:   c.info.Current&SettingDiscoverable != 0,
		AlwaysPairable: c.cfg.AlwaysPairable,
		LastError:      c.lastErr,
	}
	if rem := time.Until(c.deadline); !c.deadline.IsZero() && rem > 0 {
		s.PairingRemaining = rem
	}
	for a := range c.conns {
		if !c.loaded[a] && !c.session[a] { // unauthenticated peers are not "connected"
			continue
		}
		s.Connected = append(s.Connected, a)
	}
	sort.Slice(s.Connected, func(i, j int) bool { return s.Connected[i].String() < s.Connected[j].String() })
	return s
}

// cmd sends a command to the managed adapter.
func (c *Controller) cmd(op uint16, params []byte) ([]byte, error) {
	c.mu.Lock()
	cl, idx, present := c.cl, c.index, c.present
	c.mu.Unlock()
	if !present {
		return nil, ErrNoAdapter
	}
	return cl.Do(idx, op, params)
}

func (c *Controller) setDiscoverable(mode uint8, secs uint16) error {
	_, err := c.cmd(OpSetDiscoverable, EncodeSetDiscoverable(mode, secs))
	return err
}

// OpenPairing opens the pairing window for d (0 = configured default): the
// adapter becomes discoverable and pairing requests are accepted until d
// elapses or the first pairing completes. With AlwaysPairable it is a no-op.
func (c *Controller) OpenPairing(d time.Duration) error {
	if c.cfg.AlwaysPairable {
		return nil
	}
	if d <= 0 {
		d = c.cfg.PairingTimeout
	}
	secs := int(d.Round(time.Second) / time.Second)
	if secs < 1 {
		secs = 1
	}
	if secs > maxDiscoverableSecs {
		secs = maxDiscoverableSecs
	}
	c.winMu.Lock()
	defer c.winMu.Unlock()
	if c.closed {
		return errors.New("shutting down")
	}
	if err := c.setBondable(true); err != nil {
		return err
	}
	if err := c.setDiscoverable(1, uint16(secs)); err != nil {
		// An earlier window may be open: close it all so the deadline matches
		// the adapter (bondable is off now).
		c.logErr("close pairing", c.closePairingLocked())
		return err
	}
	dl := time.Now().Add(time.Duration(secs) * time.Second)
	c.mu.Lock()
	if c.timer != nil {
		c.timer.Stop()
	}
	c.deadline = dl
	c.timer = time.AfterFunc(time.Until(dl), func() { c.closeIfDeadline(dl) })
	c.mu.Unlock()
	c.log.Info("pairing window open", "seconds", secs)
	return nil
}

func (c *Controller) closeIfDeadline(dl time.Time) {
	c.winMu.Lock()
	defer c.winMu.Unlock()
	c.mu.Lock()
	same := c.deadline.Equal(dl)
	c.mu.Unlock()
	if same {
		if err := c.closePairingLocked(); err != nil {
			c.log.Warn("closing pairing window", "err", err)
		}
	}
}

// ClosePairing closes the window and makes the adapter non-discoverable.
func (c *Controller) ClosePairing() error {
	c.winMu.Lock()
	defer c.winMu.Unlock()
	return c.closePairingLocked()
}

// closePairingLocked is ClosePairing with winMu held.
func (c *Controller) closePairingLocked() error {
	c.mu.Lock()
	if c.timer != nil {
		c.timer.Stop()
	}
	was := !c.deadline.IsZero()
	c.deadline = time.Time{}
	c.mu.Unlock()
	if c.cfg.AlwaysPairable {
		return nil
	}
	if was {
		c.log.Info("pairing window closed")
	}
	return errors.Join(c.setDiscoverable(0, 0), c.setBondable(false))
}

// setBondable makes the kernel accept (true) or reject with "Pairing Not
// Allowed" (false) pairing requests that ask for bonding.
func (c *Controller) setBondable(on bool) error {
	_, err := c.cmd(OpSetBondable, u8(on))
	return err
}

// Admit reports whether addr may open AVDTP/AVRCP channels: it must be paired
// with its key loaded in the kernel, or the pairing window must be open. The kernel auto-accepts Just Works
// pairing for a NoInputNoOutput acceptor without asking us, so this is the gate
// that actually enforces the window.
func (c *Controller) Admit(addr bt.Addr) bool {
	return c.isLoaded(addr) || c.inSession(addr) || c.pairingAllowed()
}

func (c *Controller) inSession(addr bt.Addr) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.session[addr]
}

func (c *Controller) isLoaded(addr bt.Addr) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loaded[addr]
}

// Authenticated tells the controller that addr completed an authenticated
// (security level medium) L2CAP connection: the name learnt at connect, if
// any, is now trusted and persisted, and last_seen is updated (not at connect:
// the address alone may be spoofed).
func (c *Controller) Authenticated(addr bt.Addr) {
	// Off the accept path: Touch may fsync on slow storage.
	go func() { c.logErr("store touch", c.cfg.Store.Touch(addr, time.Now())) }()
	c.mu.Lock()
	name, ok := c.pending[addr]
	delete(c.pending, addr)
	if ok {
		c.names[addr] = name
	}
	c.mu.Unlock()
	if ok {
		go func() { c.logErr("store name", c.cfg.Store.SetDeviceName(addr, name)) }()
	}
}

func (c *Controller) pairingAllowed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return PairingAllowed(time.Now(), c.deadline, c.cfg.AlwaysPairable)
}

// Errors the web layer maps to 404/409/503 instead of a generic failure.
var (
	ErrUnknownDevice = errors.New("unknown device")
	ErrNotConnected  = errors.New("device not connected")
	ErrNoAdapter     = errors.New("bluetooth adapter not present")
)

// Disconnect drops the connection to addr.
func (c *Controller) Disconnect(addr bt.Addr) error {
	_, err := c.cmd(OpDisconnect, addrParam(addr))
	var se *StatusError
	if errors.As(err, &se) && se.Status == 5 { // Not Connected
		return fmt.Errorf("%w: %s", ErrNotConnected, addr)
	}
	return err
}

// Forget unpairs addr (disconnecting it) and drops the stored key.
func (c *Controller) Forget(addr bt.Addr) error {
	// Serialised with initAdapter: a re-init between the unpair and the store
	// removal would reload the key and re-admit the device.
	c.winMu.Lock()
	defer c.winMu.Unlock()
	c.mu.Lock()
	present := c.present
	c.mu.Unlock()
	var err error
	if present { // adapter absent: nothing to unpair, the next init loads keys from the store
		_, err = c.cmd(OpUnpairDevice, append(addrParam(addr), 1))
	}
	var se *StatusError
	notPaired := errors.As(err, &se) && se.Status == 6 // Not Paired is fine
	failed := err != nil && !notPaired
	known := c.cfg.Store.Has(addr)
	c.mu.Lock()
	delete(c.names, addr)
	delete(c.pending, addr)
	delete(c.session, addr)
	delete(c.restore, addr)
	delete(c.loaded, addr) // fail closed even if the kernel keeps the key
	c.mu.Unlock()
	if failed {
		// Keep the store entry so the user can retry; the kernel still has the key.
		c.log.Warn("unpair failed", "addr", addr, "err", err)
		return fmt.Errorf("unpair %s: %w", addr, err)
	}
	if serr := c.cfg.Store.Remove(addr); serr != nil {
		return serr
	}
	if notPaired && !known {
		return fmt.Errorf("%w: %s", ErrUnknownDevice, addr)
	}
	return nil
}

// SetName changes the adapter name and, only if the adapter accepted it, persists it.
func (c *Controller) SetName(name string) error {
	if _, err := c.cmd(OpSetLocalName, EncodeSetLocalName(name)); err != nil {
		return err
	}
	c.mu.Lock()
	c.info.Name = name
	c.mu.Unlock()
	return c.cfg.Store.SetName(name)
}

// ---- background loop ----

func (c *Controller) run(ctx context.Context) {
	defer c.wg.Done()
	for {
		c.serve(ctx, c.client())
		if ctx.Err() != nil {
			return
		}
		c.log.Error("mgmt socket closed, reopening")
		c.setErr("mgmt socket closed")
		c.mu.Lock()
		c.present = false
		c.conns = map[bt.Addr]bool{}
		c.deadline = time.Time{}
		if c.timer != nil {
			c.timer.Stop()
		}
		c.mu.Unlock()
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			rw, err := c.cfg.Open()
			if err != nil {
				c.log.Error("reopen mgmt", "err", err)
				continue
			}
			c.setClient(NewClient(rw, c.log))
			break
		}
	}
}

func (c *Controller) serve(ctx context.Context, cl *Client) {
	retry := time.NewTicker(retryInterval)
	defer retry.Stop()
	c.tryInit()
	for {
		select {
		case <-ctx.Done():
			return
		case <-retry.C:
			if !c.isPresent() && !time.Now().Before(c.initNextTry) {
				c.tryInit()
			}
		case ev, ok := <-cl.Events():
			if !ok {
				return
			}
			c.handle(ev)
		}
	}
}

func (c *Controller) isPresent() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.present
}

// initBackoff is the delay before retrying after fails consecutive failed
// inits: 5, 10, 20, 40 s, then 60 s. Each retry power-cycles the dongle.
func initBackoff(fails int) time.Duration {
	if fails < 1 {
		return 0
	}
	if fails > 4 {
		return maxInitBackoff
	}
	return min(retryInterval<<(fails-1), maxInitBackoff)
}

const maxInitBackoff = 60 * time.Second

// resetInitBackoff forgets earlier init failures (success or a new controller).
func (c *Controller) resetInitBackoff() {
	c.initFails = 0
	c.initErr = ""
	c.initNextTry = time.Time{}
}

func (c *Controller) tryInit() {
	err := c.initAdapter()
	if err == nil {
		c.resetInitBackoff()
		return
	}
	c.initFails++
	c.initNextTry = time.Now().Add(initBackoff(c.initFails))
	if msg := err.Error(); msg != c.initErr {
		c.initErr = msg
		c.log.Error("adapter init failed", "err", err)
	} else {
		c.log.Debug("adapter init failed", "err", err, "attempt", c.initFails)
	}
	c.setErr("adapter init: %v", err)
	c.mu.Lock()
	c.present = false
	c.mu.Unlock()
}

// pick selects the controller index per Config.Adapter.
func (c *Controller) pick(cl *Client) (uint16, Info, error) {
	data, err := cl.Do(IndexNone, OpReadIndexList, nil)
	if err != nil {
		return 0, Info{}, err
	}
	idxs, err := DecodeIndexList(data)
	if err != nil {
		return 0, Info{}, err
	}
	want := strings.TrimSpace(c.cfg.Adapter)
	var wantIdx = -1
	var wantAddr *bt.Addr
	switch {
	case want == "":
	case strings.HasPrefix(strings.ToLower(want), "hci"):
		n, err := strconv.Atoi(want[3:])
		if err != nil || n < 0 || n > 0xfffe {
			return 0, Info{}, fmt.Errorf("bad adapter %q", want)
		}
		wantIdx = n
	default:
		a, err := bt.ParseAddr(want)
		if err != nil {
			return 0, Info{}, fmt.Errorf("bad adapter %q (want hciN or MAC)", want)
		}
		wantAddr = &a
	}
	for _, idx := range idxs {
		if wantIdx >= 0 && int(idx) != wantIdx {
			continue
		}
		d, err := cl.Do(idx, OpReadInfo, nil)
		if err != nil {
			continue
		}
		info, err := DecodeInfo(d)
		if err != nil {
			continue
		}
		if wantAddr != nil && info.Addr != *wantAddr {
			continue
		}
		return idx, info, nil
	}
	if want == "" {
		return 0, Info{}, errors.New("no bluetooth controller found")
	}
	return 0, Info{}, fmt.Errorf("bluetooth adapter %s not found", want)
}

// initAdapter runs the startup sequence.
func (c *Controller) initAdapter() error {
	// Held throughout so a concurrent OpenPairing cannot be overwritten by the
	// bondable/discoverable-off steps (OpenPairing waits, or fails: not present).
	c.winMu.Lock()
	defer c.winMu.Unlock()
	cl := c.client()
	idx, info, err := c.pick(cl)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.index, c.info, c.present = idx, info, true
	c.deadline = time.Time{}
	c.conns = map[bt.Addr]bool{}
	c.loaded = map[bt.Addr]bool{}
	c.session = map[bt.Addr]bool{}
	c.restore = map[bt.Addr]bool{}
	c.mu.Unlock()
	c.log.Info("adapter found", "index", idx, "addr", info.Addr.String(), "name", info.Name)
	c.unblockRfkill(idx)

	name := c.cfg.Store.Name()
	if name == "" {
		name = c.cfg.Name
	}
	keys := c.storedKeys()

	type step struct {
		what     string
		op       uint16
		params   []byte
		optional bool
	}
	steps := []step{
		{"power off", OpSetPowered, u8(false), false},
		{"ssp", OpSetSSP, u8(true), false},
		{"br/edr", OpSetBREDR, u8(true), true},
		{"secure connections", OpSetSecureConn, []byte{1}, true}, // 1 = enabled, not SC-only
		{"le off", OpSetLE, u8(false), true},
		{"bondable", OpSetBondable, u8(c.cfg.AlwaysPairable), false}, // OpenPairing turns it on
		{"io capability", OpSetIOCapability, []byte{ioCapNoInputNoOutput}, false},
		{"local name", OpSetLocalName, EncodeSetLocalName(name), false},
		{"device class", OpSetDevClass, []byte{classMajorAudio, classMinorLoudspeaker}, false},
		{"clear uuids", OpRemoveUUID, make([]byte, 16), true}, // Remove UUID, all-zero UUID = all
		{"uuid a2dp sink", OpAddUUID, EncodeAddUUID(0x110B, svcHintAudioRendering), false},
		{"uuid avrcp target", OpAddUUID, EncodeAddUUID(0x110C, 0), false},
		{"uuid avrcp controller", OpAddUUID, EncodeAddUUID(0x110E, 0), false},
		{"load link keys", OpLoadLinkKeys, EncodeLoadLinkKeys(keys), false},
		{"connectable", OpSetConnectable, u8(true), false},
		{"power on", OpSetPowered, u8(true), false},
	}
	if c.cfg.AlwaysPairable {
		steps = append(steps, step{"discoverable", OpSetDiscoverable, EncodeSetDiscoverable(1, 0), false})
	} else {
		// Before power on (mgmt accepts it while off): clears a stale flag from a
		// crash. After connectable: the kernel rejects any Set Discoverable,
		// even off, while the adapter is not connectable.
		on := len(steps) - 1
		steps = append(steps[:on:on], step{"discoverable off", OpSetDiscoverable, EncodeSetDiscoverable(0, 0), false}, steps[on])
	}
	loadErr := ""
	// fillLoaded admits the keys the kernel now holds. Done right after the
	// load, so a bonded device reconnecting during the remaining steps is not
	// refused. A device forgotten since the snapshot is unpaired, not admitted.
	fillLoaded := func() {
		var gone []bt.Addr
		c.mu.Lock()
		c.loaded = make(map[bt.Addr]bool, len(keys))
		for _, k := range keys {
			if c.cfg.Store.Has(k.Addr) {
				c.loaded[k.Addr] = true
			} else {
				gone = append(gone, k.Addr)
			}
		}
		c.mu.Unlock()
		for _, a := range gone {
			if _, err := cl.Do(idx, OpUnpairDevice, append(addrParam(a), 1)); err != nil {
				c.log.Warn("unpair forgotten device failed", "addr", a.String(), "err", err)
			}
		}
	}
	// Skipped when already on: the kernel rejects them once LE is off
	// (Set BR/EDR: Rejected; SC on a pre-4.1 controller: Not Supported).
	alreadyOn := map[uint16]uint32{OpSetBREDR: SettingBREDR, OpSetSecureConn: SettingSecureConn}
	for _, s := range steps {
		if info.Current&alreadyOn[s.op] != 0 {
			continue
		}
		if _, err := cl.Do(idx, s.op, s.params); err != nil {
			if s.op == OpLoadLinkKeys {
				// Don't let bad stored keys keep the speaker dead: keep those the
				// kernel accepts (Load Link Keys is all-or-nothing per call).
				good, err2 := loadKeysTolerant(func(p []byte) error { _, e := cl.Do(idx, s.op, p); return e }, keys, true, c.log)
				if err2 != nil {
					return fmt.Errorf("%s: %w", s.what, err2)
				}
				c.log.Error("loading stored link keys failed; continuing with the accepted ones", "err", err, "loaded", len(good), "stored", len(keys))
				loadErr = fmt.Sprintf("loading stored link keys failed: %v", err)
				keys = good
				fillLoaded()
				continue
			}
			if s.optional {
				c.log.Warn("optional init step failed", "step", s.what, "err", err)
				continue
			}
			if s.op == OpSetPowered {
				if sw, ok := rfkillFind(rfkillDir, idx); ok && (sw.soft || sw.hard) {
					// The kernel reports -ERFKILL to mgmt as a generic "Failed".
					return fmt.Errorf("%s: %w (hci%d is rfkill-blocked, soft=%t hard=%t)", s.what, err, idx, sw.soft, sw.hard)
				}
			}
			return fmt.Errorf("%s: %w", s.what, err)
		}
		if s.op == OpLoadLinkKeys {
			fillLoaded()
		}
	}
	if d, err := cl.Do(idx, OpReadInfo, nil); err == nil {
		if ni, err := DecodeInfo(d); err == nil {
			c.mu.Lock()
			c.info = ni
			c.mu.Unlock()
		}
	}
	if d, err := cl.Do(idx, OpGetConnections, nil); err == nil {
		if as, err := DecodeConnections(d); err == nil {
			c.mu.Lock()
			for _, a := range as {
				c.conns[a] = true
			}
			c.mu.Unlock()
		}
	}
	c.mu.Lock()
	c.lastErr = loadErr
	c.mu.Unlock()
	c.log.Info("adapter ready", "keys", len(keys), "always_pairable", c.cfg.AlwaysPairable)
	return nil
}

func (c *Controller) handle(ev Event) {
	switch ev.Code {
	case EvIndexAdded:
		if !c.isPresent() {
			c.log.Info("controller added", "index", ev.Index)
			c.resetInitBackoff()
			c.tryInit()
		}
		return
	case EvIndexRemoved:
		c.mu.Lock()
		hit := c.present && c.index == ev.Index
		if hit {
			c.present = false
			c.conns = map[bt.Addr]bool{}
			c.deadline = time.Time{}
			if c.timer != nil {
				c.timer.Stop()
			}
			c.lastErr = "bluetooth adapter removed"
		}
		c.mu.Unlock()
		if hit {
			c.log.Warn("adapter removed; waiting for it to return", "index", ev.Index)
		}
		return
	}
	c.mu.Lock()
	mine := c.present && ev.Index == c.index
	c.mu.Unlock()
	if !mine {
		return
	}
	switch ev.Code {
	case EvNewSettings:
		if len(ev.Params) >= 4 {
			c.mu.Lock()
			c.info.Current = uint32(ev.Params[0]) | uint32(ev.Params[1])<<8 | uint32(ev.Params[2])<<16 | uint32(ev.Params[3])<<24
			c.mu.Unlock()
		}
	case EvLocalNameChanged:
		if len(ev.Params) >= 249 {
			c.mu.Lock()
			c.info.Name = cstr(ev.Params[:249])
			c.mu.Unlock()
		}
	case EvUserConfirmReq:
		addr, _, _, err := DecodeAddrEvent(ev.Params)
		if err != nil {
			return // short, or an LE address
		}
		if c.pairingAllowed() {
			c.log.Info("pairing: confirming", "addr", addr.String())
			c.reply(OpUserConfirmReply, addrParam(addr))
		} else {
			c.logRefusal("pairing refused: window closed", addr)
			c.reply(OpUserConfirmNeg, addrParam(addr))
		}
	case EvPINCodeRequest:
		addr, _, _, err := DecodeAddrEvent(ev.Params)
		if err != nil {
			return // short, or an LE address
		}
		if c.cfg.RefuseLegacyPIN {
			c.logRefusal("legacy PIN pairing refused by policy", addr)
			c.reply(OpPINCodeNegReply, addrParam(addr))
		} else if c.pairingAllowed() {
			c.log.Info("pairing: legacy PIN", "addr", addr.String())
			c.reply(OpPINCodeReply, EncodePINReply(addr, legacyPIN))
		} else {
			c.logRefusal("legacy pairing refused: window closed", addr)
			c.reply(OpPINCodeNegReply, addrParam(addr))
		}
	case EvNewLinkKey:
		c.onNewLinkKey(ev.Params)
	case EvDeviceConnected:
		d, err := DecodeDeviceConnected(ev.Params)
		if errors.Is(err, ErrNotBREDR) {
			return
		}
		if err != nil {
			c.log.Warn("bad device connected", "err", err)
			return
		}
		d.Name = SanitizeName(d.Name)
		c.mu.Lock()
		c.conns[d.Addr] = true
		// The EIR name arrives before authentication, so a spoofer of a known
		// address must not rewrite its stored name: hold it until Authenticated.
		// Strangers are remembered only during the pairing window, capped: the
		// map must not grow with every stranger that connects.
		known := c.cfg.Store.Has(d.Addr)
		if d.Name != "" {
			if known {
				if _, have := c.pending[d.Addr]; have || len(c.pending) < maxNames {
					c.pending[d.Addr] = d.Name
				}
			} else if _, have := c.names[d.Addr]; have || (len(c.names) < maxNames &&
				PairingAllowed(time.Now(), c.deadline, c.cfg.AlwaysPairable)) {
				c.names[d.Addr] = d.Name
			}
		}
		c.mu.Unlock()
		lvl := slog.LevelInfo
		if !c.cfg.Store.Has(d.Addr) {
			lvl = slog.LevelDebug // strangers: keep the log quiet
		}
		c.log.Log(context.Background(), lvl, "device connected", "addr", d.Addr.String(), "name", d.Name)
	case EvDeviceDisconn:
		addr, _, rest, err := DecodeAddrEvent(ev.Params)
		if err != nil {
			return
		}
		known := c.cfg.Store.Has(addr)
		c.mu.Lock()
		delete(c.conns, addr)
		delete(c.pending, addr)
		delete(c.session, addr)
		if !known {
			delete(c.names, addr)
		}
		restore := c.restore[addr]
		delete(c.restore, addr)
		c.mu.Unlock()
		if restore {
			// The kernel flushed the real key with the no-bonding one.
			c.winMu.Lock()
			c.reloadKeysLocked(addr)
			c.winMu.Unlock()
		}
		reason := -1
		if len(rest) > 0 {
			reason = int(rest[0])
		}
		lvl := slog.LevelInfo
		if !known {
			lvl = slog.LevelDebug // strangers: keep the log quiet
		}
		c.log.Log(context.Background(), lvl, "device disconnected", "addr", addr.String(), "reason", reason)
	case EvAuthFailed:
		addr, _, rest, err := DecodeAddrEvent(ev.Params)
		if err != nil {
			return
		}
		st := Status0(rest)
		if c.cfg.Store.Has(addr) { // strangers must not overwrite the UI error or flood the log
			c.log.Warn("authentication failed", "addr", addr.String(), "status", st)
			c.setErr("authentication with %s failed: %s", addr, st)
		} else {
			c.log.Debug("authentication failed", "addr", addr.String(), "status", st)
		}
	case EvConnectFailed:
		addr, _, rest, err := DecodeAddrEvent(ev.Params)
		if err != nil {
			return
		}
		lvl := slog.LevelWarn
		if !c.cfg.Store.Has(addr) {
			lvl = slog.LevelDebug // strangers: rejected incoming ACLs must not flood the log
		}
		c.log.Log(context.Background(), lvl, "connect failed", "addr", addr.String(), "status", Status0(rest))
	case EvControllerError:
		c.log.Warn("controller error", "params", fmt.Sprintf("%x", ev.Params))
	}
}

// Status0 formats a leading status byte of an event tail.
func Status0(rest []byte) StatusCode {
	if len(rest) == 0 {
		return 0
	}
	return StatusCode(rest[0])
}

func (c *Controller) logErr(what string, err error) {
	if err != nil {
		c.log.Error(what, "err", err)
	}
}

func (c *Controller) reply(op uint16, params []byte) {
	if _, err := c.cmd(op, params); err != nil {
		c.log.Warn("pairing reply failed", "op", op, "err", err)
	}
}

func (c *Controller) onNewLinkKey(p []byte) {
	n, err := DecodeNewLinkKey(p)
	if errors.Is(err, ErrNotBREDR) {
		return
	}
	if err != nil {
		c.log.Warn("bad new link key", "err", err)
		return
	}
	// The kernel auto-accepts Just Works pairing without a User Confirm Request,
	// so a stranger can get here with the window closed: drop and revoke the key.
	// A stored device whose key never reached the kernel counts as new too:
	// its address alone proves nothing.
	// A no-bonding key (StoreHint false) outside the window is refused even for
	// a loaded address: a spoofer could otherwise pass the security check with
	// it. A legitimately bonded device re-keys with StoreHint true.
	isNew := !c.isLoaded(n.Key.Addr)
	if (isNew || !n.StoreHint) && !c.pairingAllowed() {
		if c.firstRefusal(n.Key.Addr) {
			c.log.Warn("pairing refused: window closed, revoking key", "addr", n.Key.Addr.String())
			c.setErr("pairing with %s completed after the window closed: refused", n.Key.Addr)
		} else {
			c.log.Debug("pairing refused: window closed, revoking key", "addr", n.Key.Addr.String())
		}
		c.revokeKey(n.Key.Addr)
		return
	}
	c.acceptKey(n, isNew)
}

// revokeKey drops the kernel key of addr after a refused pairing and restores
// the stored one if the kernel had it. Takes winMu itself (so mu must not be
// held): Forget must not remove the device between the Store.Has check and
// the reload (that would re-admit a forgotten device). Safe to block here:
// command replies come from the reader, not the event goroutine.
func (c *Controller) revokeKey(addr bt.Addr) {
	c.winMu.Lock()
	defer c.winMu.Unlock()
	c.mu.Lock()
	wasLoaded := c.loaded[addr]
	wasRestore := c.restore[addr] // stored key flushed by an earlier no-bonding re-key
	delete(c.loaded, addr)        // the kernel key is revoked below
	delete(c.session, addr)
	delete(c.restore, addr)
	c.mu.Unlock()
	if _, err := c.cmd(OpUnpairDevice, append(addrParam(addr), 1)); err != nil {
		c.log.Warn("unpair failed", "addr", addr.String(), "err", err)
	}
	// The Unpair removed the kernel's real key if this address is bonded
	// (a no-bonding re-key replaces it); restore it. Only if the kernel had
	// it (or would after a pending restore): a key that never loaded must
	// stay unloaded.
	if wasLoaded || wasRestore {
		c.reloadKeysLocked(addr)
	}
}

// acceptKey admits and persists a key onNewLinkKey let through. Neither winMu
// nor mu may be held on entry: it takes mu briefly, then (via ClosePairing and
// Forget) winMu, never both nested.
func (c *Controller) acceptKey(n NewLinkKey, isNew bool) {
	c.mu.Lock()
	if p, ok := c.pending[n.Key.Addr]; ok { // a fresh key proves the peer authenticated
		c.names[n.Key.Addr] = p
		delete(c.pending, n.Key.Addr)
	}
	name := c.names[n.Key.Addr]
	open := !c.deadline.IsZero()
	c.mu.Unlock()
	// The window is for one device: close it after the first successful
	// pairing of a new device (a known device re-keying does not count).
	closeWin := open && isNew && !c.cfg.AlwaysPairable
	// Admit the device in the same section that shuts the pairing gate, before
	// the (slow) store call; it stays set if the save fails (setErr reports).
	// Without StoreHint only this connection is admitted.
	c.mu.Lock()
	if n.StoreHint {
		c.loaded[n.Key.Addr] = true
	} else {
		c.session[n.Key.Addr] = true
		// The kernel replaces a bonded device's key in place and flushes a
		// non-persistent one on disconnect (hci_add_link_key,
		// HCI_CONN_FLUSH_KEY): the stored key is gone from the kernel until
		// reloaded after the disconnect. Meanwhile do not treat it as loaded.
		if c.loaded[n.Key.Addr] && c.cfg.Store.Has(n.Key.Addr) {
			delete(c.loaded, n.Key.Addr)
			c.restore[n.Key.Addr] = true
		}
	}
	if closeWin {
		// Shut the Admit/pairingAllowed gate now, not when the goroutine runs.
		c.deadline = time.Time{}
	}
	c.mu.Unlock()
	if closeWin {
		// The window timer no longer matches the deadline, so close the
		// window on every return path, including a failed key save.
		defer func() { go func() { c.logErr("close pairing window", c.ClosePairing()) }() }()
	}
	if !n.StoreHint {
		c.log.Info("link key not to be stored", "addr", n.Key.Addr.String())
		return
	}
	k := store.NewLinkKey(n.Key.Type, n.Key.Value, n.Key.PinLen)
	if err := c.cfg.Store.PutKeyNamed(n.Key.Addr, k, name, time.Now()); err != nil {
		c.log.Error("store link key", "err", err)
		c.setErr("saving link key failed: %v", err)
		return
	}
	c.log.Info("paired", "addr", n.Key.Addr.String(), "name", name, "key_type", n.Key.Type)
	if isNew {
		for _, old := range c.cfg.Store.EvictOldest(n.Key.Addr, store.MaxDevices) {
			c.log.Warn("device limit reached: forgetting least recently seen device", "addr", old.String())
			c.setErr("device limit reached: forgot %s", old)
			if err := c.Forget(old); err != nil && !errors.Is(err, ErrUnknownDevice) {
				// EvictOldest already dropped the store entry, so this fails closed.
				c.log.Error("evict device: unpair failed, key may linger in the kernel until restart", "addr", old.String(), "err", err)
			}
		}
	}
}

// maxNames caps the remembered remote names.
const maxNames = 64
