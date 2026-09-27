#!/bin/sh
# Install the Laika agent on a Linux server with systemd, and enrol it.
#
#   sudo sh install.sh --url https://platform.example/api/agent/v1 --token lie_…
#
# Options:
#   --url URL         the platform, as "Add a server" shows it
#   --token TOKEN     the one-time enrolment token (or LAIKA_ENROLMENT_TOKEN)
#   --binary FILE     install this build instead of downloading one
#   --version X.Y.Z   the release to download (default: the latest this script knows)
#   --uninstall       stop and remove the agent; its file in /etc/laika-agent stays
#   --purge           with --uninstall, remove that file too
#
# It installs /usr/local/bin/laika-agent, makes the system user laika-agent,
# installs and starts the laika-agent systemd service, and enrols with the
# token (passed on standard input, so it is not in the process list). A
# download is checked against the release's SHA256SUMS before it is used.
set -eu

VERSION="${LAIKA_AGENT_VERSION:-0.1.0}"
RELEASES="https://github.com/laikait/lip-agent/releases/download"
BIN=/usr/local/bin/laika-agent
UNIT=/etc/systemd/system/laika-agent.service
CONF_DIR=/etc/laika-agent
ACCOUNT=laika-agent

URL=""
TOKEN="${LAIKA_ENROLMENT_TOKEN:-}"
BINARY=""
UNINSTALL=0
PURGE=0

say() { printf '%s\n' "$*"; }
die() { printf 'laika-agent install: %s\n' "$*" >&2; exit 1; }

while [ $# -gt 0 ]; do
    case "$1" in
        --url) URL="${2:-}"; shift 2 ;;
        --token) TOKEN="${2:-}"; shift 2 ;;
        --binary) BINARY="${2:-}"; shift 2 ;;
        --version) VERSION="${2:-}"; shift 2 ;;
        --uninstall) UNINSTALL=1; shift ;;
        --purge) PURGE=1; shift ;;
        -h|--help) sed -n '2,20p' "$0"; exit 0 ;;
        *) die "unknown option $1 (see --help)" ;;
    esac
done

[ "$(id -u)" -eq 0 ] || die "run it as root, or with sudo"
command -v systemctl >/dev/null 2>&1 || die "this installer needs systemd; run laika-agent by hand elsewhere"

if [ "$UNINSTALL" -eq 1 ]; then
    systemctl disable --now laika-agent 2>/dev/null || true
    rm -f "$UNIT" "$BIN"
    systemctl daemon-reload
    if [ "$PURGE" -eq 1 ]; then
        rm -rf "$CONF_DIR"
        say "Removed, with its file. Remove the server in the portal too."
    else
        say "Removed. Its file stays in $CONF_DIR; --purge removes it."
    fi
    exit 0
fi

case "$(uname -m)" in
    x86_64|amd64) ARCH=amd64 ;;
    aarch64|arm64) ARCH=arm64 ;;
    *) die "no build for $(uname -m): amd64 and arm64 only" ;;
esac

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

if [ -n "$BINARY" ]; then
    [ -f "$BINARY" ] || die "$BINARY does not exist"
    cp "$BINARY" "$WORK/laika-agent"
else
    NAME="laika-agent-linux-$ARCH"
    say "Downloading laika-agent $VERSION for $ARCH"
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL --proto '=https' --tlsv1.2 -o "$WORK/$NAME" "$RELEASES/v$VERSION/$NAME"
        curl -fsSL --proto '=https' --tlsv1.2 -o "$WORK/SHA256SUMS" "$RELEASES/v$VERSION/SHA256SUMS"
    elif command -v wget >/dev/null 2>&1; then
        wget -q --https-only -O "$WORK/$NAME" "$RELEASES/v$VERSION/$NAME"
        wget -q --https-only -O "$WORK/SHA256SUMS" "$RELEASES/v$VERSION/SHA256SUMS"
    else
        die "neither curl nor wget is installed; download the binary and pass --binary"
    fi
    # A line is "<hash>  <name>", or "<hash> *<name>" when summed in binary mode.
    (cd "$WORK" && grep -E "[ *]${NAME}\$" SHA256SUMS | sed 's/ \*/  /' | sha256sum -c -) >/dev/null || die "the download does not match SHA256SUMS; nothing was installed"
    mv "$WORK/$NAME" "$WORK/laika-agent"
fi

install -m 0755 "$WORK/laika-agent" "$BIN"
say "Installed $("$BIN" version)"

if ! id "$ACCOUNT" >/dev/null 2>&1; then
    NOLOGIN="$(command -v nologin || echo /usr/sbin/nologin)"
    if command -v useradd >/dev/null 2>&1; then
        useradd --system --no-create-home --home-dir / --shell "$NOLOGIN" "$ACCOUNT"
    else
        adduser -S -D -H -h / -s "$NOLOGIN" "$ACCOUNT"
    fi
    say "Made the system user $ACCOUNT"
fi

cat > "$UNIT" <<'UNIT'
[Unit]
Description=Laika agent: reports this server to the Laika Infrastructure Platform
Documentation=https://github.com/laikait/lip-agent
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
User=laika-agent
Group=laika-agent
ExecStart=/usr/local/bin/laika-agent run
Restart=on-failure
RestartSec=30
# 78: not enrolled, or the platform no longer accepts it. Restarting will not help.
RestartPreventExitStatus=78

# It reads /proc, the root filesystem's size and its own file, asks systemd
# over D-Bus for the services, and talks HTTPS out. Nothing else.
NoNewPrivileges=yes
CapabilityBoundingSet=
AmbientCapabilities=
ProtectSystem=strict
ProtectHome=yes
ReadOnlyPaths=/etc/laika-agent
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
SystemCallArchitectures=native
UMask=0077

[Install]
WantedBy=multi-user.target
UNIT
chmod 0644 "$UNIT"
systemctl daemon-reload

if [ -n "$URL" ] && [ -n "$TOKEN" ]; then
    printf '%s\n' "$TOKEN" | "$BIN" enrol --url "$URL" --token -
elif [ ! -f "$CONF_DIR/agent.json" ]; then
    say "Installed, not enrolled. Next: sudo laika-agent enrol --url <platform> --token <token>, then systemctl enable --now laika-agent"
    exit 0
fi

systemctl enable --now laika-agent
say "Reporting. Follow it with: journalctl -u laika-agent -f"
