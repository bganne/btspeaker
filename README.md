# btspeaker

A single static Go binary that makes a USB Bluetooth adapter behave as an A2DP speaker and plays the
audio to an ALSA `hw:` device (built for a cubox v1 feeding a hifi over S/PDIF). No D-Bus, no
bluetoothd, no alsa-lib, no cgo: it talks to the kernel Bluetooth stack (mgmt socket + L2CAP sockets)
and the kernel ALSA PCM ioctls directly. SBC is decoded in pure Go. Phones and PCs can stream to it, send
AVRCP absolute volume, and it has a small web UI for pairing.

## Install on the cubox

```sh
make arm                                   # builds btspeaker-armv7 (use GOARM=6 if you get SIGILL)
scp btspeaker-armv7 cubox:/usr/local/bin/btspeaker
scp deploy/btspeaker.service cubox:/etc/systemd/system/

# on the cubox: bluetoothd must not run (it fights over PSM 1, pairing and link keys)
systemctl disable --now bluetooth.service
systemctl mask bluetooth.service
# also: no PulseAudio / PipeWire on this box

/usr/local/bin/btspeaker -list-devices     # find the S/PDIF card, e.g. "hw:Spdif,0"
/usr/local/bin/btspeaker -alsa-device hw:Spdif,0 -test-tone   # 2 s sine, no Bluetooth involved
```

`/etc/default/btspeaker` (every flag has a `BTSPEAKER_<FLAG>` variable):

```sh
BTSPEAKER_ALSA_DEVICE=hw:Spdif,0
BTSPEAKER_NAME=Cubox HiFi
BTSPEAKER_HTTP_PASSWORD=change-me
# BTSPEAKER_ADAPTER=hci0         # or a MAC address
# BTSPEAKER_VOLUME=off           # bit-perfect output, ignore the phone's volume
```

The file holds the web password: `chmod 600 /etc/default/btspeaker`.

```sh
systemctl daemon-reload
systemctl enable --now btspeaker
journalctl -u btspeaker -f
```

The unit runs as a dynamic user in the `audio` group with only `CAP_NET_ADMIN`, `CAP_NET_RAW`
(the mgmt socket needs them for everything but reads) and `CAP_NET_BIND_SERVICE` (L2CAP PSMs
below 0x1001: SDP, AVCTP, AVDTP). State (link keys, names) is in
`/var/lib/btspeaker/state.json`.

## Flags

| Flag | Default | |
|---|---|---|
| `-adapter` | first | `hciN` or MAC |
| `-name` | host's FQDN | speaker name (default: hostname qualified from `/etc/hosts` or the resolv.conf domain/search, no DNS); a name set via the UI/API is persisted and wins |
| `-alsa-device` | `hw:0,0` | `hw:CARD,DEV` only (no plug/dmix) |
| `-buffer` | `200ms` | ALSA buffer, `20ms`..`2s` |
| `-idle-release` | `5s` | release the device after this much digital silence; must be > 0 |
| `-spdif-preroll` | `200ms` | zeros played after each (re)open so an S/PDIF receiver can relock before audio starts; `0` = off; must not exceed `-buffer` |
| `-pairing-timeout` | `1m` | default pairing window |
| `-always-pairable` | off | permanently discoverable and accepting pairing: open Just Works pairing, anyone in range can pair (insecure) |
| `-no-legacy-pin` | off | refuse legacy PIN pairing instead of answering `0000` (sniffable) |
| `-sbc-max-bitpool` | `76` | max SBC bitpool advertised: `53` = standard high quality, `76`+ = SBC XQ (the source picks the actual value); `2`..`250` |
| `-volume` | `software` | `software` applies AVRCP volume as gain, `off` is bit-perfect |
| `-state-dir` | `/var/lib/btspeaker` | |
| `-listen` | `:8099` | web UI / API |
| `-http-user`, `-http-password` | `admin`, none | basic auth on every route when a password is set; after repeated failures (burst 10, then 5/s) a client gets `429` |
| `-http-hosts` | `localhost`, hostname, its FQDN (the default `-name`), `hostname.local` | allowed `Host` names (IP literals always allowed; blocks DNS rebinding); other hosts get `421` |
| `-list-devices`, `-test-tone` | | print ALSA playback devices / play a 2 s tone, then exit |

Basic auth is cleartext on plain HTTP: if that matters, use `-listen <LAN IP>` or a TLS proxy.

## Pairing

