package main

import (
	"time"

	"btspeaker/internal/avdtp"
	"btspeaker/internal/bt"
	"btspeaker/internal/mgmt"
	"btspeaker/internal/player"
	"btspeaker/internal/store"
	"btspeaker/internal/web"
)

// backend implements web.Backend on top of the mgmt controller, store, AVDTP and player.
type backend struct {
	ctrl  *mgmt.Controller
	st    *store.Store
	avdtp *avdtp.Server
	pl    *player.Player
	// bitPerfect (-volume off): the AVRCP volume is reported but not applied.
	bitPerfect bool
}

func (b *backend) OpenPairing(d time.Duration) error { return b.ctrl.OpenPairing(d) }
func (b *backend) ClosePairing() error               { return b.ctrl.ClosePairing() }
func (b *backend) Disconnect(a bt.Addr) error        { return b.ctrl.Disconnect(a) }
func (b *backend) Forget(a bt.Addr) error            { return b.ctrl.Forget(a) }
func (b *backend) SetName(n string) error            { return b.ctrl.SetName(n) }

func (b *backend) Status() web.Status {
	return buildStatus(b.ctrl.Status(), b.pl.Status(), b.avdtp.Stats(), b.st.Devices(), b.avdtp.Streams(), b.bitPerfect)
}

func buildStatus(cs mgmt.Status, ps player.Status, as avdtp.Stats, devs []store.Device, streams []avdtp.StreamInfo, bitPerfect bool) web.Status {
	s := web.Status{
		Adapter: web.Adapter{Name: cs.Name, Addr: cs.Addr.String(), Present: cs.Present, Powered: cs.Powered},
		Pairing: web.Pairing{Open: cs.PairingRemaining > 0, Always: cs.AlwaysPairable,
			RemainingSeconds: int((cs.PairingRemaining + time.Second - 1) / time.Second)},
		Volume:        int(ps.Volume),
		VolumeIgnored: bitPerfect,
		LastError:     cs.LastError,
	}
	s.Diag = web.Diagnostics{Xruns: ps.Xruns, QueueDropFrames: ps.QueueDrop, LostPackets: as.LostPackets, BadFrames: as.BadFrames, ConcealedMs: as.ConcealedMs, DroppedDup: as.DroppedDup, Resyncs: as.Resyncs}
	if s.LastError == "" {
		s.LastError = ps.LastError
	}
	connected := map[string]bool{}
	for _, a := range cs.Connected {
		connected[a.String()] = true
	}
	seen := map[string]bool{}
	for _, d := range devs {
		seen[d.Addr] = true
		s.Devices = append(s.Devices, web.Device{Addr: d.Addr, Name: d.Name, Connected: connected[d.Addr], LastSeen: d.LastSeen})
	}
	for _, ca := range cs.Connected { // connected but unpaired (e.g. mid-pairing), in stable order
		if a := ca.String(); !seen[a] {
			seen[a] = true
			s.Devices = append(s.Devices, web.Device{Addr: a, Connected: true})
		}
	}
	for _, si := range streams {
		if si.State == "idle" && !ps.Open {
			continue
		}
		s.Stream = &web.Stream{Device: si.Device.String(), State: si.State, SampleRate: si.SampleRate,
			Channels: si.Channels, Mode: si.Mode, Bitpool: si.Bitpool, Playing: ps.Open}
		break
	}
	return s
}
