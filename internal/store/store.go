// Package store persists the adapter alias, paired devices and their link keys
// as JSON (<dir>/state.json), written atomically (tmp file + fsync + rename).
package store

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"btspeaker/internal/bt"
)

// LinkKey is a BR/EDR link key as handed out by the mgmt New Link Key event.
type LinkKey struct {
	Type   uint8  `json:"type"`
	Value  string `json:"value"` // 32 hex chars
	PinLen uint8  `json:"pin_len"`
}

// Bytes decodes Value; it returns an error unless it is exactly 16 bytes.
func (k LinkKey) Bytes() ([16]byte, error) {
	var out [16]byte
	b, err := hex.DecodeString(k.Value)
	if err != nil || len(b) != 16 {
		return out, fmt.Errorf("bad link key value %q", k.Value)
	}
	copy(out[:], b)
	return out, nil
}

// NewLinkKey builds a LinkKey from raw bytes.
func NewLinkKey(typ uint8, value [16]byte, pinLen uint8) LinkKey {
	return LinkKey{Type: typ, Value: hex.EncodeToString(value[:]), PinLen: pinLen}
}

// Device is one known remote device.
type Device struct {
	Addr     string    `json:"addr"` // "AA:BB:CC:DD:EE:FF"
	Name     string    `json:"name,omitempty"`
	LinkKey  *LinkKey  `json:"link_key,omitempty"`
	LastSeen time.Time `json:"last_seen,omitzero"`
}

type state struct {
	Name    string   `json:"name,omitempty"`
	Devices []Device `json:"devices"`
}

// Store is safe for concurrent use.
type Store struct {
	mu   sync.Mutex
	path string
	st   state
	// saved holds the last_seen value last written to disk per address, so
	// Touch can avoid an fsync'd rewrite on every reconnect.
	saved   map[string]time.Time
	corrupt string
	gen     uint64 // snapshot generation, guarded by mu

	fileMu  sync.Mutex // serialises file I/O; never taken while holding mu
	written uint64     // newest generation on disk, guarded by fileMu
}

// lastSeenSaveInterval bounds how often a last_seen-only change is persisted.
const lastSeenSaveInterval = time.Hour

// Open loads <dir>/state.json (creating dir if needed). A missing file is an
// empty store. A corrupt file is renamed to state.json.corrupt and the store
// starts empty (see Corrupt) rather than failing startup forever.
func Open(dir string) (*Store, error) {
	// Only a directory we create is restricted (the link keys are secrets); an
	// existing one is the operator's (e.g. a shared /var/lib parent) and is
	// left alone. The state file itself is always written 0600.
	_, statErr := os.Stat(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if statErr != nil {
		if err := os.Chmod(dir, 0o700); err != nil { // MkdirAll is subject to umask
			slog.Warn("store: cannot restrict directory permissions", "dir", dir, "err", err)
		}
	}
	s := &Store{path: filepath.Join(dir, "state.json")}
	b, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if uerr := json.Unmarshal(b, &s.st); uerr != nil {
			bak := s.backupPath()
			if err := os.Rename(s.path, bak); err != nil {
				return nil, fmt.Errorf("%s: %w (and cannot move it aside: %v)", s.path, uerr, err)
			}
			s.corrupt = bak
			s.st = state{}
		}
		if s.normalise() {
			// The next save rewrites the file without the dropped entries:
			// keep the original for inspection.
			// Until something saves, every boot gets here: back up only once.
			bak := s.sameBackup(b)
			if bak == "" {
				bak = s.backupPath()
				if err := os.WriteFile(bak, b, 0o600); err != nil {
					slog.Warn("store: cannot back up state before dropping entries", "err", err)
					bak = ""
				}
			}
			if bak != "" {
				slog.Warn("store: dropped invalid entries, original kept", "backup", bak)
			}
		}
	}
	if err := os.Chmod(s.path, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("store: cannot restrict state file permissions", "path", s.path, "err", err)
	}
	s.saved = s.lastSeenLocked()
	return s, nil
}

// sameBackup returns an existing backup whose content is b, or "".
func (s *Store) sameBackup(b []byte) string {
	ms, _ := filepath.Glob(s.path + ".corrupt*")
	for _, m := range ms {
		if old, err := os.ReadFile(m); err == nil && bytes.Equal(old, b) {
			return m
		}
	}
	return ""
}

