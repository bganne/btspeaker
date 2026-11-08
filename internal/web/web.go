// Package web serves the JSON API and the embedded single-page UI.
package web

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"btspeaker/internal/bt"
	"btspeaker/internal/mgmt"
)

//go:embed index.html
var indexHTML string

// The UI is server-rendered (no JavaScript): plain forms POST to /ui/... and redirect back.
var indexTmpl = template.Must(template.New("index").Parse(indexHTML))

// page is the template data for the UI.
type page struct {
	Status
	Flash   string  // error from the last UI action
	Confirm *Device // set on the forget confirmation page
}

// DeviceName returns the name of the device with address addr, or addr itself.
func (p page) DeviceName(addr string) string {
	for _, d := range p.Devices {
		if d.Addr == addr && d.Name != "" {
			return d.Name
		}
	}
	return addr
}

func (p page) VolumePercent() int { return (p.Volume*100 + 63) / 127 }

// Status is the /api/status payload.
type Status struct {
	Adapter Adapter  `json:"adapter"`
	Pairing Pairing  `json:"pairing"`
	Devices []Device `json:"devices"`
	Stream  *Stream  `json:"stream,omitempty"`
	Volume  int      `json:"volume"` // 0..127 (AVRCP scale)
	// VolumeIgnored: bit-perfect output (-volume off), Volume is not applied.
	VolumeIgnored bool        `json:"volume_ignored,omitempty"`
	LastError     string      `json:"last_error,omitempty"`
	Diag          Diagnostics `json:"diagnostics"`
}

// Diagnostics are cumulative audio-path counters since start.
type Diagnostics struct {
	Xruns           int64  `json:"xruns"`             // ALSA underruns (clock drift / starvation)
	QueueDropFrames int64  `json:"queue_drop_frames"` // frames dropped by the player queue (>100 ms backlog)
	LostPackets     uint64 `json:"lost_packets"`      // RTP gaps
	BadFrames       uint64 `json:"bad_frames"`        // undecodable/out-of-spec SBC
	ConcealedMs     uint64 `json:"concealed_ms"`      // silence substituted for lost/bad audio
	DroppedDup      uint64 `json:"dropped_dup"`       // duplicate/out-of-order RTP packets dropped
	Resyncs         uint64 `json:"resyncs"`           // RTP sequence jumps that restarted the decoder
}

type Adapter struct {
	Name    string `json:"name"`
	Addr    string `json:"addr"`
	Present bool   `json:"present"`
	Powered bool   `json:"powered"`
}

type Pairing struct {
	Open             bool `json:"open"`
	RemainingSeconds int  `json:"remaining_seconds"`
	Always           bool `json:"always"`
}

type Device struct {
	Addr      string    `json:"addr"`
	Name      string    `json:"name,omitempty"`
	Connected bool      `json:"connected"`
	LastSeen  time.Time `json:"last_seen,omitzero"`
}

type Stream struct {
	Device     string `json:"device"`
	State      string `json:"state"`
	SampleRate int    `json:"sample_rate"`
	Channels   int    `json:"channels"`
	Mode       string `json:"mode,omitempty"`
	Bitpool    int    `json:"bitpool,omitempty"`
	Playing    bool   `json:"playing"` // ALSA device currently held
}

// Backend is what the handlers drive; main implements it.
type Backend interface {
	Status() Status
	// OpenPairing opens the window for d (0 = default timeout).
	OpenPairing(d time.Duration) error
	ClosePairing() error
	Disconnect(addr bt.Addr) error
	Forget(addr bt.Addr) error
	SetName(name string) error
}

// Config configures the handler. An empty Password disables authentication.
type Config struct {
	User     string // default "admin"
	Password string
	// Hosts lists the hostnames accepted in the Host header (IP literals are always
	// accepted); nil disables the check. Blocks DNS rebinding.
	Hosts []string
}

// authFailDelay slows down password guessing.
var authFailDelay = 750 * time.Millisecond

