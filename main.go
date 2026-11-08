// btspeaker turns a Bluetooth adapter into an A2DP speaker playing to an ALSA hw device.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"btspeaker/internal/alsa"
	"btspeaker/internal/avdtp"
	"btspeaker/internal/avrcp"
	"btspeaker/internal/bt"
	"btspeaker/internal/hci"
	"btspeaker/internal/mgmt"
	"btspeaker/internal/player"
	"btspeaker/internal/sdp"
	"btspeaker/internal/store"
	"btspeaker/internal/web"
)

type options struct {
	adapter        string
	name           string
	alsaDevice     string
	buffer         time.Duration
	idleRelease    time.Duration
	spdifPreroll   time.Duration
	pairingTimeout time.Duration
	alwaysPairable bool
	noLegacyPIN    bool
	maxBitpool     int
	volume         string
	stateDir       string
	listen         string
	httpUser       string
	httpPassword   string
	httpHosts      string
	listDevices    bool
	testTone       bool
}

// envName maps a flag name to its environment variable: "alsa-device" -> BTSPEAKER_ALSA_DEVICE.
func envName(flagName string) string {
	return "BTSPEAKER_" + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// applyEnv sets every flag not given on the command line from its environment variable.
// It adds the names of the flags it set to set, which also holds the command-line flags.
func applyEnv(fs *flag.FlagSet, getenv func(string) (string, bool), set map[string]bool) error {
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	var fromEnv []string
	var err error
	fs.VisitAll(func(f *flag.Flag) {
		if set[f.Name] || err != nil {
			return
		}
		v, ok := getenv(envName(f.Name))
		if !ok && f.Name == "http-password" { // name used in the original plan
			v, ok = getenv("BTSPEAKER_PASSWORD")
		}
		if ok {
			if e := f.Value.Set(v); e != nil {
				err = fmt.Errorf("%s: %w", envName(f.Name), e)
			}
			fromEnv = append(fromEnv, f.Name)
		}
	})
	for _, n := range fromEnv {
		set[n] = true
	}
	return err
}

func parseOptions(args []string, getenv func(string) (string, bool)) (*options, error) {
	var o options
	fs := flag.NewFlagSet("btspeaker", flag.ContinueOnError)
	fs.StringVar(&o.adapter, "adapter", "", "adapter: hciN or MAC address (default: first)")
	fs.StringVar(&o.name, "name", "", "Bluetooth speaker name (default: host's FQDN; a name set via the web UI wins)")
	fs.StringVar(&o.alsaDevice, "alsa-device", "hw:0,0", "ALSA hw device, hw:CARD,DEV")
	fs.DurationVar(&o.buffer, "buffer", 200*time.Millisecond, "ALSA buffer size")
	fs.DurationVar(&o.idleRelease, "idle-release", 5*time.Second, "release the ALSA device after this long of digital silence")
	fs.DurationVar(&o.pairingTimeout, "pairing-timeout", time.Minute, "default pairing window length")
	fs.BoolVar(&o.alwaysPairable, "always-pairable", false, "stay discoverable and accept pairing at all times: open Just Works pairing, anyone in range can pair (insecure)")
	fs.IntVar(&o.maxBitpool, "sbc-max-bitpool", 76, "max SBC bitpool advertised to sources (53 = standard high quality, 76+ = SBC XQ; 2..250)")
	fs.BoolVar(&o.noLegacyPIN, "no-legacy-pin", false, "refuse legacy PIN pairing (fixed PIN 0000, sniffable) instead of answering it")
	fs.DurationVar(&o.spdifPreroll, "spdif-preroll", 200*time.Millisecond, "zeros played after (re)opening the ALSA device so an S/PDIF receiver can relock before the audio starts; 0 = off")
	fs.StringVar(&o.volume, "volume", "software", "software (AVRCP volume as gain) or off (bit-perfect)")
	fs.StringVar(&o.stateDir, "state-dir", "/var/lib/btspeaker", "directory for state.json")
	fs.StringVar(&o.listen, "listen", ":8099", "HTTP listen address")
	fs.StringVar(&o.httpUser, "http-user", "admin", "HTTP basic auth user")
	fs.StringVar(&o.httpPassword, "http-password", "", "HTTP basic auth password (empty = no auth); visible in ps, prefer BTSPEAKER_HTTP_PASSWORD")
	fs.StringVar(&o.httpHosts, "http-hosts", "", "comma-separated hostnames accepted in the HTTP Host header (default: hostname, FQDN, hostname.local, localhost; IP literals always accepted)")
	fs.BoolVar(&o.listDevices, "list-devices", false, "print ALSA playback devices and exit")
	fs.BoolVar(&o.testTone, "test-tone", false, "play a 2 s test tone on -alsa-device and exit")
	fs.SetOutput(io.Discard) // main prints a parse error once; usage only for -h
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fs.SetOutput(os.Stderr)
			fs.Usage()
		}
		return nil, err
	}
	explicit := map[string]bool{} // set on the command line or from the environment
	if err := applyEnv(fs, getenv, explicit); err != nil {
		return nil, err
	}
	if o.volume != "software" && o.volume != "off" {
		return nil, fmt.Errorf("-volume must be software or off, got %q", o.volume)
	}
	if o.buffer < 20*time.Millisecond || o.buffer > 2*time.Second {
		return nil, fmt.Errorf("-buffer must be 20ms..2s, got %v", o.buffer)
	}
	if o.idleRelease <= 0 {
		return nil, fmt.Errorf("-idle-release must be > 0, got %v", o.idleRelease)
	}
	if o.spdifPreroll < 0 || o.spdifPreroll > time.Second {
		return nil, fmt.Errorf("-spdif-preroll must be 0..1s, got %v", o.spdifPreroll)
	}
	if o.spdifPreroll > o.buffer {
		if explicit["spdif-preroll"] {
			return nil, fmt.Errorf("-spdif-preroll (%v) must not exceed -buffer (%v)", o.spdifPreroll, o.buffer)
		}
		o.spdifPreroll = o.buffer // the default must not make a small -buffer fatal
	}
	var err error
	if o.name == "" {
		// A hostname mgmt rejects must not exit(2) in a systemd restart loop.
		o.name = "btspeaker"
		if n, err := mgmt.ValidName(defaultName()); err == nil {
			o.name = n
		} else {
			slog.Warn("default name unusable, using btspeaker (set -name)", "err", err)
		}
	} else if o.name, err = mgmt.ValidName(o.name); err != nil {
		return nil, fmt.Errorf("-name: %w", err)
	}
	if o.maxBitpool < 2 || o.maxBitpool > 250 {
		return nil, fmt.Errorf("-sbc-max-bitpool must be 2..250, got %d", o.maxBitpool)
	}
	return &o, nil
}

