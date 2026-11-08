package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"btspeaker/internal/bt"
	"btspeaker/internal/mgmt"
)

type fake struct {
	opened       []time.Duration
	closed       int
	disc, forgot []bt.Addr
	name         string
	err          error
	st           *Status
}

func (f *fake) Status() Status {
	if f.st != nil {
		return *f.st
	}
	return Status{Adapter: Adapter{Name: "Hifi", Addr: "AA:BB:CC:DD:EE:FF", Present: true}}
}
func (f *fake) OpenPairing(d time.Duration) error { f.opened = append(f.opened, d); return f.err }
func (f *fake) ClosePairing() error               { f.closed++; return f.err }
func (f *fake) Disconnect(a bt.Addr) error        { f.disc = append(f.disc, a); return f.err }
func (f *fake) Forget(a bt.Addr) error            { f.forgot = append(f.forgot, a); return f.err }
func (f *fake) SetName(n string) error            { f.name = n; return f.err }

func do(h http.Handler, method, path, body string, auth ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if len(auth) == 2 {
		r.SetBasicAuth(auth[0], auth[1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestEndpoints(t *testing.T) {
	f := &fake{}
	h := Handler(f, Config{})
	if w := do(h, "GET", "/", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "<html") {
		t.Fatalf("index %d", w.Code)
	}
	if w := do(h, "GET", "/api/status", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"devices":[]`) {
		t.Fatalf("status %d %s", w.Code, w.Body)
	}
	if w := do(h, "POST", "/api/pairing", `{"seconds":90}`); w.Code != 200 || len(f.opened) != 1 || f.opened[0] != 90*time.Second {
		t.Fatalf("pairing %d %v", w.Code, f.opened)
	}
	if w := do(h, "POST", "/api/pairing", ``); w.Code != 200 || f.opened[1] != 0 {
		t.Fatalf("pairing default %d", w.Code)
	}
	if w := do(h, "POST", "/api/pairing", `{"seconds":-1}`); w.Code != 400 {
		t.Fatalf("neg seconds %d", w.Code)
	}
	if w := do(h, "DELETE", "/api/pairing", ""); w.Code != 200 || f.closed != 1 {
		t.Fatal("close pairing")
	}
	if w := do(h, "POST", "/api/devices/11:22:33:44:55:66/disconnect", ""); w.Code != 200 || len(f.disc) != 1 || f.disc[0][0] != 0x11 {
		t.Fatalf("disconnect %d", w.Code)
	}
	if w := do(h, "DELETE", "/api/devices/11:22:33:44:55:66", ""); w.Code != 200 || len(f.forgot) != 1 {
		t.Fatalf("forget %d", w.Code)
	}
	if w := do(h, "DELETE", "/api/devices/nonsense", ""); w.Code != 400 {
		t.Fatalf("bad addr %d", w.Code)
	}
	if w := do(h, "POST", "/api/name", `{"name":" Living room "}`); w.Code != 200 || f.name != "Living room" {
		t.Fatalf("name %d %q", w.Code, f.name)
	}
	for _, body := range []string{`{"name":""}`, `{"name":"a\nb"}`, `{bad`} {
		if w := do(h, "POST", "/api/name", body); w.Code != 400 {
			t.Fatalf("name %s -> %d", body, w.Code)
		}
	}
	if w := do(h, "PUT", "/api/name", ""); w.Code != 405 {
		t.Fatalf("method %d", w.Code)
	}
	for _, body := range []string{`{"seconds":5} x`, `{"seconds":5}{}`, `{"secs":5}`} {
		if w := do(h, "POST", "/api/pairing", body); w.Code != 400 {
			t.Fatalf("body %s -> %d", body, w.Code)
		}
	}
	f.err = fmt.Errorf("%w: x", mgmt.ErrUnknownDevice)
	if w := do(h, "DELETE", "/api/devices/11:22:33:44:55:66", ""); w.Code != 404 {
		t.Fatalf("unknown device %d", w.Code)
	}
	f.err = fmt.Errorf("%w: x", mgmt.ErrNotConnected)
	if w := do(h, "POST", "/api/devices/11:22:33:44:55:66/disconnect", ""); w.Code != 409 {
		t.Fatalf("not connected %d", w.Code)
	}
	if w := form(h, "/ui/devices/11:22:33:44:55:66/disconnect", ""); !strings.Contains(w.Header().Get("Location"), "msg=not_connected") {
		t.Fatalf("ui flash %q", w.Header().Get("Location"))
	}
	f.err = errors.New("boom")
	if w := do(h, "POST", "/api/pairing", ``); w.Code != 500 || strings.Contains(w.Body.String(), "boom") || !strings.Contains(w.Body.String(), "action failed") {
		t.Fatalf("backend error %d", w.Code)
	}
}

func form(h http.Handler, path, body string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestUI(t *testing.T) {
	f := &fake{st: &Status{
		Adapter: Adapter{Name: "Hifi <b>", Addr: "AA:BB:CC:DD:EE:FF", Present: true, Powered: true},
		Devices: []Device{{Addr: "11:22:33:44:55:66", Name: "Phone", Connected: true}, {Addr: "11:22:33:44:55:77"}},
		Stream:  &Stream{Device: "11:22:33:44:55:66", State: "streaming", SampleRate: 44100, Playing: true},
		Volume:  127,
	}}
	h := Handler(f, Config{})
	body := do(h, "GET", "/", "").Body.String()
	for _, want := range []string{"Hifi &lt;b&gt;", "Phone", "/ui/devices/11:22:33:44:55:66/disconnect",
		`href="/ui/devices/11:22:33:44:55:77/forget"`, "Phone: streaming, 44100 Hz (playing)", "Volume 100%", "/ui/pairing/open"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(body, "<script") || strings.Contains(body, "http-equiv") {
		t.Error("page has script or refresh while pairing closed")
	}
	f.st.VolumeIgnored = true
	if b := do(h, "GET", "/", "").Body.String(); strings.Contains(b, "<meter") || !strings.Contains(b, "volume ignored") {
		t.Error("bit-perfect: volume meter shown")
	}
	f.st.VolumeIgnored = false

	for _, c := range []struct {
		path, body string
		check      func() bool
	}{
		{"/ui/pairing/open", "", func() bool { return len(f.opened) == 1 && f.opened[0] == 0 }},
		{"/ui/pairing/close", "", func() bool { return f.closed == 1 }},
		{"/ui/devices/11:22:33:44:55:66/disconnect", "", func() bool { return len(f.disc) == 1 }},
		{"/ui/devices/11:22:33:44:55:66/forget", "", func() bool { return len(f.forgot) == 1 }},
		{"/ui/name", "name=+Living+room+", func() bool { return f.name == "Living room" }},
	} {
		w := form(h, c.path, c.body)
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" || !c.check() {
			t.Errorf("%s: %d %q", c.path, w.Code, w.Header().Get("Location"))
		}
	}

	if b := do(h, "GET", "/ui/devices/11:22:33:44:55:66/forget", "").Body.String(); !strings.Contains(b, "Forget <b>Phone</b>") ||
		!strings.Contains(b, `action="/ui/devices/11:22:33:44:55:66/forget"`) || strings.Contains(b, "Rename") {
		t.Errorf("confirm page:\n%s", b)
	}
	if w := do(h, "GET", "/ui/devices/bogus/forget", ""); w.Code != http.StatusSeeOther {
		t.Errorf("confirm bad addr: %d", w.Code)
	}

	// Errors are shown once on the next page view.
	loc := form(h, "/ui/name", "name=").Header().Get("Location")
	if !strings.HasPrefix(loc, "/?msg=") {
		t.Fatalf("redirect %q", loc)
	}
	if b := do(h, "GET", loc, "").Body.String(); !strings.Contains(b, "Invalid name") {
		t.Error("flash not shown")
	}
	if b := do(h, "GET", "/", "").Body.String(); strings.Contains(b, "invalid name") {
		t.Error("flash leaked to other client")
	}
	if b := do(h, "GET", "/?msg=Send+money+to+evil", "").Body.String(); strings.Contains(b, "evil") || strings.Contains(b, `class="err"`) {
		t.Error("arbitrary flash text shown")
	}
	loc = form(h, "/ui/devices/bogus/forget", "").Header().Get("Location")
	if b := do(h, "GET", loc, "").Body.String(); !strings.Contains(b, "class=\"err\"") {
		t.Error("bad addr not reported")
	}

	// Cross-site form posts are rejected; same-origin ones pass.
	n := len(f.opened)
	if w := form(h, "/ui/pairing/open", "", "Sec-Fetch-Site", "cross-site"); w.Code != http.StatusForbidden || len(f.opened) != n {
		t.Errorf("cross-site post: %d", w.Code)
	}
	if w := form(h, "/ui/pairing/open", "", "Sec-Fetch-Site", "same-origin"); w.Code != http.StatusSeeOther {
		t.Errorf("same-origin post: %d", w.Code)
	}

	f.st.Pairing = Pairing{Open: true, RemainingSeconds: 42}
	if b := do(h, "GET", "/", "").Body.String(); !strings.Contains(b, "42 s left") || !strings.Contains(b, "http-equiv=\"refresh\"") {
		t.Error("pairing open: no countdown/refresh")
	}
}

func TestHostCheck(t *testing.T) {
	h := Handler(&fake{}, Config{Hosts: []string{"cubox", "cubox.local"}})
	for host, want := range map[string]int{"cubox": 200, "CUBOX.local:8099": 200, "192.168.1.5:8099": 200,
		"[::1]:8099": 200, "[::1]": 200, "cubox.": 200, "cubox.local.:8099": 200, "cubox:": 200, "evil.example.com.": 421, "evil.example.com": 421, "evil.example.com:8099": 421} {
		r := httptest.NewRequest("GET", "/api/status", nil)
		r.Host = host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("host %q: %d, want %d", host, w.Code, want)
		}
	}
}

func TestAuth(t *testing.T) {
	authFailDelay = 0
	h := Handler(&fake{}, Config{Password: "s3cret"})
	for _, p := range [][2]string{{"GET", "/"}, {"GET", "/api/status"}, {"POST", "/api/pairing"}, {"DELETE", "/api/devices/11:22:33:44:55:66"}, {"GET", "/nope"}} {
		w := do(h, p[0], p[1], "")
		if w.Code != 401 || w.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%v without creds: %d", p, w.Code)
		}
	}
	if w := do(h, "GET", "/api/status", "", "admin", "wrong"); w.Code != 401 {
		t.Error("wrong password accepted")
	}
	if w := do(h, "GET", "/api/status", "", "root", "s3cret"); w.Code != 401 {
		t.Error("wrong user accepted")
	}
	if w := do(h, "GET", "/api/status", "", "admin", "s3cret"); w.Code != 200 {
		t.Errorf("good creds: %d", w.Code)
	}
	// A burst of failures trips the client's limiter, even for the right password.
	got429 := false
	for range 30 {
		if do(h, "GET", "/api/status", "", "admin", "wrong").Code == 429 {
			got429 = true
		}
	}
	if !got429 || do(h, "GET", "/api/status", "", "admin", "s3cret").Code != 429 {
		t.Error("failed-auth rate limit not applied")
	}
	// The limit is per client: another host is not locked out.
	r := httptest.NewRequest("GET", "/api/status", nil)
	r.RemoteAddr = "192.0.2.99:5555"
	r.SetBasicAuth("admin", "s3cret")
	w := httptest.NewRecorder()
	if h.ServeHTTP(w, r); w.Code != 200 {
		t.Errorf("other client locked out: %d", w.Code)
	}
	h = Handler(&fake{}, Config{User: "bob", Password: "x"})
	if w := do(h, "GET", "/", "", "bob", "x"); w.Code != 200 {
		t.Error("custom user rejected")
	}
}

func TestAuthNoHeaderNotCounted(t *testing.T) {
	authFailDelay = 0
	h := Handler(&fake{}, Config{Password: "s3cret"})
	for range 50 {
		if w := do(h, "GET", "/api/status", ""); w.Code != 401 {
			t.Fatalf("no creds: %d", w.Code)
		}
	}
	if w := do(h, "GET", "/api/status", "", "admin", "s3cret"); w.Code != 200 {
		t.Errorf("owner locked out by anonymous requests: %d", w.Code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := Handler(&fake{}, Config{})
	w := do(h, "GET", "/", "")
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") ||
		w.Header().Get("X-Frame-Options") != "DENY" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("headers: %v", w.Header())
	}
}

func TestFailLimiterBounded(t *testing.T) {
	l := newFailLimiter()
	for i := range 3 * authFailClients {
		l.fail(fmt.Sprintf("[2001:db8:%x::1]:1", i))
	}
	if len(l.clients) > authFailClients {
		t.Fatalf("%d clients tracked", len(l.clients))
	}
	if ClientKey("[2001:db8::1]:1") != ClientKey("[2001:db8::2]:2") {
		t.Error("IPv6 not keyed by /64")
	}
	if ClientKey("[fe80::1%eth0]:1") != ClientKey("[fe80::2%eth1]:2") {
		t.Error("zoned IPv6 not keyed by /64")
	}
	if ClientKey("[::ffff:192.0.2.1]:1") != ClientKey("192.0.2.1:2") {
		t.Error("v4-mapped not unmapped")
	}
}

func TestValidNameRejectsBidi(t *testing.T) {
	for _, n := range []string{"evil‮gnp", "a\x7fb", "a\u0085b"} {
		if _, err := mgmt.ValidName(n); err == nil {
			t.Errorf("mgmt.ValidName(%q) accepted", n)
		}
	}
	if _, err := mgmt.ValidName("👨‍👩‍👧 Café"); err != nil {
		t.Errorf("valid name rejected: %v", err)
	}
}
