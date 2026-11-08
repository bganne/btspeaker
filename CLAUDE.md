# btspeaker

Single static Go binary that makes a USB Bluetooth dongle behave as an A2DP speaker and plays
to an ALSA hw device. Target: cubox v1 (Marvell Dove, single-core ARMv7 armhf, Debian 13 trixie, S/PDIF to a hifi,
also running mpd).

## Hard constraints
- No D-Bus, no bluetoothd, no alsa-lib, no cgo. Deps: stdlib + `golang.org/x/sys/unix` only.
- Uses the kernel BT stack directly: mgmt socket (HCI_CHANNEL_CONTROL) for adapter/pairing/link
  keys, L2CAP SOCK_SEQPACKET sockets for SDP (PSM 1), AVDTP (25), AVCTP (23).
  bluetoothd must NOT run at the same time (it fights over PSM 1, pairing, keys).
- ALSA via raw PCM ioctls on /dev/snd: only `hw:CARD,DEV`; no dmix/plug, so the device is
  exclusive. mpd coexistence: hold the device only while non-silent audio flows; EBUSY → report
  "busy" and retry every 3 s.
- Struct layouts for ioctls differ amd64 vs arm (32-bit): never hardcode sizes, use unsafe.Sizeof.

## Layout
- `main.go` flags/env (`BTSPEAKER_*`), wiring, listeners
- `internal/bt` shared types (`Addr`, `Conn`) — imported by protocol pkgs, keep tiny
- `internal/hci` sockets (mgmt, L2CAP listen/accept → bt.Conn) · `internal/mgmt` mgmt protocol client
- `internal/store` persisted state (link keys, names) · `internal/web` JSON API + server-rendered no-JS UI (forms → /ui/*)
- `internal/sdp`, `internal/avdtp`, `internal/avrcp` protocol servers (pure, test on byte fixtures)
- `internal/sbc` SBC decoder (testdata/ = ffmpeg-made vectors) · `internal/alsa` PCM ioctls
- `internal/player` PCM → ALSA, volume, idle release
- `deploy/` systemd unit · `cmd/webdemo` dev-only web UI server on a fake in-memory backend

## Git
- Commit directly on `master` (no feature branches), except in worktrees.
- One commit per area (package), subject `pkg: what changed`, body explaining what/why. Never
  mention reviews (no "review fixes", no review ids); review reports/decisions (`docs/history/`, `docs/review-decisions.md`) stay uncommitted.

## Commands
- Test: `go vet ./... && go test ./...`
- Build host: `go build .` · cubox: `make arm` (CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7)
- `make bench-arm`: armv7 test binaries in `bench-arm/` (sbc.test, player.test); run on cubox with `-test.bench=.`
- Vet for arm too: `GOARCH=arm go vet ./...`
- Fuzz: `go test -run=^$ -fuzz=FuzzDecode -fuzztime=60s ./internal/sbc` (also `FuzzSignal`, `FuzzRTP`, `FuzzStream` in `./internal/avdtp`; `FuzzSDP` in `./internal/sdp`; `FuzzAVRCP` in `./internal/avrcp`)
- Laptop E2E: CSR dongle is hci1; needs `sudo systemctl stop bluetooth` first;
  `sudo ./btspeaker -adapter hci1 -alsa-device hw:0,0`; debug with `sudo btmon`.
- Web UI without BT: `go run ./cmd/webdemo` → http://localhost:8099 (`-fail` makes actions error, `-http-password`).
- ALSA without BT: `sudo modprobe snd-aloop`; `./btspeaker -alsa-device hw:Loopback,0 -test-tone`
  + `arecord -D hw:Loopback,1 -f S16_LE -r 44100 -c 2 x.wav`.
