package mgmt

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"btspeaker/internal/store"
)

// Tests never touch the host's rfkill switches.
func init() {
	rfkillDir = "/nonexistent/rfkill"
	rfkillDev = "/nonexistent/rfkill-dev"
}

func mkRfkill(t *testing.T, dir, n, name, soft, hard string) {
	t.Helper()
	p := filepath.Join(dir, n)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	for f, v := range map[string]string{"name": name, "soft": soft, "hard": hard} {
		if err := os.WriteFile(filepath.Join(p, f), []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRfkillFind(t *testing.T) {
	dir := t.TempDir()
	mkRfkill(t, dir, "rfkill1", "phy0", "1", "0")
	mkRfkill(t, dir, "rfkill20", "hci0", "1", "0")
	mkRfkill(t, dir, "rfkill21", "hci1", "0", "1")
	if sw, ok := rfkillFind(dir, 0); !ok || sw.idx != 20 || !sw.soft || sw.hard {
		t.Fatalf("hci0: %+v %v", sw, ok)
	}
	if sw, ok := rfkillFind(dir, 1); !ok || sw.idx != 21 || sw.soft || !sw.hard {
		t.Fatalf("hci1: %+v %v", sw, ok)
	}
	if _, ok := rfkillFind(dir, 2); ok {
		t.Fatal("hci2 found")
	}
	if _, ok := rfkillFind(filepath.Join(dir, "missing"), 0); ok {
		t.Fatal("found in missing dir")
	}
}

func rfkillEvent(idx uint32, soft byte) []byte {
	return []byte{byte(idx), byte(idx >> 8), byte(idx >> 16), byte(idx >> 24), 0, rfkillOpChange, soft, 0}
}

// A soft-blocked adapter is unblocked at init and re-blocked at Shutdown,
// unless its switch index now belongs to something else.
func TestRfkillUnblockAndRestore(t *testing.T) {
	for _, replugged := range []bool{false, true} {
		dir := t.TempDir()
		dev := filepath.Join(t.TempDir(), "rfkill")
		if err := os.WriteFile(dev, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		rfkillDir, rfkillDev = dir, dev
		mkRfkill(t, dir, "rfkill7", "hci0", "1", "0")

		fake, conn := newFake(t)
		st, err := store.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		c := New(Config{Name: "Test", Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
			Open: func() (io.ReadWriteCloser, error) { return conn, nil }})
		if err := c.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "ready", func() bool { return c.Status().Present && fake.count(OpGetConnections) == 1 })
		if got, _ := os.ReadFile(dev); !bytes.Equal(got, rfkillEvent(7, 0)) {
			t.Fatalf("after init: %x", got)
		}
		os.WriteFile(dev, nil, 0o600)
		want := rfkillEvent(7, 1)
		if replugged {
			mkRfkill(t, dir, "rfkill7", "hci3", "0", "0")
			want = nil
		}
		c.Shutdown()
		if got, _ := os.ReadFile(dev); !bytes.Equal(got, want) {
			t.Fatalf("replugged=%v after shutdown: %x, want %x", replugged, got, want)
		}
	}
	rfkillDir, rfkillDev = "/nonexistent/rfkill", "/nonexistent/rfkill-dev"
}
