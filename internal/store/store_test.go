package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"btspeaker/internal/bt"
)

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := bt.Addr{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0x01}
	if err := s.SetDeviceName(a, "ignored"); err != nil || s.Has(a) {
		t.Fatal("unknown device must be ignored")
	}
	key := NewLinkKey(8, [16]byte{1, 2, 3, 15: 0xff}, 0)
	now := time.Unix(1700000000, 0).UTC()
	if err := s.PutKey(a, key, now); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDeviceName(a, "Phone"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetName("Hifi"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json.tmp")); err == nil {
		t.Fatal("tmp file left behind")
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Name() != "Hifi" {
		t.Fatalf("name %q", s2.Name())
	}
	ds := s2.Devices()
	if len(ds) != 1 || ds[0].Addr != a.String() || ds[0].Name != "Phone" || ds[0].LinkKey == nil {
		t.Fatalf("devices %+v", ds)
	}
	raw, err := ds[0].LinkKey.Bytes()
	if err != nil || raw[0] != 1 || raw[15] != 0xff || ds[0].LinkKey.Type != 8 {
		t.Fatalf("key %+v %v", ds[0].LinkKey, err)
	}
	if err := s2.Remove(a); err != nil || len(s2.Devices()) != 0 {
		t.Fatal("remove failed")
	}
}

func TestCorrupt(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "state.json"), []byte("{nope"), 0o600)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	bak := filepath.Join(dir, "state.json.corrupt")
	if s.Corrupt() != bak {
		t.Fatalf("Corrupt() = %q", s.Corrupt())
	}
	if b, err := os.ReadFile(bak); err != nil || string(b) != "{nope" {
		t.Fatalf("backup: %q %v", b, err)
	}
	if len(s.Devices()) != 0 {
		t.Fatal("want empty store")
	}
	if err := s.SetName("x"); err != nil {
		t.Fatal(err)
	}
	if s2, err := Open(dir); err != nil || s2.Corrupt() != "" || s2.Name() != "x" {
		t.Fatal("reopen", err)
	}
}

func TestTouchSkipsSave(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	a := bt.Addr{1, 2, 3, 4, 5, 6}
	t0 := time.Unix(1700000000, 0).UTC()
	if err := s.PutKey(a, NewLinkKey(8, [16]byte{1}, 0), t0); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "state.json")
	before, _ := os.ReadFile(path)
	s.Touch(a, t0.Add(10*time.Minute))
	s.SetDeviceName(a, "")
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("file rewritten for a recent last_seen change")
	}
	if got := s.Devices()[0].LastSeen; !got.Equal(t0.Add(10 * time.Minute)) {
		t.Fatalf("memory not updated: %v", got)
	}
	s.Touch(a, t0.Add(2*time.Hour))
	if after, _ := os.ReadFile(path); string(after) == string(before) {
		t.Fatal("file not rewritten after 1h")
	}
	s2, _ := Open(dir)
	if got := s2.Devices()[0].LastSeen; !got.Equal(t0.Add(2 * time.Hour)) {
		t.Fatalf("persisted %v", got)
	}
}

func TestPutKeyNamed(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := bt.Addr{1, 2, 3, 4, 5, 6}
	if err := s.PutKeyNamed(a, NewLinkKey(8, [16]byte{1}, 0), "Phone", time.Now()); err != nil {
		t.Fatal(err)
	}
	d := s.Devices()
	if len(d) != 1 || d[0].Name != "Phone" || d[0].LinkKey == nil {
		t.Errorf("devices %+v", d)
	}
}

func TestTouchBackwardClockSaves(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	a := bt.Addr{1, 2, 3, 4, 5, 6}
	t0 := time.Unix(1700000000, 0).UTC()
	if err := s.PutKey(a, NewLinkKey(8, [16]byte{1}, 0), t0); err != nil {
		t.Fatal(err)
	}
	back := t0.Add(-48 * time.Hour)
	if err := s.Touch(a, back); err != nil {
		t.Fatal(err)
	}
	s2, _ := Open(dir)
	if got := s2.Devices()[0].LastSeen; !got.Equal(back) {
		t.Fatalf("persisted %v, want %v", got, back)
	}
}

