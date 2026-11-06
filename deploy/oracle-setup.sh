#!/usr/bin/env bash
# Sets up the VaultDB playground on a fresh Ubuntu server (made for Oracle
# Cloud's free tier, works on any Ubuntu 22.04 or 24.04 machine).
#
# Run it on the server, replacing the domain with yours:
#
#   curl -fsSL https://raw.githubusercontent.com/ABDULRAIYAAN/vaultdb/main/deploy/oracle-setup.sh | sudo DOMAIN=example.com bash
#
# Run the same command again any time to update to the latest code.
#
# What it does:
#   1. installs Go, Git and Caddy (Caddy gets free HTTPS certificates automatically)
#   2. downloads VaultDB and builds the website program
#   3. runs it as a locked-down background service that restarts on failure
#   4. opens ports 80 and 443 in the server's own firewall
set -euo pipefail

DOMAIN="${DOMAIN:-}"
GO_VERSION="${GO_VERSION:-1.27.3}"
REPO="${REPO:-https://github.com/ABDULRAIYAAN/vaultdb.git}"
APP_DIR=/opt/vaultdb

say() { printf '\n\033[1;33m==> %s\033[0m\n' "$*"; }
die() { printf '\n\033[1;31mError: %s\033[0m\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "run this with sudo"
[ -n "$DOMAIN" ] || die "set your domain, e.g. sudo DOMAIN=example.com bash oracle-setup.sh"
case "$DOMAIN" in *[!a-zA-Z0-9.-]*) die "the domain may only contain letters, digits, dots and dashes";; esac

case "$(uname -m)" in
  aarch64|arm64) GOARCH=arm64 ;;
  x86_64|amd64)  GOARCH=amd64 ;;
  *) die "unsupported processor $(uname -m)" ;;
esac

say "Installing system packages"
export DEBIAN_FRONTEND=noninteractive
apt-get update -q
apt-get install -y -q git curl ca-certificates gnupg debian-keyring debian-archive-keyring apt-transport-https iptables-persistent

if ! command -v caddy >/dev/null; then
  say "Installing Caddy (web server with automatic HTTPS)"
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | gpg --dearmor --yes -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' > /etc/apt/sources.list.d/caddy-stable.list
  apt-get update -q
  apt-get install -y -q caddy
fi

if ! /usr/local/go/bin/go version 2>/dev/null | grep -q "go$GO_VERSION "; then
  say "Installing Go $GO_VERSION ($GOARCH)"
  curl -fsSL "https://go.dev/dl/go$GO_VERSION.linux-$GOARCH.tar.gz" -o /tmp/go.tgz
  rm -rf /usr/local/go
  tar -C /usr/local -xzf /tmp/go.tgz
  rm /tmp/go.tgz
fi
export PATH=/usr/local/go/bin:$PATH

say "Downloading and building VaultDB"
if [ -d "$APP_DIR/src/.git" ]; then
  git -C "$APP_DIR/src" fetch -q origin main
  git -C "$APP_DIR/src" reset -q --hard origin/main
else
  mkdir -p "$APP_DIR"
  git clone -q "$REPO" "$APP_DIR/src"
fi
cd "$APP_DIR/src"
GOFLAGS=-trimpath go build -ldflags="-s -w" -o "$APP_DIR/vaultdb-web.new" ./cmd/vaultdb-web
mv "$APP_DIR/vaultdb-web.new" "$APP_DIR/vaultdb-web"

# The docs assistant answers from a copy of the documentation and code.
rm -rf "$APP_DIR/docs"
mkdir -p "$APP_DIR/docs"
cp -r README.md docs cmd internal "$APP_DIR/docs/"

# Settings and API keys (only readable by root and the service).
mkdir -p /etc/vaultdb
if [ ! -f /etc/vaultdb/env ]; then
  cat > /etc/vaultdb/env <<'ENV'
# Settings for the VaultDB website. After editing: sudo systemctl restart vaultdb-web
# Which AI writes answers in "Ask the docs" (none = show the passages only).
VAULTDB_EMBED=local
VAULTDB_LLM=none
# GEMINI_API_KEY=
# GROQ_API_KEY=
ENV
fi
chmod 600 /etc/vaultdb/env

say "Installing the background service"
cp "$APP_DIR/src/deploy/vaultdb-web.service" /etc/systemd/system/vaultdb-web.service
systemctl daemon-reload
systemctl enable -q vaultdb-web
systemctl restart vaultdb-web

say "Configuring Caddy for https://$DOMAIN"
sed "s/{DOMAIN}/$DOMAIN/g" "$APP_DIR/src/deploy/Caddyfile" > /etc/caddy/Caddyfile
caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile >/dev/null
systemctl enable -q caddy
systemctl reload caddy || systemctl restart caddy

say "Opening ports 80 and 443 in the server firewall"
# Oracle's Ubuntu images block everything except SSH by default.
for port in 80 443; do
  iptables -C INPUT -p tcp --dport "$port" -m state --state NEW -j ACCEPT 2>/dev/null ||
    iptables -I INPUT 6 -p tcp --dport "$port" -m state --state NEW -j ACCEPT
done
netfilter-persistent save >/dev/null 2>&1 || true

sleep 2
if curl -fsS http://127.0.0.1:8080/api/stats >/dev/null; then
  say "Done. VaultDB is running."
  echo "  Site:     https://$DOMAIN  (the HTTPS certificate appears within a minute once DNS points here)"
  echo "  Logs:     sudo journalctl -u vaultdb-web -f"
  echo "  Settings: sudo nano /etc/vaultdb/env, then sudo systemctl restart vaultdb-web"
  echo "  Update:   run this same command again"
else
  die "the service did not start; see: sudo journalctl -u vaultdb-web -n 50"
fi
