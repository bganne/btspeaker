package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// taskCaps returns the capability lines and NoNewPrivs of every thread.
func taskCaps(t *testing.T) map[string][]string {
	t.Helper()
	dirs, err := filepath.Glob("/proc/self/task/*/status")
	if err != nil || len(dirs) == 0 {
		t.Fatalf("no task status: %v", err)
	}
	out := map[string][]string{}
	for _, p := range dirs {
		b, err := os.ReadFile(p)
		if err != nil {
			continue // thread exited
		}
		for _, l := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(l, "Cap") || strings.HasPrefix(l, "NoNewPrivs:") {
				out[p] = append(out[p], l)
			}
		}
	}
	return out
}

// TestDropCaps runs itself in a user namespace (full capabilities there) and
// checks that dropCaps empties every set of every thread.
func TestDropCaps(t *testing.T) {
	if os.Getenv("BTSPEAKER_DROPCAPS_CHILD") != "1" {
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("unshare", "-r", exe, "-test.run=^TestDropCaps$", "-test.v")
		cmd.Env = append(os.Environ(), "BTSPEAKER_DROPCAPS_CHILD=1")
		out, err := cmd.CombinedOutput()
		if strings.Contains(string(out), "SKIP-CGO") {
			t.Skip("cgo build: run with CGO_ENABLED=0")
		}
		if err != nil && !strings.Contains(string(out), "=== RUN") {
			t.Skipf("unshare -r unavailable: %v: %s", err, out)
		}
		if err != nil {
			t.Fatalf("child failed: %s", out)
		}
		return
	}
	// Extra threads, parked, to check the drop reaches all of them.
	stop := make(chan struct{})
	defer close(stop)
	for range 4 {
		go func() {
			runtime.LockOSThread()
			<-stop
		}()
	}
	before := taskCaps(t)
	for p, ls := range before {
		if slices.Contains(ls, "CapEff:\t0000000000000000") {
			t.Fatalf("%s: no capabilities to drop: %v", p, ls)
		}
	}
	if err := dropCaps(); err != nil {
		if errors.Is(err, errCgoBuild) {
			t.Log("SKIP-CGO")
			return
		}
		t.Fatal(err)
	}
	after := taskCaps(t)
	if len(after) < 2 {
		t.Fatalf("only %d threads", len(after))
	}
	for p, ls := range after {
		for _, l := range ls {
			k, v, _ := strings.Cut(l, ":")
			v = strings.TrimSpace(v)
			want := "0000000000000000"
			if k == "NoNewPrivs" {
				want = "1"
			}
			if v != want {
				t.Errorf("%s: %s = %s, want %s (before: %v)", p, k, v, want, before[p])
			}
		}
	}
}