// breakSave makes saveLocked fail even as root: the temp file path is a directory.
func breakSave(t *testing.T, dir string) {
	t.Helper()
	tmp := filepath.Join(dir, "state.json.tmp")
	if err := os.Mkdir(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	// Non-empty: writeFile removes a stale tmp first, which must not clear it.
	if err := os.WriteFile(filepath.Join(tmp, "x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestStaleTmpModeAndSymlink(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(dir, "state.json.tmp")
	if err := os.WriteFile(tmp, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := bt.Addr{1, 2, 3, 4, 5, 6}
	if err := s.PutKeyNamed(a, NewLinkKey(8, [16]byte{1}, 0), "P", time.Now()); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "state.json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("state.json mode %v, err %v; want 0600", fi.Mode(), err)
	}
	// A planted symlink is replaced, not followed.
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.Symlink(victim, tmp); err != nil {
		t.Fatal(err)
	}
	if err := s.Touch(a, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(victim); err == nil {
		t.Fatal("symlink target was written")
	}
}

func TestFailedSaveRollsBack(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := bt.Addr{1, 2, 3, 4, 5, 6}
	b := bt.Addr{6, 5, 4, 3, 2, 1}
	if err := s.PutKeyNamed(a, NewLinkKey(8, [16]byte{1}, 0), "Phone", time.Now()); err != nil {
		t.Fatal(err)
	}
	breakSave(t, dir)
	if s.Remove(a) == nil {
		t.Fatal("Remove: want error")
	}
	if !s.Has(a) {
		t.Error("Remove not rolled back")
	}
	if s.SetDeviceName(a, "Other") == nil {
		t.Fatal("SetDeviceName: want error")
	}
	if d := s.Devices(); len(d) != 1 || d[0].Name != "Phone" {
		t.Errorf("SetDeviceName not rolled back: %+v", d)
	}
	if s.PutKeyNamed(b, NewLinkKey(8, [16]byte{2}, 0), "New", time.Now()) == nil {
		t.Fatal("PutKeyNamed: want error")
	}
	if s.Has(b) {
		t.Error("PutKeyNamed (new) not rolled back")
	}
	if s.PutKeyNamed(a, NewLinkKey(4, [16]byte{3}, 0), "Changed", time.Now()) == nil {
		t.Fatal("PutKeyNamed: want error")
	}
	if d := s.Devices(); len(d) != 1 || d[0].Name != "Phone" || d[0].LinkKey.Type != 8 {
		t.Errorf("PutKeyNamed (existing) not rolled back: %+v", d)
	}
}

func TestConcurrentSaves(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a := bt.Addr{0, 0, 0, 0, 0, byte(i + 1)}
			if err := s.PutKey(a, NewLinkKey(8, [16]byte{byte(i)}, 0), time.Unix(1700000000, 0)); err != nil {
				t.Error(err)
			}
			s.Has(a)
			s.Devices()
		}()
	}
	wg.Wait()
	if err := s.SetName(fmt.Sprint("final")); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(s2.Devices()); n != 20 || s2.Name() != "final" {
		t.Fatalf("disk has %d devices, name %q; want 20, final", n, s2.Name())
	}
}

func TestOpenNormalisesAddresses(t *testing.T) {
	dir := t.TempDir()
	js := `{"devices":[{"addr":"aa:bb:cc:dd:ee:ff","name":"A"},{"addr":"junk"},{"addr":"AA:BB:CC:DD:EE:FF"},{"addr":" 1:00:00:00:00:00"}]}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(js), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	d := s.Devices()
	if len(d) != 1 || d[0].Addr != "AA:BB:CC:DD:EE:FF" || d[0].Name != "A" {
		t.Fatalf("devices = %+v", d)
	}
	if !s.Has(bt.Addr{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF}) {
		t.Error("Has: false after normalise")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "state.json.corrupt")); string(b) != js {
		t.Errorf("original not backed up: %q", b)
	}
	if s.Corrupt() != "" {
		t.Errorf("Corrupt() = %q for a parseable file", s.Corrupt())
	}
	// Reopening without a save in between must not pile up identical backups.
	if _, err := Open(dir); err != nil {
		t.Fatal(err)
	}
	if ms, _ := filepath.Glob(filepath.Join(dir, "state.json.corrupt*")); len(ms) != 1 {
		t.Errorf("backups = %v", ms)
	}
}

func TestBackupDedupGlobMetachars(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "st[a*?")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	js := `{"devices":[{"addr":"junk"}]}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(js), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := Open(dir); err != nil {
			t.Fatal(err)
		}
	}
	es, _ := os.ReadDir(dir)
	n := 0
	for _, e := range es {
		if strings.HasPrefix(e.Name(), "state.json.corrupt") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("backups = %d, want 1", n)
	}
}

func TestCorruptBackupNotOverwritten(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	os.WriteFile(p+".corrupt", []byte("first"), 0o600)
	os.WriteFile(p, []byte("{bad"), 0o600)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Corrupt() == p+".corrupt" || s.Corrupt() == "" {
		t.Errorf("Corrupt() = %q", s.Corrupt())
	}
	if b, _ := os.ReadFile(p + ".corrupt"); string(b) != "first" {
		t.Errorf("old backup overwritten: %q", b)
	}
	if b, _ := os.ReadFile(s.Corrupt()); string(b) != "{bad" {
		t.Errorf("new backup = %q", b)
	}
}

func TestEvictOldest(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= MaxDevices+2; i++ {
		a := bt.Addr{0, 0, 0, 0, 0, byte(i)}
		if err := s.PutKey(a, NewLinkKey(4, [16]byte{byte(i)}, 0), time.Unix(int64(1000+i), 0)); err != nil {
			t.Fatal(err)
		}
	}
	keep := bt.Addr{0, 0, 0, 0, 0, 1} // oldest, but protected
	ev := s.EvictOldest(keep, MaxDevices)
	if len(ev) != 2 || ev[0] != (bt.Addr{0, 0, 0, 0, 0, 2}) || ev[1] != (bt.Addr{0, 0, 0, 0, 0, 3}) {
		t.Fatalf("evicted %v", ev)
	}
	if !s.Has(keep) || s.Has(ev[0]) || len(s.Devices()) != MaxDevices {
		t.Errorf("bad state after evict: %d devices", len(s.Devices()))
	}
	s2, _ := Open(dir)
	if len(s2.Devices()) != MaxDevices {
		t.Errorf("disk has %d devices", len(s2.Devices()))
	}
	if ev := s.EvictOldest(keep, MaxDevices); ev != nil {
		t.Errorf("second evict: %v", ev)
	}
}

func TestOpenCapsDevices(t *testing.T) {
	dir := t.TempDir()
	var st state
	n := MaxDevices + 5
	for i := 0; i < n; i++ {
		st.Devices = append(st.Devices, Device{Addr: bt.Addr{0, 0, 0, 0, 0, byte(i)}.String(), LastSeen: time.Unix(int64(1000+i), 0)})
	}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "state.json"), b, 0o600)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Devices()) != MaxDevices {
		t.Fatalf("%d devices loaded", len(s.Devices()))
	}
	for i := 0; i < n; i++ {
		a := bt.Addr{0, 0, 0, 0, 0, byte(i)}
		if want := i >= n-MaxDevices; s.Has(a) != want {
			t.Errorf("device %d: has=%v want %v", i, s.Has(a), want)
		}
	}
}

