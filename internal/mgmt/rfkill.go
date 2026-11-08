package mgmt

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// A soft rfkill block (desktop Bluetooth toggle, airplane mode, restored at
// boot by systemd-rfkill) makes power on fail with a bare "Failed". init lifts
// it through /dev/rfkill (sysfs is read-only under ProtectKernelTunables) and
// Shutdown puts it back. A hard block (physical switch) cannot be lifted.

// Variables for tests.
var (
	rfkillDir = "/sys/class/rfkill"
	rfkillDev = "/dev/rfkill"
)

const (
	rfkillOpChange  = 2 // RFKILL_OP_CHANGE
	rfkillEventSize = 8 // struct rfkill_event v1: u32 idx; u8 type, op, soft, hard
)

type rfkillSwitch struct {
	idx        uint32
	name       string // "hci<N>"
	soft, hard bool
}

// rfkillFind returns the rfkill switch of hci<idx>.
func rfkillFind(dir string, idx uint16) (rfkillSwitch, bool) {
	want := fmt.Sprintf("hci%d", idx)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return rfkillSwitch{}, false
	}
	for _, e := range ents {
		n, err := strconv.ParseUint(strings.TrimPrefix(e.Name(), "rfkill"), 10, 32)
		if err != nil || !strings.HasPrefix(e.Name(), "rfkill") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if readTrim(filepath.Join(p, "name")) != want {
			continue
		}
		return rfkillSwitch{idx: uint32(n), name: want,
			soft: readTrim(filepath.Join(p, "soft")) == "1",
			hard: readTrim(filepath.Join(p, "hard")) == "1"}, true
	}
	return rfkillSwitch{}, false
}

func readTrim(p string) string {
	b, _ := os.ReadFile(p)
	return strings.TrimSpace(string(b))
}

// rfkillSetSoft sets the soft block of switch idx through /dev/rfkill.
func rfkillSetSoft(idx uint32, block bool) error {
	f, err := os.OpenFile(rfkillDev, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	ev := make([]byte, rfkillEventSize)
	binary.NativeEndian.PutUint32(ev, idx)
	ev[5] = rfkillOpChange
	if block {
		ev[6] = 1
	}
	_, err = f.Write(ev)
	return err
}

// unblockRfkill lifts a soft block on hci<idx> and remembers it for Shutdown.
func (c *Controller) unblockRfkill(idx uint16) {
	sw, ok := rfkillFind(rfkillDir, idx)
	if !ok || !sw.soft {
		if ok && sw.hard {
			c.log.Error("adapter is hard rfkill-blocked (hardware switch)", "rfkill", sw.name)
		}
		return
	}
	if err := rfkillSetSoft(sw.idx, false); err != nil {
		c.log.Error("adapter is rfkill-blocked and unblocking failed", "rfkill", sw.name, "err", err)
		return
	}
	c.log.Info("rfkill soft block lifted; restored at shutdown", "rfkill", sw.name)
	c.mu.Lock()
	c.rfkill, c.rfkillSet = sw, true
	c.mu.Unlock()
}

// rfkillRestore re-blocks sw if its index still belongs to the same adapter
// (a replugged dongle gets a new switch, restored by systemd-rfkill).
// It reports whether it did.
func rfkillRestore(sw rfkillSwitch) (bool, error) {
	p := filepath.Join(rfkillDir, fmt.Sprintf("rfkill%d", sw.idx))
	if readTrim(filepath.Join(p, "name")) != sw.name {
		return false, nil
	}
	return true, rfkillSetSoft(sw.idx, true)
}
