#!/bin/sh
# The Repo Depot agent installer for Linux (systemd).
#   curl -fsSL __SERVER__/agent/install.sh | sudo sh -s -- --token TOKEN [--port 8471] [--apps-dir /opt/repodepot/apps]
# Running it again updates the agent and keeps its settings.
set -eu

SERVER="__SERVER__"
TOKEN=""
PORT=""
APPS_DIR=""
while [ $# -gt 0 ]; do
  case "$1" in
    --token) TOKEN="$2"; shift 2 ;;
    --port) PORT="$2"; shift 2 ;;
    --apps-dir) APPS_DIR="$2"; shift 2 ;;
    *) echo "Unknown option: $1" >&2; exit 2 ;;
  esac
done

if [ "$(id -u)" -ne 0 ]; then
  echo "Run this with sudo: the agent installs software, so it runs as root." >&2
  exit 1
fi
command -v systemctl >/dev/null 2>&1 || { echo "This installer needs systemd." >&2; exit 1; }

case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "Unsupported CPU: $(uname -m)" >&2; exit 1 ;;
esac

CONF_DIR=/etc/repodepot-agent
CONF="$CONF_DIR/config.json"
BIN=/usr/local/bin/repodepot-agent

if [ -z "$TOKEN" ] && [ ! -f "$CONF" ]; then
  echo "Pass --token (shown when you add the machine in The Repo Depot)." >&2
  exit 1
fi

echo "Downloading the agent ($ARCH) from $SERVER"
TMP="$(mktemp)"
if command -v curl >/dev/null 2>&1; then
  curl -fsSL "$SERVER/agent/download/linux-$ARCH" -o "$TMP"
else
  wget -qO "$TMP" "$SERVER/agent/download/linux-$ARCH"
fi
chmod 755 "$TMP"
"$TMP" version >/dev/null
mv -f "$TMP" "$BIN"

mkdir -p "$CONF_DIR"
chmod 700 "$CONF_DIR"
if [ -f "$CONF" ] && [ -z "$TOKEN" ]; then
  echo "Keeping the existing $CONF"
else
  OLD_PORT=""; OLD_DIR=""
  if [ -f "$CONF" ]; then
    OLD_PORT="$(sed -n 's/.*"listen" *: *":\([0-9]*\)".*/\1/p' "$CONF")"
    OLD_DIR="$(sed -n 's/.*"apps_dir" *: *"\([^"]*\)".*/\1/p' "$CONF")"
  fi
  PORT="${PORT:-${OLD_PORT:-8471}}"
  APPS_DIR="${APPS_DIR:-${OLD_DIR:-/opt/repodepot/apps}}"
  umask 077
  printf '{\n  "token": "%s",\n  "listen": ":%s",\n  "apps_dir": "%s"\n}\n' "$TOKEN" "$PORT" "$APPS_DIR" > "$CONF"
  umask 022
fi
PORT="${PORT:-$(sed -n 's/.*"listen" *: *":\([0-9]*\)".*/\1/p' "$CONF")}"
mkdir -p "${APPS_DIR:-/opt/repodepot/apps}"

cat > /etc/systemd/system/repodepot-agent.service <<UNIT
[Unit]
Description=The Repo Depot agent
After=network-online.target docker.service
Wants=network-online.target

[Service]
Type=simple
ExecStart=$BIN -config $CONF
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable repodepot-agent >/dev/null 2>&1
systemctl restart repodepot-agent

if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q "Status: active"; then
  ufw allow "$PORT/tcp" >/dev/null && echo "Opened port $PORT in ufw"
fi
if command -v csf >/dev/null 2>&1; then
  echo "Note: this machine runs CSF. Allow TCP port $PORT in /etc/csf/csf.conf (TCP_IN) and run 'csf -r' if the server can't reach the agent."
fi

sleep 1
if systemctl is-active --quiet repodepot-agent; then
  echo "The Repo Depot agent $("$BIN" version) is running on port $PORT."
else
  echo "The agent didn't start. See: journalctl -u repodepot-agent -n 50" >&2
  exit 1
fi