The adapter is connectable but **not** discoverable, and refuses pairing, until you open the pairing
window: press "Start pairing" in the web UI (`http://cubox:8099/`) or `POST /api/pairing`. The window
lasts `-pairing-timeout`, makes the speaker discoverable, and accepts the pairing request (Just Works
for SSP, PIN `0000` for legacy devices unless `-no-legacy-pin`). It closes after the first successful pairing or when the time
is up. Already-paired devices reconnect without the window (keys survive restarts).

The window is enforced by btspeaker itself, not just by discoverability (the kernel auto-accepts Just
Works pairing for a speaker with no display or buttons): outside the window the adapter is not
bondable, a link key from an unknown device is discarded and revoked, and AVDTP/AVRCP connections
from unpaired devices are refused. SDP stays open to everyone.

Only bonded devices are remembered. A source that pairs without bonding gets a session key that
the kernel drops whenever btspeaker reloads the stored keys (adapter re-init, or restoring a bonded
device's key); that source then has to pair again inside a window.

On shutdown btspeaker turns discoverable and bondable off. If that cannot complete (the process is
killed, or an adapter init holds things up for more than 2 s), the adapter can stay discoverable and
bondable until the next start.

## HTTP API

| Method | Path | |
|---|---|---|
| GET | `/` | UI |
| GET | `/api/status` | adapter, pairing window, devices (connected, last seen), stream, volume, last error, `volume_ignored`, `diagnostics` |
| POST / DELETE | `/api/pairing` `{"seconds":120}` | open (body optional; `seconds` 0..3600, 0 = `-pairing-timeout`) / close the pairing window |
| POST | `/api/devices/{addr}/disconnect` | disconnect a device |
| DELETE | `/api/devices/{addr}` | unpair and forget a device |
| POST | `/api/name` `{"name":"..."}` | rename the speaker |

```sh
curl -u admin:pw -X POST -d '{"seconds":60}' http://cubox:8099/api/pairing
```

JSON bodies with unknown fields are rejected with `400`.

`/api/status` extras:
- `volume_ignored` (omitted when false): `true` with `-volume off` (bit-perfect output); `volume` is
  then the phone's setting but is not applied.
- `diagnostics`: cumulative counters since start: `xruns` (ALSA underruns), `queue_drop_frames`
  (frames dropped when the player queue backs up over 100 ms), `lost_packets` (RTP gaps),
  `bad_frames` (undecodable SBC), `concealed_ms` (silence substituted for lost/bad audio),
  `dropped_dup` (duplicate/out-of-order packets), `resyncs` (RTP sequence jumps that restarted the decoder).

## mpd

Kernel ALSA cannot share a hw PCM, so btspeaker and mpd take turns. btspeaker holds the device only
while real (non-silent) audio flows and releases it after `-idle-release` of silence, on stream
suspend, or when the phone disconnects. mpd releases the device on pause/stop unless `always_on "yes"`
is set for its output; do not set it. If the other side holds the device, the UI shows
"ALSA device busy" and btspeaker retries every 3 s, so pausing mpd starts Bluetooth audio within
a few seconds. Only one of the two can be audible at a time.

## Latency

Audio is delayed by about 100-200 ms (start threshold of half the ALSA buffer plus the
queue). The S/PDIF preroll (`-spdif-preroll`, 200 ms) adds to that: first audio after each
open of the device is delayed by it. That is fine for music but noticeable for video unless the player compensates.

## Troubleshooting

- `bluetoothd is running`: `systemctl mask bluetooth.service` and stop it.
- `permission denied` on the mgmt socket: run as root or with `CAP_NET_ADMIN CAP_NET_RAW`.
- Adapter not found: `btmgmt info` (when bluez tools are installed) or check `dmesg` for `btusb`; the
  service keeps retrying and re-initializes if the adapter is re-plugged.
- `power on: Failed` / `rfkill-blocked`: btspeaker lifts a soft rfkill block itself (and restores it
  at exit) through `/dev/rfkill`, which the unit reaches via the `netdev` group (Debian's ifupdown
  udev rule; without it the unit fails to start: `groupadd -r netdev` and a udev rule
  `KERNEL=="rfkill", GROUP="netdev", MODE="0664"`). A hard block is a hardware switch.
- Pairing or AVDTP/AVRCP interop problems: run `sudo btmon` (add `-w trace.btsnoop` to capture) next
  to `journalctl -u btspeaker -f`.
- No sound: check `-list-devices`, that mpd is not holding the device, and the "stream" line in the UI.
- Laptop testing: `sudo systemctl stop bluetooth`, then `sudo ./btspeaker -adapter hci1 -alsa-device hw:CARD,0` (`aplay -l`; card names are stable).