// bluetoothdRunning reports whether a process named bluetoothd exists.
func bluetoothdRunning() bool {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, e := range ents {
		if e.Name() == "" || e.Name()[0] < '0' || e.Name()[0] > '9' {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/comm")
		if err == nil && strings.TrimSpace(string(b)) == "bluetoothd" {
			return true
		}
	}
	return false
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	o, err := parseOptions(os.Args[1:], os.LookupEnv)
	if errors.Is(err, flag.ErrHelp) {
		os.Exit(0)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "btspeaker:", err)
		os.Exit(2) // the unit's RestartPreventExitStatus: a config error does not fix itself
	}
	if err := run(o); err != nil {
		slog.Error("fatal", "err", err)
		if errors.Is(err, errBluetoothd) {
			os.Exit(3) // the unit's RestartPreventExitStatus: do not restart-loop
		}
		os.Exit(1)
	}
}

var errBluetoothd = errors.New("bluetoothd is running")

func run(o *options) error {
	if o.listDevices {
		cards, err := alsa.ListPlayback()
		if err != nil {
			return err
		}
		for _, c := range cards {
			fmt.Printf("%s\t%s\t%s\n", c.Device, c.Name, c.Desc)
		}
		return nil
	}
	if o.testTone {
		return player.TestTone(o.alsaDevice, 2*time.Second)
	}
	if bluetoothdRunning() {
		return fmt.Errorf("%w and would conflict with btspeaker (PSM 1, pairing, link keys); "+
			"stop it first: systemctl disable --now bluetooth.service && systemctl mask bluetooth.service", errBluetoothd)
	}

	rates := []int{44100, 48000}
	probed := false
	if caps, err := alsa.Probe(o.alsaDevice); err != nil {
		slog.Warn("ALSA probe failed, assuming 44100+48000 and retrying", "device", o.alsaDevice, "err", err)
	} else {
		probed = true
		if len(caps.Rates) > 0 {
			rates = caps.Rates
		}
	}
	slog.Info("ALSA device", "device", o.alsaDevice, "rates", rates)

	st, err := store.Open(o.stateDir)
	if err != nil {
		return fmt.Errorf("state: %w", err)
	}
	if bak := st.Corrupt(); bak != "" {
		slog.Warn("state file was corrupt, starting empty (paired devices lost)", "backup", bak)
	}

	pl := player.New(player.Config{Device: o.alsaDevice, Buffer: o.buffer, IdleRelease: o.idleRelease, SoftVolume: o.volume == "software", Preroll: o.spdifPreroll})
	defer pl.Close()
	sdpSrv := sdp.NewServer()
	avdtpSrv := avdtp.NewServer(avdtp.Config{SampleRates: rates, MaxBitpool: o.maxBitpool}, pl)
	avrcpSrv := avrcp.NewServer(pl)
	avrcpSrv.HasSession = avdtpSrv.HasSession
	avrcpSrv.MayControlVolume = avdtpSrv.MayControlVolume

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if !probed {
		go reprobeRates(ctx, o.alsaDevice, probeRetry, alsa.Probe, avdtpSrv.SetSampleRates)
	}

	// Bind HTTP first: a busy port must fail before the adapter is powered, or
	// systemd would power-cycle it every restart.
	ln, err := net.Listen("tcp", o.listen)
	if err != nil {
		return err
	}
	served := false
	defer func() {
		if !served {
			ln.Close()
		}
	}()

	ctrl := mgmt.New(mgmt.Config{Adapter: o.adapter, Name: o.name, AlwaysPairable: o.alwaysPairable,
		PairingTimeout: o.pairingTimeout, RefuseLegacyPIN: o.noLegacyPIN, Store: st, Log: slog.Default()})
	if err := ctrl.Start(ctx); err != nil {
		return fmt.Errorf("mgmt: %w (needs root or CAP_NET_ADMIN/CAP_NET_RAW)", err)
	}

	var wg sync.WaitGroup
	var listeners []*hci.Listener
	closeAll := func() {
		for _, l := range listeners {
			l.Close()
		}
	}
	// SDP is open to all (phones browse it before pairing); AVDTP and AVRCP only
	// to paired peers or during the pairing window.
	gate := newAdmitGate(ctrl.Admit)
	gate.auth = ctrl.Authenticated
	// avdtp allows 3 per peer (signalling + media + a reconnect's new signalling,
	// which evicts the stale pair).
	for _, s := range []struct {
		psm    uint16
		handle func(bt.Conn)
		total  int
		peer   int
		gate   *admitGate
	}{{sdp.PSM, sdpSrv.HandleConn, 16, 3, nil}, {avdtp.PSM, avdtpSrv.HandleConn, 6, 3, gate}, {avrcp.PSM, avrcpSrv.HandleConn, 4, 2, gate}} {
		l, err := hci.Listen(bt.Addr{}, s.psm)
		if err != nil {
			closeAll()
			ctrl.Shutdown()
			return fmt.Errorf("%w (is bluetoothd running?)", err)
		}
		listeners = append(listeners, l)
		wg.Add(1)
		go func() {
			defer wg.Done()
			acceptLoop(l, s.handle, newLimiter(s.total, s.peer), s.gate)
		}()
	}

	be := &backend{ctrl: ctrl, st: st, avdtp: avdtpSrv, pl: pl, bitPerfect: o.volume != "software"}
	httpSrv := &http.Server{Addr: o.listen, Handler: web.Handler(be, web.Config{User: o.httpUser, Password: o.httpPassword, Hosts: allowedHosts(o.httpHosts)}),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second,
		IdleTimeout: web.IdleTimeout, MaxHeaderBytes: 16 << 10}
	ln = newConnLimitListener(ln, maxHTTPConns, maxHTTPConnsPerIP)
	served = true
	slog.Info("web UI listening", "addr", ln.Addr().String(), "auth", o.httpPassword != "")
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http", "err", err)
		}
	}()

	<-ctx.Done()
	stop() // restore default signal handling: a second Ctrl-C kills a hung shutdown
	slog.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ctrl.Shutdown() // discoverable off first: the HTTP shutdown can take up to 2 s
	if err := httpSrv.Shutdown(sctx); err != nil {
		slog.Warn("http shutdown", "err", err)
		httpSrv.Close()
	}
	closeAll()
	// Bounded: a stuck goroutine must not make systemd wait TimeoutStopSec (90 s).
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(shutdownWait):
		slog.Error("shutdown: accept loops still running, exiting anyway", "after", shutdownWait)
	}
	return nil
}