// backupPath returns state.json.corrupt, or a timestamped variant if that
// already exists so an earlier backup is never overwritten.
func (s *Store) backupPath() string {
	bak := s.path + ".corrupt"
	if _, err := os.Lstat(bak); err == nil {
		bak += "." + time.Now().Format("20060102T150405")
	}
	return bak
}

// normalise canonicalises device addresses (find compares Addr.String()) and
// drops entries whose address does not parse or that duplicate an earlier one.
// It reports whether any entry was dropped.
func (s *Store) normalise() (dropped bool) {
	seen := map[string]bool{}
	out := s.st.Devices[:0]
	for _, d := range s.st.Devices {
		a, err := bt.ParseAddr(d.Addr)
		if err != nil {
			slog.Warn("store: dropping device with bad address", "addr", d.Addr)
			dropped = true
			continue
		}
		d.Addr = a.String()
		if seen[d.Addr] {
			slog.Warn("store: dropping duplicate device", "addr", d.Addr)
			dropped = true
			continue
		}
		seen[d.Addr] = true
		out = append(out, d)
	}
	if len(out) > MaxDevices {
		// Keep the most recently seen (a huge hand-edited list would not fit
		// in one Load Link Keys command).
		sort.SliceStable(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })
		slog.Warn("store: too many devices, dropping the oldest", "count", len(out), "max", MaxDevices)
		clear(out[MaxDevices:])
		out = out[:MaxDevices]
		dropped = true
	}
	s.st.Devices = out
	return dropped
}

// MaxDevices caps the number of stored devices (see EvictOldest).
const MaxDevices = 32

// EvictOldest removes the devices with the oldest last_seen (never keep) until
// at most limit remain, and returns their addresses. It reports nothing on a
// save failure beyond the in-memory rollback; the caller unpairs the result.
func (s *Store) EvictOldest(keep bt.Addr, limit int) []bt.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.st.Devices) <= limit {
		return nil
	}
	undo := s.snapshotLocked()
	ks := keep.String()
	var out []bt.Addr
	for len(s.st.Devices) > limit {
		oi := -1
		for i, d := range s.st.Devices {
			if d.Addr != ks && (oi < 0 || d.LastSeen.Before(s.st.Devices[oi].LastSeen)) {
				oi = i
			}
		}
		if oi < 0 {
			break
		}
		if a, err := bt.ParseAddr(s.st.Devices[oi].Addr); err == nil {
			out = append(out, a)
		}
		s.st.Devices = append(s.st.Devices[:oi:oi], s.st.Devices[oi+1:]...)
	}
	if err := s.saveOrRestoreLocked(undo); err != nil {
		slog.Error("store: evicting old devices", "err", err)
		return nil
	}
	return out
}

// Corrupt returns the path the unreadable state file was moved to by Open, or
// "" if the state loaded fine. Callers should log a warning when non-empty.
func (s *Store) Corrupt() string { return s.corrupt }

// Name returns the persisted adapter alias ("" if never set).
func (s *Store) Name() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.Name
}

func (s *Store) SetName(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.st.Name
	s.st.Name = name
	before := s.gen
	g, err := s.saveGenLocked()
	if err != nil {
		if s.gen == g && g == before+1 {
			s.st.Name = old
		}
		return err
	}
	return nil
}

// Devices returns a copy of all known devices sorted by address.
func (s *Store) Devices() []Device {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Device, len(s.st.Devices))
	for i, d := range s.st.Devices {
		if d.LinkKey != nil {
			k := *d.LinkKey
			d.LinkKey = &k
		}
		out[i] = d
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Addr < out[j].Addr })
	return out
}

// find returns the index of addr, appending an empty entry if create.
func (s *Store) find(addr bt.Addr, create bool) int {
	a := addr.String()
	for i := range s.st.Devices {
		if s.st.Devices[i].Addr == a {
			return i
		}
	}
	if !create {
		return -1
	}
	s.st.Devices = append(s.st.Devices, Device{Addr: a})
	return len(s.st.Devices) - 1
}

// Has reports whether addr is known (has an entry).
func (s *Store) Has(addr bt.Addr) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.find(addr, false) >= 0
}

// PutKey stores (replaces) the link key for addr, keeping its name.
func (s *Store) PutKey(addr bt.Addr, k LinkKey, now time.Time) error {
	return s.PutKeyNamed(addr, k, "", now)
}

