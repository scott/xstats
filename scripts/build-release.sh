#!/usr/bin/env bash
# Build CGO-free linux release binaries and SHA256SUMS.
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

ldflags="-X main.version=${version} -X main.commit=${commit}"
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" \
    go build -trimpath -ldflags "$ldflags" -o "$outdir/xstats-linux-$arch" ./cmd/xstats
done

(
  cd "$outdir"
  sha256sum xstats-linux-amd64 xstats-linux-arm64 > SHA256SUMS
)