const shutdownWait = 5 * time.Second

// defaultName is the host's FQDN, computed once; a variable for tests.
var defaultName = sync.OnceValue(func() string {
	h, _ := os.Hostname()
	hosts, _ := os.ReadFile("/etc/hosts")
	resolv, _ := os.ReadFile("/etc/resolv.conf")
	return fqdn(h, string(hosts), string(resolv))
})

// fqdn qualifies host like hostname -f on Debian, but without DNS (the network
// may not be up at boot): the name /etc/hosts gives it, else host plus the
// resolv.conf domain or first search domain, else host alone.
func fqdn(host, hosts, resolv string) string {
	if host == "" {
		return "btspeaker"
	}
	if strings.Contains(host, ".") {
		return host
	}
	for _, line := range strings.Split(hosts, "\n") {
		line, _, _ = strings.Cut(line, "#")
		f := strings.Fields(line)
		if len(f) >= 2 && strings.HasPrefix(f[1], host+".") && slices.Contains(f[1:], host) {
			return f[1]
		}
	}
	var domain, search string
	for _, line := range strings.Split(resolv, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "domain":
			domain = f[1]
		case "search":
			search = f[1]
		}
	}
	if domain == "" {
		domain = search
	}
	if domain = strings.TrimSuffix(domain, "."); domain == "" {
		return host
	}
	return host + "." + domain
}