func TestFailedRemoveRestoredDespiteTouch(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	s, _ := Open(dir)
	a := bt.Addr{1, 2, 3, 4, 5, 6}
	t0 := time.Unix(1700000000, 0).UTC()
	if err := s.PutKey(a, NewLinkKey(8, [16]byte{1}, 0), t0); err != nil {
		t.Fatal(err)
	}
	b := bt.Addr{6, 5, 4, 3, 2, 1}
	if err := s.PutKey(b, NewLinkKey(8, [16]byte{2}, 0), t0); err != nil {
		t.Fatal(err)
	}
	s.fileMu.Lock() // stall Remove's file write
	errc := make(chan error, 1)
	go func() { errc <- s.Remove(a) }()
	for { // wait until Remove released s.mu for its I/O
		s.mu.Lock()
		gone := len(s.st.Devices) == 1
		s.mu.Unlock()
		if gone {
			break
		}
		time.Sleep(time.Millisecond)
	}
	s.Touch(b, t0.Add(time.Minute)) // non-persisting, but bumps gen
	breakSave(t, dir)
	s.fileMu.Unlock()
	if err := <-errc; err == nil {
		t.Fatal("Remove: want error")
	}
	if !s.Has(a) {
		t.Fatal("failed Remove not restored")
	}
}

func TestOpenPerms(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "st")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte(`{"devices":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]os.FileMode{dir: 0o755, path: 0o600} {
		if fi, _ := os.Stat(p); fi.Mode().Perm() != want {
			t.Errorf("%s: %v", p, fi.Mode().Perm())
		}
	}
}

func TestOpenNewDirIs0700(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "new")
	if _, err := Open(dir); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Errorf("new dir: %v", fi.Mode().Perm())
	}
}

func TestCheckWritable(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CheckWritable(); err != nil {
		t.Fatal(err)
	}
	if es, _ := os.ReadDir(dir); len(es) != 0 {
		t.Fatalf("probe left %v", es)
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	if err := s.CheckWritable(); err == nil {
		t.Fatal("read-only dir reported writable")
	}
}
