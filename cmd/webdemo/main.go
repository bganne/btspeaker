// Command webdemo serves the web UI against an in-memory fake backend, for
// working on the HTTP API / UI without Bluetooth hardware:
//
//	go run ./cmd/webdemo -listen localhost:8099
package main

import (
	"errors"
	"flag"
	"log"
	"net/http"
	"sync"
	"time"

	"btspeaker/internal/bt"
	"btspeaker/internal/web"
)

type device struct {
	name      string
	connected bool
	lastSeen  time.Time
}

type fake struct {
	mu          sync.Mutex
	name        string
	pairUntil   time.Time
	devices     map[string]*device
	order       []string
	streaming   string // addr of the streaming device, "" if none
	lastError   string
	failActions bool
}

func newFake(failActions bool) *fake {
	now := time.Now()
	f := &fake{name: "Cubox HiFi", devices: map[string]*device{}, failActions: failActions}
	add := func(addr, name string, connected bool, seen time.Duration) {
		f.devices[addr] = &device{name: name, connected: connected, lastSeen: now.Add(-seen)}
		f.order = append(f.order, addr)
	}
	add("AA:BB:CC:00:00:01", "Pixel 9", true, 0)
	add("AA:BB:CC:00:00:02", "MacBook Air", false, 3*time.Hour)
	add("AA:BB:CC:00:00:03", "", false, 72*time.Hour)
	f.streaming = "AA:BB:CC:00:00:01"
	return f
}

func (f *fake) Status() web.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := web.Status{
		Adapter:   web.Adapter{Name: f.name, Addr: "00:1A:7D:DA:71:13", Present: true, Powered: true},
		Volume:    100,
		LastError: f.lastError,
		Diag:      web.Diagnostics{Xruns: 2, QueueDropFrames: 4410, LostPackets: 5, BadFrames: 1, ConcealedMs: 87, DroppedDup: 3, Resyncs: 1},
	}
	if rem := time.Until(f.pairUntil); rem > 0 {
		st.Pairing = web.Pairing{Open: true, RemainingSeconds: int(rem.Seconds() + 0.5)}
	}
	for _, a := range f.order {
		d := f.devices[a]
		st.Devices = append(st.Devices, web.Device{Addr: a, Name: d.name, Connected: d.connected, LastSeen: d.lastSeen})
	}
	if f.streaming != "" {
		// Alternate playing/idle every 10 s to exercise both UI states.
		playing := time.Now().Unix()/10%2 == 0
		st.Stream = &web.Stream{Device: f.streaming, State: "streaming", SampleRate: 44100,
			Channels: 2, Mode: "joint-stereo", Bitpool: 53, Playing: playing}
	}
	return st
}

func (f *fake) act(fn func() error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failActions {
		f.lastError = "simulated failure"
		return errors.New("simulated failure")
	}
	return fn()
}

func (f *fake) OpenPairing(d time.Duration) error {
	return f.act(func() error {
		if d == 0 {
			d = 2 * time.Minute
		}
		f.pairUntil = time.Now().Add(d)
		return nil
	})
}

func (f *fake) ClosePairing() error {
	return f.act(func() error { f.pairUntil = time.Time{}; return nil })
}

func (f *fake) Disconnect(a bt.Addr) error {
	return f.act(func() error {
		d, ok := f.devices[a.String()]
		if !ok || !d.connected {
			return errors.New("not connected")
		}
		d.connected, d.lastSeen = false, time.Now()
		if f.streaming == a.String() {
			f.streaming = ""
		}
		return nil
	})
}

func (f *fake) Forget(a bt.Addr) error {
	return f.act(func() error {
		s := a.String()
		if _, ok := f.devices[s]; !ok {
			return errors.New("unknown device")
		}
		delete(f.devices, s)
		for i, x := range f.order {
			if x == s {
				f.order = append(f.order[:i], f.order[i+1:]...)
				break
			}
		}
		if f.streaming == s {
			f.streaming = ""
		}
		return nil
	})
}

func (f *fake) SetName(n string) error {
	return f.act(func() error { f.name = n; return nil })
}

func main() {
	listen := flag.String("listen", "localhost:8099", "HTTP listen address")
	password := flag.String("http-password", "", "HTTP basic auth password (user admin; empty = no auth)")
	fail := flag.Bool("fail", false, "make every action fail, to test error display")
	flag.Parse()
	h := web.Handler(newFake(*fail), web.Config{Password: *password})
	log.Printf("webdemo: serving fake backend on %s", *listen)
	srv := &http.Server{Addr: *listen, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