// failLimiter rate-limits failed authentications per client (parallel guessing is not
// slowed by authFailDelay). While a client's bucket is empty, its requests are refused
// with 429 before the credentials are even compared. Clients are keyed by IPv4 address
// or IPv6 /64, so one LAN host spamming bad credentials cannot lock the owner out.
// At most authFailClients buckets are tracked; full (idle) buckets are pruned, and when
// the table is still full, new clients share one overflow bucket rather than evicting
// someone (which would reset an attacker's bucket).
type failLimiter struct {
	mu       sync.Mutex
	clients  map[string]*bucket
	overflow bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

const (
	authFailRate    = 5.0 // failed attempts per second
	authFailBurst   = 10.0
	authFailClients = 64
)

func newFailLimiter() *failLimiter {
	return &failLimiter{clients: map[string]*bucket{}, overflow: bucket{tokens: authFailBurst, last: time.Now()}}
}

func (b *bucket) refill(now time.Time) {
	b.tokens = min(authFailBurst, b.tokens+now.Sub(b.last).Seconds()*authFailRate)
	b.last = now
}

// ClientKey is the IPv4 address or IPv6 /64 of a RemoteAddr (zone dropped).
func ClientKey(remote string) string {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	a = a.WithZone("").Unmap()
	if a.Is4() {
		return a.String()
	}
	return netip.PrefixFrom(a, 64).Masked().String()
}

// prune drops idle (fully refilled) buckets once the table is full; l.mu must be held.
func (l *failLimiter) prune(now time.Time) {
	if len(l.clients) < authFailClients {
		return
	}
	for k, c := range l.clients {
		if c.refill(now); c.tokens >= authFailBurst {
			delete(l.clients, k)
		}
	}
}

// get returns the client's bucket, refilled; l.mu must be held.
func (l *failLimiter) get(key string, create bool, now time.Time) *bucket {
	b := l.clients[key]
	if b == nil {
		if !create {
			return nil
		}
		l.prune(now)
		if len(l.clients) >= authFailClients {
			b = &l.overflow
		} else {
			b = &bucket{tokens: authFailBurst, last: now}
			l.clients[key] = b
		}
	}
	b.refill(now)
	return b
}

// blocked reports whether the client's bucket is empty.
func (l *failLimiter) blocked(remote string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b := l.get(ClientKey(remote), false, now)
	if b == nil {
		l.prune(now)
	}
	if b == nil && len(l.clients) >= authFailClients {
		b = &l.overflow
		b.refill(now)
	}
	return b != nil && b.tokens < 1
}

func (l *failLimiter) fail(remote string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.get(ClientKey(remote), true, time.Now())
	b.tokens = max(0, b.tokens-1)
}

// hostGuard rejects requests whose Host is neither an IP literal nor an allowed name.
func hostGuard(hosts []string, next http.Handler) http.Handler {
	if hosts == nil {
		return next
	}
	allowed := map[string]bool{}
	for _, h := range hosts {
		allowed[strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), "."))] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Host
		if hh, _, err := net.SplitHostPort(h); err == nil {
			h = hh
		}
		h = strings.Trim(h, "[]")
		if _, err := netip.ParseAddr(h); err != nil && !allowed[strings.ToLower(strings.TrimSuffix(h, "."))] {
			http.Error(w, "unknown host (add it to -http-hosts / BTSPEAKER_HTTP_HOSTS)", http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Handler builds the HTTP handler for all routes, wrapped in basic auth if configured.
func Handler(b Backend, cfg Config) http.Handler {
	s := &server{b: b}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /ui/devices/{addr}/forget", s.confirmForget)
	mux.HandleFunc("POST /ui/pairing/open", s.ui(func(*http.Request) error { return b.OpenPairing(0) }))
	mux.HandleFunc("POST /ui/pairing/close", s.ui(func(*http.Request) error { return b.ClosePairing() }))
	mux.HandleFunc("POST /ui/devices/{addr}/disconnect", s.ui(func(r *http.Request) error {
		a, err := bt.ParseAddr(r.PathValue("addr"))
		if err != nil {
			return errBadAddr
		}
		return b.Disconnect(a)
	}))
	mux.HandleFunc("POST /ui/devices/{addr}/forget", s.ui(func(r *http.Request) error {
		a, err := bt.ParseAddr(r.PathValue("addr"))
		if err != nil {
			return errBadAddr
		}
		return b.Forget(a)
	}))
	mux.HandleFunc("POST /ui/name", s.ui(func(r *http.Request) error {
		name, err := mgmt.ValidName(r.PostFormValue("name"))
		if err != nil {
			return err
		}
		return b.SetName(name)
	}))
	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("POST /api/pairing", s.openPairing)
	mux.HandleFunc("DELETE /api/pairing", s.closePairing)
	mux.HandleFunc("POST /api/devices/{addr}/disconnect", s.disconnect)
	mux.HandleFunc("DELETE /api/devices/{addr}", s.forget)
	mux.HandleFunc("POST /api/name", s.setName)
	// Form POSTs carry the browser's basic-auth credentials, so reject cross-site requests.
	return securityHeaders(hostGuard(cfg.Hosts, basicAuth(cfg, http.NewCrossOriginProtection().Handler(mux))))
}

// IdleTimeout is the keep-alive idle timeout the HTTP server should use: short, so a
// few idle clients cannot hold all connection slots.
const IdleTimeout = 10 * time.Second

// securityHeaders forbids framing (clickjacking) and sniffing. The UI has no scripts.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

func basicAuth(cfg Config, next http.Handler) http.Handler {
	if cfg.Password == "" {
		return next
	}
	user := cfg.User
	if user == "" {
		user = "admin"
	}
	wantU, wantP := sha256.Sum256([]byte(user)), sha256.Sum256([]byte(cfg.Password))
	lim := newFailLimiter()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A request without credentials (the browser's first visit) costs nothing and
		// is not counted, so a LAN host cannot keep the owner locked out.
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", `Basic realm="btspeaker", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if lim.blocked(r.RemoteAddr) {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "too many failed attempts", http.StatusTooManyRequests)
			return
		}
		u, p, ok := r.BasicAuth()
		// Evaluate both comparisons unconditionally.
		// Compare fixed-length digests so the length of the secrets does not leak.
		hu, hp := sha256.Sum256([]byte(u)), sha256.Sum256([]byte(p))
		okU := subtle.ConstantTimeCompare(hu[:], wantU[:])
		okP := subtle.ConstantTimeCompare(hp[:], wantP[:])
		if !ok || okU&okP != 1 {
			lim.fail(r.RemoteAddr)
			time.Sleep(authFailDelay)
			w.Header().Set("WWW-Authenticate", `Basic realm="btspeaker", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type server struct {
	b Backend
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// writeErr reports err to the client. 5xx details (device paths, syscall errors) stay
// in the log; 4xx texts describe the caller's own mistake and are returned as is.
func writeErr(w http.ResponseWriter, code int, err error) {
	msg := err.Error()
	if code >= 500 {
		slog.Error("web: api action failed", "err", err)
		msg = "action failed"
	}
	writeJSON(w, code, map[string]string{"error": msg})
}

// writeActionErr answers a failed device action: unknown device is 404, a
// device that is not connected 409 (client errors, not logged as failures),
// anything else 500.
func writeActionErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, mgmt.ErrUnknownDevice):
		slog.Info("web: api action rejected", "err", err)
		writeErr(w, http.StatusNotFound, err)
	case errors.Is(err, mgmt.ErrNotConnected):
		slog.Info("web: api action rejected", "err", err)
		writeErr(w, http.StatusConflict, err)
	default:
		writeErr(w, http.StatusInternalServerError, err)
	}
}

func ok(w http.ResponseWriter) { writeJSON(w, http.StatusOK, map[string]bool{"ok": true}) }

// Errors from UI actions reach the page only as one of these codes (?msg=code), never as
// free text, so a crafted link cannot show attacker-chosen wording.
var flashMessages = map[string]string{
	"bad_addr":      "Invalid device address.",
	"unknown":       "Unknown device.",
	"not_connected": "The device is not connected.",
	"invalid_name":  "Invalid name (1..248 bytes, no control or bidi format characters).",
	"failed":        "The action failed; see the service log.",
}

var (
	errBadAddr     = errors.New("bad address")
	errInvalidName = mgmt.ErrInvalidName
)

// flashCode maps an action error to a flashMessages key.
func flashCode(err error) string {
	switch {
	case errors.Is(err, errBadAddr):
		return "bad_addr"
	case errors.Is(err, errInvalidName):
		return "invalid_name"
	case errors.Is(err, mgmt.ErrUnknownDevice):
		return "unknown"
	case errors.Is(err, mgmt.ErrNotConnected):
		return "not_connected"
	}
	return "failed"
}

func (s *server) index(w http.ResponseWriter, r *http.Request) {
	s.render(w, page{Status: s.b.Status(), Flash: flashMessages[r.URL.Query().Get("msg")]})
}

// confirmForget shows the "forget this device?" page; the POST does the work.
func (s *server) confirmForget(w http.ResponseWriter, r *http.Request) {
	a, err := bt.ParseAddr(r.PathValue("addr"))
	if err != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	p := page{Status: s.b.Status()}
	p.Confirm = &Device{Addr: a.String()}
	for _, d := range p.Devices {
		if d.Addr == p.Confirm.Addr {
			p.Confirm = &d
		}
	}
	s.render(w, p)
}

func (s *server) render(w http.ResponseWriter, p page) {
	var buf bytes.Buffer
	if err := indexTmpl.Execute(&buf, p); err != nil {
		slog.Error("web: render", "err", err)
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(buf.Bytes())
}

// ui wraps a form action: run it, redirect to / carrying any error as a fixed ?msg= code.
func (s *server) ui(fn func(*http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		err := fn(r)
		dest := "/"
		if err != nil {
			slog.Warn("web: action failed", "path", r.URL.Path, "err", err)
			dest = "/?msg=" + flashCode(err)
		}
		http.Redirect(w, r, dest, http.StatusSeeOther)
	}
}

func (s *server) status(w http.ResponseWriter, r *http.Request) {
	st := s.b.Status()
	if st.Devices == nil {
		st.Devices = []Device{}
	}
	writeJSON(w, http.StatusOK, st)
}

// decode reads an optional small JSON body into v. An empty body is fine.
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("unexpected data after JSON body")
	}
	return nil
}

func (s *server) openPairing(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Seconds int `json:"seconds"`
	}
	if err := decode(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Seconds < 0 || req.Seconds > 3600 {
		writeErr(w, http.StatusBadRequest, errors.New("seconds must be 0..3600"))
		return
	}
	if err := s.b.OpenPairing(time.Duration(req.Seconds) * time.Second); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	ok(w)
}

func (s *server) closePairing(w http.ResponseWriter, r *http.Request) {
	if err := s.b.ClosePairing(); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	ok(w)
}

func (s *server) disconnect(w http.ResponseWriter, r *http.Request) {
	addr, err := bt.ParseAddr(r.PathValue("addr"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.b.Disconnect(addr); err != nil {
		writeActionErr(w, err)
		return
	}
	ok(w)
}

func (s *server) forget(w http.ResponseWriter, r *http.Request) {
	addr, err := bt.ParseAddr(r.PathValue("addr"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.b.Forget(addr); err != nil {
		writeActionErr(w, err)
		return
	}
	ok(w)
}

func (s *server) setName(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := decode(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	name, err := mgmt.ValidName(req.Name)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.b.SetName(name); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	ok(w)
}