// allowedHosts parses -http-hosts; empty means hostname, its FQDN, hostname.local and localhost.
func allowedHosts(list string) []string {
	var out []string
	for _, h := range strings.Split(list, ",") {
		if h = strings.TrimSpace(h); h != "" {
			out = append(out, h)
		}
	}
	if len(out) == 0 {
		out = []string{"localhost"}
		if h, err := os.Hostname(); err == nil && h != "" {
			out = append(out, h, h+".local")
			// The default speaker name (what a phone shows) is the FQDN.
			if n := defaultName(); n != "" && n != h {
				out = append(out, n)
			}
		}
	}
	return out
}

// limiter caps concurrent connections in total and per peer address.
type limiter struct {
	mu          sync.Mutex
	total, peer int
	n           int
	perPeer     map[bt.Addr]int
}

func newLimiter(total, peer int) *limiter {
	return &limiter{total: total, peer: peer, perPeer: map[bt.Addr]int{}}
}

// acquire reserves a slot for a; the caller must call release(a) if it returns true.
func (l *limiter) acquire(a bt.Addr) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.n >= l.total || l.perPeer[a] >= l.peer {
		return false
	}
	l.n++
	l.perPeer[a]++
	return true
}

func (l *limiter) release(a bt.Addr) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.n--
	if l.perPeer[a]--; l.perPeer[a] <= 0 {
		delete(l.perPeer, a)
	}
}

// maxHTTPConns caps concurrent HTTP connections so idle LAN clients cannot exhaust the fd
// budget (LimitNOFILE=256) that L2CAP accept and the ALSA open also need.
const maxHTTPConns = 32

// maxHTTPConnsPerIP keeps one LAN host from holding every slot.
const maxHTTPConnsPerIP = 8

// probeRetry is how often a failed boot-time ALSA probe is retried.
const probeRetry = 30 * time.Second

// reprobeRates retries probe every interval until it succeeds (the device was
// busy at boot), then hands the rates to set once and returns.
func reprobeRates(ctx context.Context, dev string, interval time.Duration, probe func(string) (alsa.Caps, error), set func([]int)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		caps, err := probe(dev)
		if err != nil {
			continue
		}
		if len(caps.Rates) > 0 {
			set(caps.Rates)
		}
		slog.Info("ALSA device probed", "device", dev, "rates", caps.Rates)
		return
	}
}

// connLimitListener blocks in Accept while max connections are open. The slot is taken
// before the blocking Listener.Accept and becomes that connection's slot when one
// arrives, so the cap is exactly max (not max-1); the pending Accept merely reserves
// the next slot.
type connLimitListener struct {
	net.Listener
	sem   chan struct{}
	perIP int // max concurrent connections per remote IP
	done  chan struct{}
	once  sync.Once

	mu   sync.Mutex
	byIP map[string]int
}

