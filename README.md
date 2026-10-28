# Kerf Bridge

The source of [Kerf Bridge](https://trykerf.com), the free bridge that puts a
GRBL laser on your network for [Kerf for iPad](https://trykerf.com) and
LightBurn. It's here so you can read what you're running before you run it.

- **`install.sh`** is the exact installer behind
  `curl -fsSL https://trykerf.com/install.sh | sh`. Read it first, or
  download it, read it and run it yourself — the
  [set-up guide](https://trykerf.com/setup/#pi) shows how.
- The downloads on trykerf.com are built from this code with the commands
  under [Build](#build); each release is tagged here, and the site publishes a
  SHA-256 checksum for every file.

Source-visible, not open source: see [LICENSE](LICENSE).

Sits between a GRBL laser's USB port and the network. One file, no runtime:
Raspberry Pi (64- or 32-bit), Windows, Linux PCs, macOS.

- **Job mode** — Kerf uploads the whole job over HTTP and the bridge streams
  it to the laser itself. The iPad can sleep, leave the room or lose Wi-Fi;
  the burn carries on. Reconnect any time to watch, pause or stop.
- **Pass-through** — raw GRBL on TCP port 23, the same as ser2net, so
  LightBurn's *GRBL → Ethernet/TCP* device keeps working unchanged.
- **Web page** on port 8023: status, position, job progress, pause/resume,
  STOP, jog, home, unlock, run a G-code file, settings.
- **Bonjour** — announced as `_kerf._tcp`, so Kerf finds it by name.

## One controller at a time

Whoever starts first owns the laser until they finish: a running job, or a
pass-through client (LightBurn, or Kerf in direct mode). Anyone else who
connects to the raw port can watch everything the laser says, and the only
bytes of theirs that reach it are feed hold (`!`), reset (Ctrl-X) and status
(`?`) — so anyone can always stop it. Uploads and commands are refused, with
the reason, while someone else is in control. **STOP** on the web page and
`POST /api/stop` work no matter who is in control.

The serial port is opened once and held. Opening a port toggles DTR, which
resets most GRBL boards; clients coming and going never reopen it.

If the bridge is stopped while running a job (service stop, Ctrl-C), it holds
and resets the laser first instead of leaving it mid-line.

## Run it

```
kerf-bridge                       # settings in the user config dir
kerf-bridge -config /etc/kerf-bridge/config.json
```

First run writes the settings file:

```json
{
  "serialPort": "",          // "" = find the laser; or /dev/ttyUSB0, COM3
  "baud": 0,                 // 0 = try 115200, 250000, 230400, 57600, 9600
  "httpAddr": ":8023",
  "rawAddr": ":23",          // "" turns pass-through off
  "name": "Kerf Bridge on <host>"
}
```

Finding the laser opens each USB serial port (CH340, CP210x, FTDI, Arduino,
ESP32, STM32 first) and waits for GRBL's banner, sending Ctrl-X if it doesn't
come. **Opening a port resets whatever is on it — if this machine also
drives a 3D printer, set `serialPort` instead of letting it search.**

### Camera

Plug a USB camera (UVC, MJPEG — nearly all of them) into the same machine
and point it at the bed: the web page shows it live, and so does Kerf. It's
read straight through Video4Linux2 on 64-bit Linux, no ffmpeg needed. The
camera is only open while someone is watching. Settings: `cameraDevice`
(`""` finds it, `"off"`, or `/dev/videoN`), `cameraWidth`/`cameraHeight`
(default 1600 × 1200) and `cameraFocus` (`-1` autofocus, else a fixed
focus). The bridge's user must be in the `video` group — the service file
adds it. To check the camera without touching the laser:
`kerf-bridge -config /etc/kerf-bridge/config.json -snapshot test.jpg` (set `KERF_CAMDEBUG=1` to see why each
device was or wasn't used).

### Home Assistant

Give the bridge an MQTT broker on its web page (Settings → Home Assistant:
the Mosquitto add-on's address, and a Home Assistant user to log in as) and
a "Kerf Bridge" device appears in Home Assistant by itself, through MQTT
discovery — nothing to install there. Sensors: laser state, job, job state,
progress, time left, head X/Y; binary sensors: laser connected, burning;
buttons: Stop, Pause, Resume (Stop always works, so an automation can stop
the laser); a camera with a picture of the bed every 5 s while a job runs.
Settings: `mqttBroker` (`""` off; port 1883 unless given), `mqttUsername`,
`mqttPassword` (never sent back by the API), `mqttPrefix` (default
`homeassistant`), `mqttCamera`. Topics are under `kerf/<node>/`, where the
node comes from the bridge's name (`kerf_bridge_pibuild`).

Port 23 needs privileges on Linux: the service file grants
`CAP_NET_BIND_SERVICE`. **It can't share port 23 with ser2net** — stop
ser2net, or set `rawAddr` to another port and point LightBurn there.

### Raspberry Pi service

```
sudo install -m 755 kerf-bridge-linux-arm64 /usr/local/bin/kerf-bridge
sudo useradd --system --no-create-home --shell /usr/sbin/nologin --groups dialout kerf
sudo install -d -o kerf -g kerf /etc/kerf-bridge      # the page saves settings here
sudo install -m 644 kerf-bridge.service /etc/systemd/system/kerf-bridge.service
sudo systemctl enable --now kerf-bridge
journalctl -u kerf-bridge -f
```

## API

JSON throughout; errors are `{"error": "…"}` with 4xx/5xx.

| | |
|---|---|
| `GET /api/status` | bridge, connection, GRBL status, controller, job |
| `POST /api/job?name=…` | body: G-code. 201 and starts; 409 if busy; 400 if unrunnable |
| `POST /api/job/pause`, `/resume` | |
| `POST /api/stop` | hold, wait ≤ 1 s, reset — always allowed |
| `POST /api/command` | `{"line": "$H"}`, answers in `replies`; only when free |
| `GET /api/status` → `homeAssistant` | `enabled`, `connected`, `broker`, `error` |
| `GET /api/camera` | the camera: `available`, `name`, `width`, `height`, `error` |
| `GET /api/camera/frame?after=N` | the next JPEG after frame N (its number in `X-Frame`); waits up to 4 s |
| `GET /api/camera/stream` | live MJPEG, for an `<img>` |
| `POST /api/camera/focus` | `{"lock": true}` fixes the focus where autofocus has it, `{"focus": N}` fixes it at N, `{"focus": -1}` back to auto; saved as `cameraFocus` |
| `GET /api/ports` | serial ports seen |
| `GET`/`PUT /api/config` | name, port, baud (applied at next connect) |

**Not yet:** pairing. Anyone on the LAN can use the page and the API, as with
ser2net. Keep it off networks you don't trust until pairing lands.

## Build

Go 1.27.3. The downloads on trykerf.com are built with exactly these
commands (`VERSION` from `api.go`), so the same Go version gives the same
file, byte for byte — compare its SHA-256 with the site's `SHA256SUMS`:

```
go test -race ./...
V=0.4.0
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -buildvcs=false -ldflags "-s -w -X main.Version=$V" -o kerf-bridge-linux-arm64 .
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -buildvcs=false -ldflags "-s -w -X main.Version=$V" -o kerf-bridge-linux-armv7 .
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -ldflags "-s -w -X main.Version=$V" -o kerf-bridge-linux-x64 .
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -buildvcs=false -ldflags "-s -w -X main.Version=$V" -o kerf-bridge-windows-x64.exe .
```

(0.4.0 was built before `-buildvcs=false`, so its files also carry Go's
record of the git commit they were built from; `go version -m` shows that is
the only difference. The Mac build uses cgo and Apple's linker, so it isn't
byte-for-byte.)
