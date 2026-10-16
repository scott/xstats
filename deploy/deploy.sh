#!/usr/bin/env bash
# Run from the box. Copies the prebuilt binary, unit and Caddyfile to the droplet,
# installs Caddy, and sets basic auth. Nothing is built on the droplet.
# Config: put XSTATS_HOST, XSTATS_DOMAIN, etc. in ~/.config/xstats/deploy.env (or .env.deploy).
# Usage: XSTATS_HOST=203.0.113.10 XSTATS_DOMAIN=stats.example.com BASIC_AUTH_PASSWORD=... deploy/deploy.sh
set -euo pipefail
cd "$(dirname "$0")/.."
# Load local deploy config (gitignored). See .env.deploy.example.
DEPLOY_ENV="${XSTATS_DEPLOY_ENV:-$HOME/.config/xstats/deploy.env}"
[ -f .env.deploy ] && [ ! -f "$DEPLOY_ENV" ] && DEPLOY_ENV=.env.deploy
if [ -f "$DEPLOY_ENV" ]; then set -a; . "$DEPLOY_ENV"; set +a; fi
XSTATS_HOST="${XSTATS_HOST:?set XSTATS_HOST (server IP or hostname)}"
XSTATS_DOMAIN="${XSTATS_DOMAIN:?set XSTATS_DOMAIN (e.g. stats.example.com)}"
XSTATS_SSH_USER="${XSTATS_SSH_USER:-root}"
XSTATS_AUTH_USER="${XSTATS_AUTH_USER:-admin}"
SSH_KEY="${SSH_KEY:-$HOME/.ssh/id_ed25519}"
: "${BASIC_AUTH_PASSWORD:?set BASIC_AUTH_PASSWORD}"
T="$XSTATS_SSH_USER@$XSTATS_HOST"
O=(-i "$SSH_KEY" -o IdentitiesOnly=yes)

[ -x bin/xstats ] || CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/xstats ./cmd/xstats
scp "${O[@]}" bin/xstats deploy/xstats.service deploy/Caddyfile.tmpl deploy/set-auth.sh "$T:/tmp/"

ssh "${O[@]}" "$T" 'bash -s' <<'REMOTE'
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
if ! command -v caddy >/dev/null; then
  apt-get update -qq
  apt-get install -y -qq debian-keyring debian-archive-keyring apt-transport-https curl gpg
  curl -1sLf https://dl.cloudsmith.io/public/caddy/stable/gpg.key | gpg --dearmor --yes -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
  curl -1sLf https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt > /etc/apt/sources.list.d/caddy-stable.list
  apt-get update -qq && apt-get install -y -qq caddy
fi
id xstats >/dev/null 2>&1 || useradd --system --home /var/lib/xstats --shell /usr/sbin/nologin xstats
install -d -o xstats -g xstats /var/lib/xstats /var/lib/xstats/inbox
[ -f /usr/local/bin/xstats ] && cp -p /usr/local/bin/xstats /usr/local/bin/xstats.prev
[ -f /var/lib/xstats/xstats.db ] && cp -p /var/lib/xstats/xstats.db /var/lib/xstats/xstats.db.bak-$(date +%Y%m%d%H%M)
install -m 0755 /tmp/xstats /usr/local/bin/xstats
install -m 0644 /tmp/xstats.service /etc/systemd/system/xstats.service
install -d -m 0750 -g xstats /etc/xstats; [ -f /etc/xstats/env ] || install -m 0640 -g xstats /dev/null /etc/xstats/env
systemctl daemon-reload && systemctl enable xstats
ufw allow OpenSSH >/dev/null 2>&1 || true; ufw allow 80,443/tcp >/dev/null 2>&1 || true
REMOTE

# Ingest token over stdin into /etc/xstats/env (0640 root:xstats).
TOKEN_FILE="${XSTATS_TOKEN_FILE:-$HOME/.config/xstats/ingest_token}"
ssh "${O[@]}" "$T" 'install -d -m 0750 -g xstats /etc/xstats; umask 027; { printf "XSTATS_INGEST_TOKEN="; cat; echo; echo "XSTATS_ACCOUNTS=${XSTATS_ACCOUNTS:-}"; } > /etc/xstats/env; chgrp xstats /etc/xstats/env; systemctl restart xstats' < "$TOKEN_FILE"

# Password goes over stdin only: never on a command line, in a file, or in logs.
printf '%s\n' "$BASIC_AUTH_PASSWORD" | ssh "${O[@]}" "$T" "bash /tmp/set-auth.sh $XSTATS_DOMAIN $XSTATS_AUTH_USER"
echo "Deployed. Check: curl -u $XSTATS_AUTH_USER https://$XSTATS_DOMAIN/api/data"
