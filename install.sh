#!/bin/sh
# Kerf Bridge installer for Raspberry Pi OS and other Linux with systemd.
#
#   curl -fsSL https://trykerf.com/install.sh | sh
#
# Installs /usr/local/bin/kerf-bridge, a "kerf" system user in the dialout
# group, and a kerf-bridge service that starts at boot. Running it again
# updates the bridge and keeps your settings (/etc/kerf-bridge/config.json).
set -eu

BASE="https://trykerf.com/downloads/bridge"
say() { printf '%s\n' "$*"; }
die() { say "Kerf Bridge: $*" >&2; exit 1; }

[ "$(uname -s)" = Linux ] || die "this installer is for Linux. For Windows or Mac, download the bridge from https://trykerf.com/#bridge"
command -v systemctl >/dev/null 2>&1 || die "systemd is needed to run the bridge as a service."
command -v curl >/dev/null 2>&1 || die "curl is needed."

SUDO=""
if [ "$(id -u)" -ne 0 ]; then
    command -v sudo >/dev/null 2>&1 || die "run this as root, or install sudo."
    SUDO="sudo"
fi

case "$(uname -m)" in
    aarch64|arm64) ARCH=linux-arm64 ;;
    armv7l)        ARCH=linux-armv7 ;;
    armv6l)        die "the original Pi Zero and Pi 1 aren't supported; use a Pi Zero 2 W, 3, 4 or 5." ;;
    x86_64|amd64)  ARCH=linux-x64 ;;
    *) die "no build for $(uname -m) yet." ;;
esac

LATEST=$(curl -fsSL "$BASE/latest.json") || die "couldn't reach trykerf.com."
FILE=$(printf '%s' "$LATEST" | sed -n "s/.*\"$ARCH\": *\"\([^\"]*\)\".*/\1/p")
SUMS=$(printf '%s' "$LATEST" | sed -n 's/.*"sums": *"\([^"]*\)".*/\1/p')
VERSION=$(printf '%s' "$LATEST" | sed -n 's/.*"version": *"\([^"]*\)".*/\1/p')
[ -n "$FILE" ] && [ -n "$SUMS" ] || die "couldn't read the list of downloads."

TMP=$(mktemp -d)
say "Downloading Kerf Bridge $VERSION for $ARCH…"
curl -fsSL -o "$TMP/$FILE" "$BASE/$FILE" || die "download failed."
curl -fsSL -o "$TMP/sums" "$BASE/$SUMS" || die "checksum download failed."
WANT=$(grep " $FILE\$" "$TMP/sums" | cut -d' ' -f1)
if command -v sha256sum >/dev/null 2>&1; then
    GOT=$(sha256sum "$TMP/$FILE" | cut -d' ' -f1)
else
    GOT=$(shasum -a 256 "$TMP/$FILE" | cut -d' ' -f1)
fi
[ -n "$WANT" ] && [ "$WANT" = "$GOT" ] || die "the download didn't match its checksum; nothing was installed."

UPDATE=0
[ -x /usr/local/bin/kerf-bridge ] && UPDATE=1

say "Installing…"
$SUDO install -m 0755 "$TMP/$FILE" /usr/local/bin/kerf-bridge
id kerf >/dev/null 2>&1 || $SUDO useradd --system --no-create-home --shell /usr/sbin/nologin kerf
getent group dialout >/dev/null 2>&1 && $SUDO usermod -aG dialout kerf
# The camera over the bed (/dev/video*).
getent group video >/dev/null 2>&1 && $SUDO usermod -aG video kerf
$SUDO mkdir -p /etc/kerf-bridge
$SUDO chown kerf /etc/kerf-bridge

# LightBurn's pass-through port is 23. If something else already has it —
# ser2net, usually — leave that alone and use 2323 instead.
if [ ! -f /etc/kerf-bridge/config.json ]; then
    RAW=":23"
    if command -v ss >/dev/null 2>&1 && ss -ltn 2>/dev/null | grep -q ':23 '; then
        RAW=":2323"
        PORT_NOTE=1
    fi
    printf '{\n  "httpAddr": ":8023",\n  "rawAddr": "%s",\n  "name": "Kerf Bridge on %s"\n}\n' "$RAW" "$(hostname)" \
        | $SUDO tee /etc/kerf-bridge/config.json >/dev/null
    $SUDO chown kerf /etc/kerf-bridge/config.json
fi

$SUDO tee /etc/systemd/system/kerf-bridge.service >/dev/null <<'EOF'
[Unit]
Description=Kerf Bridge (GRBL laser on the network)
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/kerf-bridge -config /etc/kerf-bridge/config.json
User=kerf
Group=dialout
# The camera over the bed (/dev/video*).
SupplementaryGroups=video
# Port 23 (pass-through for LightBurn) is privileged on Linux.
AmbientCapabilities=CAP_NET_BIND_SERVICE
Restart=always
RestartSec=2
# Stopping holds and resets a running job first; give it time.
TimeoutStopSec=10

[Install]
WantedBy=multi-user.target
EOF
$SUDO systemctl daemon-reload
$SUDO systemctl enable kerf-bridge >/dev/null 2>&1
# A restart resets the laser's controller, so an update says so first.
[ "$UPDATE" = 1 ] && say "Restarting the bridge (this resets the laser's controller — home it again before the next job)."
$SUDO systemctl restart kerf-bridge

IP=$(hostname -I 2>/dev/null | cut -d' ' -f1)
say ""
say "Kerf Bridge $VERSION is running."
say "  Web page:  http://${IP:-$(hostname).local}:8023"
say "  In Kerf:   it appears under Kerf Bridges on this network."
if [ "${PORT_NOTE:-0}" = 1 ]; then
    say "  Port 23 was already in use (ser2net?), so LightBurn pass-through is on port 2323."
    say "  To use 23, stop the other service and change rawAddr in /etc/kerf-bridge/config.json."
fi
