#!/usr/bin/env bash
# Build release assets, serve them locally, and run bootstrap.sh in
# Ubuntu 22.04 and 24.04 containers. systemctl, caddy, and apt are stubbed
# inside the container. This does not install Caddy from the network or
# start systemd.
set -euo pipefail
cd "$(dirname "$0")/.."
ROOT=$(pwd)

if grep -n 'set -x' deploy/bootstrap.sh; then
  printf 'bootstrap.sh must not enable xtrace\n' >&2
  exit 1
fi
if grep -nE 'America/Denver|Mountain' deploy/bootstrap.sh; then
  printf 'bootstrap.sh hardcodes a zone\n' >&2
  exit 1
fi

if docker info >/dev/null 2>&1; then
  docker=(docker)
elif sudo docker info >/dev/null 2>&1; then
  docker=(sudo docker)
else
  printf 'docker is required\n' >&2
  exit 1
fi

work=$(mktemp -d)
http_pid=
cleanup() {
  if [ -n "${http_pid:-}" ]; then
    kill "$http_pid" 2>/dev/null || true
    wait "$http_pid" 2>/dev/null || true
  fi
  if [ -n "${work:-}" ] && [ -d "$work" ]; then
    sudo rm -rf "$work"
  fi
}
trap cleanup EXIT

umask 077
printf '%s\n' 'dummy-ingest-token-7f3a9c' > "$work/token"
printf '%s\n' 'dummy-secret-password-9c2e4b' > "$work/password"
printf '%s\n' 'dummy-ingest-token-rotate-11ab' > "$work/token2"
umask 022

head=$(git rev-parse HEAD)
scripts/build-release.sh v0.0.0-bootstrap1 "$head" "$work/rel1"
scripts/build-release.sh v0.0.0-bootstrap2 "${head}-rerun" "$work/rel2"
if cmp -s "$work/rel1/xstats-linux-amd64" "$work/rel2/xstats-linux-amd64"; then
  printf 'release builds did not differ\n' >&2
  exit 1
fi

reset_www() {
  sudo rm -rf "$work/www"
  mkdir -p "$work/www/deploy"
  cp "$work/rel1/xstats-linux-amd64" "$work/rel1/xstats-linux-arm64" "$work/rel1/SHA256SUMS" "$work/www/"
  cp deploy/xstats.service deploy/Caddyfile.tmpl deploy/set-auth.sh "$work/www/deploy/"
  chmod -R a+rX "$work/www"
}

reset_www

port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')
python3 -m http.server "$port" --bind 127.0.0.1 --directory "$work/www" >"$work/http.log" 2>&1 &
http_pid=$!

ready=0
for _ in $(seq 1 50); do
  if curl -fsS -o /dev/null "http://127.0.0.1:${port}/SHA256SUMS" 2>/dev/null; then
    ready=1
    break
  fi
  sleep 0.1
done
[ "$ready" -eq 1 ]

for image in ubuntu:22.04 ubuntu:24.04; do
  printf '=== %s ===\n' "$image"
  "${docker[@]}" pull "$image"
  reset_www
  "${docker[@]}" run --rm --network host \
    -v "$ROOT:/src:ro" \
    -v "$work:/work" \
    -e "XSTATS_RELEASE_URL=http://127.0.0.1:${port}" \
    -e "XSTATS_FILES_URL=http://127.0.0.1:${port}/deploy" \
    -e SRC=/src \
    -e WORK=/work \
    "$image" \
    bash /src/deploy/bootstrap_container_test.sh
done

printf 'bootstrap_test ok\n'
