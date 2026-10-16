#!/usr/bin/env bash
# Runs ON the server. Usage: set-auth.sh DOMAIN USER (password on stdin). Reads the basic auth password from stdin, hashes it, writes the Caddyfile.
set -euo pipefail
HASH=$(caddy hash-password 2>/dev/null)
[[ "$HASH" == \$2* ]] || { echo "hash failed" >&2; exit 1; }
sed -e "s|__HASH__|$HASH|" -e "s|__DOMAIN__|$1|" -e "s|__USER__|$2|" /tmp/Caddyfile.tmpl > /etc/caddy/Caddyfile.new
caddy validate --config /etc/caddy/Caddyfile.new --adapter caddyfile >/dev/null 2>&1 || { echo "caddyfile invalid" >&2; exit 1; }
mv /etc/caddy/Caddyfile.new /etc/caddy/Caddyfile
systemctl reload caddy || systemctl restart caddy
rm -f /tmp/Caddyfile.tmpl /tmp/xstats /tmp/xstats.service /tmp/set-auth.sh
echo "auth set"
