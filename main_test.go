package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"btspeaker/internal/alsa"
	"btspeaker/internal/bt"
)

func TestEnvOverrides(t *testing.T) {
	env := map[string]string{
		"BTSPEAKER_NAME":            "FromEnv",
		"BTSPEAKER_ALSA_DEVICE":     "hw:Spdif,0",
		"BTSPEAKER_BUFFER":          "300ms",
		"BTSPEAKER_ALWAYS_PAIRABLE": "true",
		"BTSPEAKER_PASSWORD":        "pw",
	}
	get := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	o, err := parseOptions([]string{"-alsa-device", "hw:1,0"}, get)
	if err != nil {
		t.Fatal(err)
	}
	if o.name != "FromEnv" || o.alsaDevice != "hw:1,0" || o.buffer.Milliseconds() != 300 || !o.alwaysPairable || o.httpPassword != "pw" {
		t.Fatalf("%+v", o)
	}
	if o.listen != ":8099" || o.volume != "software" {
		t.Fatalf("defaults: %+v", o)
	}
	if _, err := parseOptions([]string{"-volume", "loud"}, get); err == nil {
		t.Fatal("want volume error")
	}
}

func TestUnknownEnv(t *testing.T) {
	o, err := parseOptions(nil, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	got := unknownEnv(o.envNames, []string{"BTSPEAKER_HTTP_PASSWORD=x", "BTSPEAKER_HTTP_PASSWD=x",
		"BTSPEAKER_PASSWORD=x", "BTSPEAKER_ALSA_DEVICE=hw:1,0", "HOME=/root", "BTSPEAKER_X"})
	if !slices.Equal(got, []string{"BTSPEAKER_HTTP_PASSWD", "BTSPEAKER_X"}) {
		t.Fatalf("unknown %v", got)
	}
}

func TestLimiter(t *testing.T) {
	l := newLimiter(3, 2)
	a, b := bt.Addr{1}, bt.Addr{2}
	if !l.acquire(a) || !l.acquire(a) || l.acquire(a) {
		t.Fatal("per-peer cap")
	}
	if !l.acquire(b) || l.acquire(bt.Addr{3}) {
		t.Fatal("total cap")
	}
	l.release(a)
	if !l.acquire(bt.Addr{3}) {
		t.Fatal("slot not released")
	}
}

func TestAllowedHosts(t *testing.T) {
	if h := allowedHosts(" a , b,"); len(h) != 2 || h[0] != "a" || h[1] != "b" {
		t.Errorf("%v", h)
	}
	if h := allowedHosts(""); len(h) == 0 || h[0] != "localhost" {
		t.Errorf("%v", h)
	}
}

func TestAllowedHostsDefaultFQDN(t *testing.T) {
	old := defaultName
	defaultName = func() string { return "cubox.lan" }
	t.Cleanup(func() { defaultName = old })
	found := false
	for _, h := range allowedHosts("") {
		found = found || h == "cubox.lan"
	}
	if !found {
		t.Errorf("FQDN missing: %v", allowedHosts(""))
	}
}

func TestPrerollClampedToSmallBuffer(t *testing.T) {
	noEnv := func(string) (string, bool) { return "", false }
	o, err := parseOptions([]string{"-buffer", "100ms"}, noEnv)
	if err != nil || o.spdifPreroll != 100*time.Millisecond {
		t.Fatalf("preroll %v, err %v", o.spdifPreroll, err)
	}
	if _, err := parseOptions([]string{"-buffer", "100ms", "-spdif-preroll", "200ms"}, noEnv); err == nil {
		t.Error("explicit preroll > buffer accepted")
	}
	env := func(k string) (string, bool) {
		if k == "BTSPEAKER_SPDIF_PREROLL" {
			return "500ms", true
		}
		return "", false
	}
	if o, err := parseOptions(nil, env); err == nil {
		t.Errorf("env preroll > buffer accepted (preroll %v)", o.spdifPreroll)
	}
}

func TestInvalidDefaultNameFallsBack(t *testing.T) {
	old := defaultName
	defaultName = func() string { return "   " }
	t.Cleanup(func() { defaultName = old })
	noEnv := func(string) (string, bool) { return "", false }
	o, err := parseOptions(nil, noEnv)
	if err != nil || o.name != "btspeaker" {
		t.Fatalf("name %q, err %v", o.name, err)
	}
	if _, err := parseOptions([]string{"-name", "  "}, noEnv); err == nil {
		t.Error("explicit invalid -name accepted")
	}
}

func TestConnLimitListener(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := newConnLimitListener(inner, 2, 2, nil)
	defer l.Close()
	var cs []net.Conn
	for range 3 {
		c, err := net.Dial("tcp", l.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		cs = append(cs, c)
	}
	a1, _ := l.Accept()
	a2, _ := l.Accept()
	got := make(chan net.Conn, 1)
	go func() { c, _ := l.Accept(); got <- c }()
	select {
	case <-got:
		t.Fatal("third Accept did not block at the cap")
	case <-time.After(100 * time.Millisecond):
	}
	a1.Close()
	a1.Close() // idempotent: must not free a second slot
	select {
	case c := <-got:
		if c == nil {
			t.Fatal("accept failed")
		}
		defer c.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("Accept still blocked after Close")
	}
	a2.Close()
	if len(l.sem) != 1 {
		t.Errorf("slots in use %d, want 1", len(l.sem))
	}
}

type cwConn struct {
	net.Conn
	cw int
}

func (c *cwConn) CloseWrite() error { c.cw++; return nil }

func TestLimitedConnCloseWrite(t *testing.T) {
	inner := &cwConn{}
	c := &limitedConn{Conn: inner}
	if err := c.CloseWrite(); err != nil || inner.cw != 1 {
		t.Fatalf("err=%v cw=%d", err, inner.cw)
	}
	if err := (&limitedConn{Conn: nil}).CloseWrite(); err != nil { // no CloseWrite: no-op
		t.Fatal(err)
	}
}

func TestAdmitGate(t *testing.T) {
	ok := bt.Addr{1}
	g := newAdmitGate(func(a bt.Addr) bool { return a == ok })
	if !g.allow(25, ok) || g.allow(25, bt.Addr{2}) || g.allow(23, bt.Addr{2}) {
		t.Fatal("gate decisions")
	}
	if len(g.warned) != 1 {
		t.Fatalf("warned %v", g.warned)
	}
	for i := 0; i < 100; i++ {
		g.allow(25, bt.Addr{3, byte(i)})
	}
	if len(g.warned) > 64 {
		t.Fatalf("warned map unbounded: %d", len(g.warned))
	}
	var nilGate *admitGate
	if !nilGate.allow(1, bt.Addr{9}) {
		t.Fatal("nil gate must allow")
	}
}

func TestConnLimitListenerPerIP(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := newConnLimitListener(inner, 10, 2, nil)
	defer l.Close()
	var cs []net.Conn
	for range 3 {
		c, err := net.Dial("tcp", l.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		cs = append(cs, c)
	}
	a1, _ := l.Accept()
	a2, _ := l.Accept()
	got := make(chan net.Conn, 1)
	go func() { c, _ := l.Accept(); got <- c }() // third from same IP is dropped, Accept keeps waiting
	select {
	case <-got:
		t.Fatal("per-IP cap not enforced")
	case <-time.After(200 * time.Millisecond):
	}
	a1.Close()
	a2.Close()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.byIP) != 0 {
		t.Errorf("not released: %v", l.byIP)
	}
}

func TestReprobeRates(t *testing.T) {
	calls := 0
	probe := func(string) (alsa.Caps, error) {
		if calls++; calls < 3 {
			return alsa.Caps{}, errors.New("busy")
		}
		return alsa.Caps{Rates: []int{48000}}, nil
	}
	got := make(chan []int, 2)
	reprobeRates(context.Background(), "hw:0,0", time.Millisecond, probe, func(r []int) { got <- r })
	if r := <-got; calls != 3 || len(r) != 1 || r[0] != 48000 {
		t.Fatalf("calls %d rates %v", calls, r)
	}
}

func TestAdmitGateAuth(t *testing.T) {
	ok := bt.Addr{1}
	var got []bt.Addr
	g := newAdmitGate(func(a bt.Addr) bool { return a == ok })
	g.auth = func(a bt.Addr) { got = append(got, a) }
	for _, c := range []struct {
		psm uint16
		a   bt.Addr
	}{{1, ok} /* SDP: not authenticated */, {25, bt.Addr{2}} /* refused */, {25, ok}, {23, ok}} {
		if g.allow(c.psm, c.a) {
			g.accepted(c.psm, c.a)
		}
	}
	var nilGate *admitGate
	nilGate.accepted(25, ok)
	if len(got) != 2 || got[0] != ok || got[1] != ok {
		t.Fatalf("auth calls %v", got)
	}
}

func TestFlagValidation(t *testing.T) {
	env := func(string) (string, bool) { return "", false }
	for _, args := range [][]string{
		{"-spdif-preroll", "500ms"},
		{"-name", "   "},
		{"-name", "a\x00b"},
		{"-name", strings.Repeat("x", 249)},
		{"-buffer", "10ms"},
		{"-buffer", "3s"},
		{"-buffer", "0"},
		{"-idle-release", "0"},
		{"-idle-release", "-1s"},
	} {
		if _, err := parseOptions(args, env); err == nil {
			t.Errorf("%v: want error", args)
		}
	}
	if _, err := parseOptions([]string{"-spdif-preroll", "500ms", "-buffer", "500ms"}, env); err != nil {
		t.Error(err)
	}
}

func TestFQDN(t *testing.T) {
	hosts := "127.0.0.1 localhost\n# 10.0.0.1 htpc.wrong htpc\n127.0.1.1\tbagend.benou.fr bagend # me\n"
	for _, c := range []struct{ host, hosts, resolv, want string }{
		{"bagend", hosts, "search home\n", "bagend.benou.fr"},     // /etc/hosts wins
		{"htpc", hosts, "search benou.fr lan\n", "htpc.benou.fr"}, // commented hosts line ignored
		{"htpc", "", "search a.b\ndomain c.d\n", "htpc.c.d"},      // domain over search
		{"htpc", "", "search .\n", "htpc"},                        // root search domain
		{"htpc", "", "", "htpc"},
		{"htpc.example.org", hosts, "search home\n", "htpc.example.org"}, // already qualified
		{"", "", "", "btspeaker"},
	} {
		if got := fqdn(c.host, c.hosts, c.resolv); got != c.want {
			t.Errorf("fqdn(%q) = %q, want %q", c.host, got, c.want)
		}
	}
}

func TestDefaultNameUsedWhenUnset(t *testing.T) {
	old := defaultName
	defaultName = func() string { return "htpc.benou.fr" }
	t.Cleanup(func() { defaultName = old })
	o, err := parseOptions(nil, func(string) (string, bool) { return "", false })
	if err != nil || o.name != "htpc.benou.fr" {
		t.Fatalf("name %q, err %v", o.name, err)
	}
}

func TestParseAllow(t *testing.T) {
	for _, bad := range []string{"", " , ", "10.0.0.0/33", "nope", "fe80::1%eth0", "::ffff:0.0.0.0/95"} {
		if _, err := parseAllow(bad); err == nil {
			t.Errorf("parseAllow(%q) accepted", bad)
		}
	}
	if p, err := parseAllow("private,any"); err != nil || p != nil {
		t.Errorf("any: %v %v", p, err)
	}
	priv, err := parseAllow("private")
	if err != nil {
		t.Fatal(err)
	}
	custom, err := parseAllow("203.0.113.7, 2001:db8::/32,10.1.2.3/8")
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := parseAllow("::ffff:192.168.1.5,::ffff:10.0.0.0/104")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		allow  []netip.Prefix
		remote string
		want   bool
	}{
		{nil, "8.8.8.8:1", true},
		{priv, "127.0.0.1:1", true},
		{priv, "192.168.1.5:1", true},
		{priv, "172.31.0.1:1", true},
		{priv, "172.32.0.1:1", false},
		{priv, "100.100.1.1:1", true},
		{priv, "[::ffff:10.0.0.1]:1", true},
		{priv, "[::1]:1", true},
		{priv, "[fd12::1]:1", true},
		{priv, "[fe80::1%eth0]:1", true},
		{priv, "8.8.8.8:1", false},
		{priv, "[2001:db8::1]:1", false},
		{priv, "garbage", false},
		{custom, "203.0.113.7:1", true},
		{custom, "203.0.113.8:1", false},
		{custom, "[2001:db8:1::1]:1", true},
		{custom, "10.200.0.1:1", true},
		{mapped, "192.168.1.5:1", true},
		{mapped, "[::ffff:192.168.1.5]:1", true},
		{mapped, "10.9.9.9:1", true},
		{mapped, "192.168.1.6:1", false},
	} {
		if got := peerAllowed(c.allow, c.remote); got != c.want {
			t.Errorf("peerAllowed(%v, %s) = %v", c.allow, c.remote, got)
		}
	}
}

func TestConnLimitListenerAllow(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := newConnLimitListener(inner, 2, 2, []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")})
	defer l.Close()
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	got := make(chan net.Conn, 1)
	go func() { a, _ := l.Accept(); got <- a }()
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err != io.EOF {
		t.Errorf("refused peer: read err %v, want EOF", err)
	}
	select {
	case a := <-got:
		t.Fatalf("refused peer accepted: %v", a)
	case <-time.After(50 * time.Millisecond):
	}
	if len(l.sem) != 1 { // only the pending Accept's reservation
		t.Errorf("slots in use %d, want 1", len(l.sem))
	}
}

// TestUnitAllowMatchesPrivateNets keeps the unit's IPAddressAllow and
// -http-allow=private in sync: a peer one admits must not be dropped by the other.
func TestUnitAllowMatchesPrivateNets(t *testing.T) {
	b, err := os.ReadFile("deploy/btspeaker.service")
	if err != nil {
		t.Fatal(err)
	}
	var unit []string
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "IPAddressAllow="); ok {
			for _, f := range strings.Fields(v) {
				switch f {
				case "localhost":
					unit = append(unit, "127.0.0.0/8", "::1/128")
				case "link-local":
					unit = append(unit, "169.254.0.0/16", "fe80::/10")
				default:
					unit = append(unit, netip.MustParsePrefix(f).Masked().String())
				}
			}
		}
	}
	var priv []string
	for _, p := range privateNets {
		priv = append(priv, p.String())
	}
	slices.Sort(unit)
	slices.Sort(priv)
	if !slices.Equal(unit, priv) {
		t.Fatalf("unit IPAddressAllow %v != privateNets %v", unit, priv)
	}
}
