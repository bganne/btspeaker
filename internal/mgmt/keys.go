package mgmt

import (
	"log/slog"

	"btspeaker/internal/bt"
)

// maxLinkKeyType is the highest key type the kernel accepts in Load Link Keys.
const maxLinkKeyType = 8

// storedKeys returns the valid stored link keys, as sent to Load Link Keys.
func (c *Controller) storedKeys() []Key {
	var keys []Key
	for _, d := range c.cfg.Store.Devices() {
		if d.LinkKey == nil {
			continue
		}
		a, err := bt.ParseAddr(d.Addr)
		if err != nil {
			continue
		}
		v, err := d.LinkKey.Bytes()
		if err != nil {
			c.log.Warn("skipping bad stored key", "addr", d.Addr)
			continue
		}
		if d.LinkKey.Type > maxLinkKeyType {
			c.log.Warn("skipping stored key with invalid type", "addr", d.Addr, "type", d.LinkKey.Type)
			continue
		}
		keys = append(keys, Key{Addr: a, Type: d.LinkKey.Type, Value: v, PinLen: d.LinkKey.PinLen})
	}
	return keys
}

// loadKeysTolerant retries a failed batch Load Link Keys per key and ends with
// a load of the accepted ones; the rejected keys are logged. It returns the
// loaded keys, or an error if even that final load fails. do sends one Load
// Link Keys command. The batch is tried first unless the caller already
// did (batchFailed).
func loadKeysTolerant(do func(params []byte) error, keys []Key, batchFailed bool, log *slog.Logger) ([]Key, error) {
	if !batchFailed && do(EncodeLoadLinkKeys(keys)) == nil {
		return keys, nil
	}
	var good []Key
	for _, k := range keys {
		if err := do(EncodeLoadLinkKeys([]Key{k})); err != nil {
			log.Warn("kernel rejected stored link key", "addr", k.Addr.String(), "err", err)
			continue
		}
		good = append(good, k)
	}
	if err := do(EncodeLoadLinkKeys(good)); err != nil {
		return nil, err
	}
	return good, nil
}

// reloadKeysLocked restores the stored link keys in the kernel after an
// Unpair or a flushed non-persistent key removed one (Load Link Keys replaces
// the kernel's whole list, so all stored keys are sent), admitting every key
// that loaded. Session keys of connected no-bonding peers are lost too; such a
// peer must re-pair (rare; documented in the README). Needs winMu: a concurrent Forget must not interleave between
// the Store.Has check and the load. Does nothing if addr is no longer stored.
func (c *Controller) reloadKeysLocked(addr bt.Addr) {
	if !c.cfg.Store.Has(addr) {
		return
	}
	good, err := loadKeysTolerant(func(p []byte) error { _, e := c.cmd(OpLoadLinkKeys, p); return e }, c.storedKeys(), false, c.log)
	if err != nil {
		c.log.Warn("reload link keys failed", "addr", addr.String(), "err", err)
		return
	}
	c.mu.Lock()
	for _, k := range good {
		c.loaded[k.Addr] = true
	}
	c.mu.Unlock()
}
