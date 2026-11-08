package main

import (
	"testing"
	"time"

	"btspeaker/internal/avdtp"
	"btspeaker/internal/bt"
	"btspeaker/internal/mgmt"
	"btspeaker/internal/player"
	"btspeaker/internal/store"
)

func TestBuildStatusMergesDevices(t *testing.T) {
	paired := bt.Addr{1, 2, 3, 4, 5, 6}
	stranger := bt.Addr{9, 9, 9, 9, 9, 9}
	cs := mgmt.Status{Present: true, Connected: []bt.Addr{stranger, paired}, PairingRemaining: 1500 * time.Millisecond}
	devs := []store.Device{{Addr: paired.String(), Name: "phone"}, {Addr: "AA:AA:AA:AA:AA:AA", Name: "old"}}
	s := buildStatus(cs, player.Status{}, avdtp.Stats{}, devs, nil, false)
	if len(s.Devices) != 3 {
		t.Fatalf("devices %+v", s.Devices)
	}
	if d := s.Devices[0]; d.Name != "phone" || !d.Connected {
		t.Fatalf("%+v", d)
	}
	if d := s.Devices[1]; d.Connected {
		t.Fatalf("%+v", d)
	}
	if d := s.Devices[2]; d.Addr != stranger.String() || !d.Connected || d.Name != "" {
		t.Fatalf("%+v", d)
	}
	if !s.Pairing.Open || s.Pairing.RemainingSeconds != 2 {
		t.Fatalf("pairing %+v", s.Pairing)
	}
	if s2 := buildStatus(mgmt.Status{}, player.Status{}, avdtp.Stats{}, nil, nil, false); s2.Pairing.Open || s2.Pairing.RemainingSeconds != 0 {
		t.Fatalf("pairing %+v", s2.Pairing)
	}
}

func TestBuildStatusStreams(t *testing.T) {
	idle := avdtp.StreamInfo{Device: bt.Addr{1}, State: "idle"}
	open := avdtp.StreamInfo{Device: bt.Addr{2}, State: "streaming", SampleRate: 44100}
	s := buildStatus(mgmt.Status{}, player.Status{}, avdtp.Stats{}, nil, []avdtp.StreamInfo{idle, open}, false)
	if s.Stream == nil || s.Stream.Device != open.Device.String() || s.Stream.Playing {
		t.Fatalf("%+v", s.Stream)
	}
	if s := buildStatus(mgmt.Status{}, player.Status{}, avdtp.Stats{}, nil, []avdtp.StreamInfo{idle}, false); s.Stream != nil {
		t.Fatalf("idle stream reported: %+v", s.Stream)
	}
	s = buildStatus(mgmt.Status{}, player.Status{Open: true}, avdtp.Stats{}, nil, []avdtp.StreamInfo{idle}, false)
	if s.Stream == nil || !s.Stream.Playing {
		t.Fatalf("%+v", s.Stream)
	}
}
