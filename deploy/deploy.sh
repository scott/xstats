#!/usr/bin/env bash
# Run from the box. Copies the prebuilt binary, unit and Caddyfile to the droplet,
# installs Caddy, and sets basic auth. Nothing is built on the droplet.
# Config: put XSTATS_HOST, XSTATS_DOMAIN, etc. in ~/.config/xstats/deploy.env (or .env.deploy).
# XSTATS_TZ, when set, is checked locally and written to /etc/xstats/env. Omit it to keep the binary default.
# XSTATS_START_DATES, when set, is checked locally and written to /etc/xstats/env. Omit it for no start dates.
# The service reads both at startup. deploy.sh restarts xstats after writing the env file.
# Usage: XSTATS_HOST=203.0.113.10 XSTATS_DOMAIN=stats.example.com BASIC_AUTH_PASSWORD=... deploy/deploy.sh
set -euo pipefail

# trim_space strips leading and trailing whitespace, matching the binary's
# TrimSpace on XSTATS_TZ. An empty result means "unset".
trim_space() {
  local s="$1"
  s="${s#"${s%%[![:space:]]*}"}"
  s="${s%"${s##*[![:space:]]}"}"
  printf '%s' "$s"
}

# render_xstats_env prints the /etc/xstats/env body. The ingest token is read
# from stdin so it is never placed on a command line. Trailing newlines on the
# token are removed; the rest of the token is unchanged. XSTATS_TZ and
# XSTATS_START_DATES are written only when set. Omitting XSTATS_TZ leaves
# the binary default. Omitting XSTATS_START_DATES means no start dates.
render_xstats_env() {
  local token
  # $(cat) strips trailing newlines only. printf is a shell builtin, so the
  # token is not passed as an argument to an external program.
  token=$(cat)
  printf 'XSTATS_INGEST_TOKEN=%s\n' "$token"
  printf 'XSTATS_ACCOUNTS=%s\n' "${XSTATS_ACCOUNTS:-}"
  if [ -n "${XSTATS_TZ:-}" ]; then
    printf 'XSTATS_TZ=%s\n' "$XSTATS_TZ"
  fi
  if [ -n "${XSTATS_START_DATES:-}" ]; then
    printf 'XSTATS_START_DATES=%s\n' "$XSTATS_START_DATES"
  fi
}

# validate_xstats_tz rejects a name the server binary cannot load.
# time.LoadLocation with embedded tzdata is the same check as statsLocation,
# so a zone with no file on this machine still deploys when the binary has it.
# Called before scp/ssh.
validate_xstats_tz() {
  local zone="$1" dir err
  dir=$(mktemp -d)
  trap 'rm -rf "$dir"' RETURN
  cat >"$dir/main.go" <<'EOF'
package main

import (
	"fmt"
	"os"
	"time"

	_ "time/tzdata"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: checktz ZONE")
		os.Exit(2)
	}
	if _, err := time.LoadLocation(os.Args[1]); err != nil {
		fmt.Fprintf(os.Stderr, "invalid XSTATS_TZ %q: %v\n", os.Args[1], err)
		os.Exit(1)
	}
}
EOF
  if ! err=$(cd "$dir" && go mod init xstatstz >/dev/null 2>&1 && go build -o checktz . && ./checktz "$zone" 2>&1); then
    if [ -n "$err" ]; then
      printf '%s\n' "$err" >&2
    else
      printf 'invalid XSTATS_TZ %q\n' "$zone" >&2
    fi
    return 1
  fi
}

# validate_xstats_start_dates rejects a value the server would refuse at
# startup. go run uses this checkout, so the check matches the binary that
# deploy builds. Called before scp/ssh. A bad pair is printed and the
# deploy stops.
validate_xstats_start_dates() {
  local value="$1" dir err
  dir=$(mktemp -d)
  trap 'rm -rf "$dir"' RETURN
  if ! err=$(XSTATS_START_DATES="$value" go run ./cmd/xstats ingest --db "$dir/check.db" 2>&1); then
    if [ -n "$err" ]; then
      printf '%s\n' "$err" >&2
    else
      printf 'invalid XSTATS_START_DATES %q\n' "$value" >&2
    fi
    return 1
  fi
}

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

if [ -n "${XSTATS_TZ+x}" ]; then
  XSTATS_TZ=$(trim_space "$XSTATS_TZ")
fi
if [ -n "${XSTATS_TZ:-}" ]; then
  validate_xstats_tz "$XSTATS_TZ"
fi
if [ -n "${XSTATS_START_DATES+x}" ]; then
  XSTATS_START_DATES=$(trim_space "$XSTATS_START_DATES")
fi
if [ -n "${XSTATS_START_DATES:-}" ]; then
  validate_xstats_start_dates "$XSTATS_START_DATES"
fi
# Same gate as the zone check: fail before the build, scp, or ssh.
TOKEN_FILE="${XSTATS_TOKEN_FILE:-$HOME/.config/xstats/ingest_token}"
if [ ! -r "$TOKEN_FILE" ]; then
  printf 'cannot read ingest token file: %s\n' "$TOKEN_FILE" >&2
  exit 1
fi

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

# Env file over stdin (0640 root:xstats). The token stays off the command line.
render_xstats_env < "$TOKEN_FILE" | ssh "${O[@]}" "$T" 'install -d -m 0750 -g xstats /etc/xstats; umask 027; cat > /etc/xstats/env; chgrp xstats /etc/xstats/env; systemctl restart xstats'

# Password goes over stdin only: never on a command line, in a file, or in logs.
printf '%s\n' "$BASIC_AUTH_PASSWORD" | ssh "${O[@]}" "$T" "bash /tmp/set-auth.sh $XSTATS_DOMAIN $XSTATS_AUTH_USER"
echo "Deployed. Check: curl -u $XSTATS_AUTH_USER https://$XSTATS_DOMAIN/api/data"