func newConnLimitListener(l net.Listener, max, perIP int) *connLimitListener {
	return &connLimitListener{Listener: l, sem: make(chan struct{}, max), perIP: perIP,
		done: make(chan struct{}), byIP: map[string]int{}}
}

func (l *connLimitListener) Accept() (net.Conn, error) {
	for {
		select {
		case l.sem <- struct{}{}:
		case <-l.done:
			return nil, net.ErrClosed
		}
		c, err := l.Listener.Accept()
		if err != nil {
			<-l.sem
			return nil, err
		}
		ip := web.ClientKey(c.RemoteAddr().String())
		l.mu.Lock()
		over := l.byIP[ip] >= l.perIP
		if !over {
			l.byIP[ip]++
		}
		l.mu.Unlock()
		if over {
			c.Close()
			<-l.sem
			continue
		}
		return &limitedConn{Conn: c, release: func() {
			l.mu.Lock()
			if l.byIP[ip]--; l.byIP[ip] <= 0 {
				delete(l.byIP, ip)
			}
			l.mu.Unlock()
			<-l.sem
		}}, nil
	}
}

func (l *connLimitListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return l.Listener.Close()
}

// limitedConn releases its slot on the first Close.
type limitedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

// CloseWrite forwards the half-close so net/http can send a FIN after an
// oversized request.
func (c *limitedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// admitGate rejects connections from peers that are not admitted (unpaired,
// pairing window closed), warning once per peer.
type admitGate struct {
	admit  func(bt.Addr) bool
	auth   func(bt.Addr) // called for admitted peers on authenticated PSMs; may be nil
	mu     sync.Mutex
	warned map[bt.Addr]bool
}

func newAdmitGate(admit func(bt.Addr) bool) *admitGate {
	return &admitGate{admit: admit, warned: map[bt.Addr]bool{}}
}

// allow reports whether a may connect; a nil gate allows everyone.
func (g *admitGate) allow(psm uint16, a bt.Addr) bool {
	if g == nil {
		return true
	}
	ok := g.admit(a)
	g.mu.Lock()
	warn := !ok && !g.warned[a]
	if ok {
		delete(g.warned, a)
	} else if warn {
		if len(g.warned) >= 64 { // bounded: strangers must not grow it
			clear(g.warned)
		}
		g.warned[a] = true
	}
	g.mu.Unlock()
	if warn {
		slog.Warn("connection refused: not paired and pairing window closed", "psm", psm, "peer", a.String())
	}
	return ok
}

// accepted reports an admitted connection that also got a connection slot.
// Called after the limiter so a reconnect loop cannot pile up auth goroutines.
func (g *admitGate) accepted(psm uint16, a bt.Addr) {
	if g != nil && psm != sdp.PSM && g.auth != nil { // AVDTP/AVCTP listeners require BT_SECURITY_MEDIUM; SDP does not
		g.auth(a)
	}
}

func acceptLoop(l *hci.Listener, handle func(bt.Conn), lim *limiter, gate *admitGate) {
	var errLogged time.Time // a persistent error (EMFILE) logs once a minute, not every retry
	for {
		c, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			if time.Since(errLogged) >= time.Minute {
				errLogged = time.Now()
				slog.Error("accept", "psm", l.PSM(), "err", err)
			}
			time.Sleep(time.Second)
			continue
		}
		errLogged = time.Time{}
		peer := c.RemoteAddr()
		if !gate.allow(l.PSM(), peer) {
			c.Close()
			continue
		}
		if !lim.acquire(peer) {
			if slog.Default().Enabled(context.Background(), slog.LevelDebug) {
				slog.Debug("l2cap connection limit, closing", "psm", l.PSM(), "peer", peer.String())
			}
			c.Close()
			continue
		}
		gate.accepted(l.PSM(), peer)
		if slog.Default().Enabled(context.Background(), slog.LevelDebug) {
			slog.Debug("l2cap connection", "psm", l.PSM(), "peer", peer.String(), "imtu", c.InMTU(), "omtu", c.OutMTU())
		}
		go func() {
			defer lim.release(peer)
			defer c.Close()
			handle(c)
		}()
	}
}
