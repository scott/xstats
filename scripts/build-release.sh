#!/usr/bin/env bash
# Build CGO-free linux release binaries, stage deploy files, and write SHA256SUMS.
# Usage: scripts/build-release.sh <version> <commit> <outdir>
set -euo pipefail
cd "$(dirname "$0")/.."

if [ $# -ne 3 ] || [ -z "$1" ] || [ -z "$2" ] || [ -z "$3" ]; then
  echo "usage: scripts/build-release.sh <version> <commit> <outdir>" >&2
  exit 2
fi

version=$1
commit=$2
outdir=$3
mkdir -p "$outdir"

# Flat names so a release URL can serve them next to the binaries.
# bootstrap.sh is added by a later change; the other three are required.
copy_asset() {
  local src=$1 name=$2 required=$3
  if [ -f "$src" ]; then
    cp -p "$src" "$outdir/$name"
  elif [ "$required" = required ]; then
    echo "build-release: missing $src" >&2
    exit 1
  fi
}

copy_asset deploy/xstats.service xstats.service required
copy_asset deploy/Caddyfile.tmpl Caddyfile.tmpl required
copy_asset deploy/set-auth.sh set-auth.sh required
copy_asset deploy/bootstrap.sh bootstrap.sh optional
if [ ! -f deploy/bootstrap.sh ]; then
  rm -f "$outdir/bootstrap.sh"
fi

ldflags="-X main.version=${version} -X main.commit=${commit}"
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" \
    go build -trimpath -ldflags "$ldflags" -o "$outdir/xstats-linux-$arch" ./cmd/xstats
done

assets=(
  xstats-linux-amd64
  xstats-linux-arm64
  xstats.service
  Caddyfile.tmpl
  set-auth.sh
)
if [ -f "$outdir/bootstrap.sh" ]; then
  assets+=(bootstrap.sh)
fi
(
  cd "$outdir"
  sha256sum "${assets[@]}" > SHA256SUMS
)