// PutKeyNamed is PutKey plus the device name (if non-empty), in a single write.
func (s *Store) PutKeyNamed(addr bt.Addr, k LinkKey, name string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	undo := s.snapshotLocked()
	i := s.find(addr, true)
	s.st.Devices[i].LinkKey = &k
	if name != "" {
		s.st.Devices[i].Name = name
	}
	s.st.Devices[i].LastSeen = now
	return s.saveOrRestoreLocked(undo)
}

// SetDeviceName records the remote device's name; unknown devices are ignored.
func (s *Store) SetDeviceName(addr bt.Addr, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.find(addr, false)
	if i < 0 || s.st.Devices[i].Name == name {
		return nil
	}
	undo := s.snapshotLocked()
	s.st.Devices[i].Name = name
	return s.saveOrRestoreLocked(undo)
}

// Touch updates last_seen; unknown devices are ignored. To limit flash wear
// the file is only rewritten if the last persisted last_seen is over an hour old.
func (s *Store) Touch(addr bt.Addr, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.find(addr, false)
	if i < 0 {
		return nil
	}
	s.st.Devices[i].LastSeen = now
	// d < 0: the clock went backwards (no RTC), so save rather than block forever.
	if last, ok := s.saved[s.st.Devices[i].Addr]; ok {
		if d := now.Sub(last); d >= 0 && d < lastSeenSaveInterval {
			s.gen++ // memory changed: a concurrent failed save must not roll it back
			return nil
		}
	}
	return s.saveLocked()
}

// Remove forgets addr (and its key). Unknown addresses are not an error.
func (s *Store) Remove(addr bt.Addr) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.find(addr, false)
	if i < 0 {
		return nil
	}
	removed := s.st.Devices[i]
	s.st.Devices = append(s.st.Devices[:i:i], s.st.Devices[i+1:]...)
	_, err := s.saveGenLocked()
	if err != nil && s.find(addr, false) < 0 {
		// Undo just this removal (not a gen-based snapshot restore, which a
		// concurrent Touch would defeat): memory must not get ahead of disk.
		s.st.Devices = append(s.st.Devices, removed)
	}
	return err
}

// snapshotLocked copies the device list (entries are replaced, never mutated
// through their LinkKey pointer) so a failed save can be rolled back.
func (s *Store) snapshotLocked() []Device {
	return append([]Device(nil), s.st.Devices...)
}

// saveOrRestoreLocked saves, restoring the snapshot if that fails so memory
// never gets ahead of disk (a forgotten device must not return after a restart).
func (s *Store) saveOrRestoreLocked(undo []Device) error {
	before := s.gen
	g, err := s.saveGenLocked()
	if err != nil {
		// Only roll back if nothing else changed while s.mu was released.
		if s.gen == g && g == before+1 {
			s.st.Devices = undo
		}
		return err
	}
	return nil
}

// saveLocked persists the current state. Called with s.mu held; it marshals a
// snapshot, releases s.mu for the file I/O (so Has/Devices never wait on an
// fsync) and re-acquires it before returning.
func (s *Store) saveLocked() error {
	_, err := s.saveGenLocked()
	return err
}

// saveGenLocked is saveLocked also returning the generation of the snapshot
// it wrote, so callers can tell whether memory changed while s.mu was released.
func (s *Store) saveGenLocked() (uint64, error) {
	if s.st.Devices == nil {
		s.st.Devices = []Device{}
	}
	b, err := json.Marshal(&s.st)
	if err != nil {
		return s.gen, err
	}
	s.gen++
	g := s.gen
	saved := s.lastSeenLocked()
	s.mu.Unlock()
	err = s.writeFile(b, g, saved)
	s.mu.Lock()
	return g, err
}

func (s *Store) lastSeenLocked() map[string]time.Time {
	m := make(map[string]time.Time, len(s.st.Devices))
	for _, d := range s.st.Devices {
		m[d.Addr] = d.LastSeen
	}
	return m
}

// writeFile writes snapshot b of generation g unless a newer one is already
// on disk (concurrent saves coalesce: only the newest snapshot matters).
func (s *Store) writeFile(b []byte, g uint64, saved map[string]time.Time) error {
	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	if g <= s.written {
		return nil
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		os.Remove(tmp)
		return err
	}
	if d, err := os.Open(filepath.Dir(s.path)); err == nil {
		d.Sync()
		d.Close()
	}
	s.written = g
	s.mu.Lock()
	s.saved = saved
	s.mu.Unlock()
	return nil
}
