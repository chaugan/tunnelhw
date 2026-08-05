#!/usr/bin/env bash
# Cross-compile TunnelHW agent + relay for all supported platforms into dist/.
# Usage: scripts/build.sh [version]
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION="${1:-dev}"
mkdir -p dist

build() {
  local goos="$1" goarch="$2" bin="$3" out="$4"
  echo "  ${goos}/${goarch}  ${out}"
  # Windows binaries deliberately keep their symbol tables: stripped (-s -w),
  # unsigned, metadata-less executables are a classic Defender/SmartScreen
  # false-positive trigger. Version resources are embedded via go-winres.
  local ldflags="-s -w -X main.version=${VERSION}"
  if [ "$goos" = "windows" ]; then
    ldflags="-X main.version=${VERSION}"
  fi
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
    go build -trimpath -ldflags "$ldflags" \
    -o "dist/${out}" "./cmd/${bin}"
}

# Embed Windows version-info + manifest resources (.syso, linked into
# windows/* builds automatically). Needs go-winres on PATH; skipped if absent.
if command -v go-winres >/dev/null 2>&1; then
  for bin in agent relay; do
    go-winres make --in winres/winres.json \
      --product-version "${VERSION}" --file-version "${VERSION}" \
      --out "cmd/${bin}/rsrc"
  done
else
  echo "warning: go-winres not found — windows exes will lack version resources" >&2
fi

echo "building TunnelHW ${VERSION}"
# Agent — runs on the machine with the hardware.
build windows amd64 agent tunnelhw-agent-windows-amd64.exe
build windows arm64 agent tunnelhw-agent-windows-arm64.exe
build linux   amd64 agent tunnelhw-agent-linux-amd64
build linux   arm64 agent tunnelhw-agent-linux-arm64
build darwin  amd64 agent tunnelhw-agent-darwin-amd64
build darwin  arm64 agent tunnelhw-agent-darwin-arm64
# Relay — runs on the server next to the LLM.
build windows amd64 relay tunnelhw-relay-windows-amd64.exe
build linux   amd64 relay tunnelhw-relay-linux-amd64
build linux   arm64 relay tunnelhw-relay-linux-arm64
build darwin  amd64 relay tunnelhw-relay-darwin-amd64
build darwin  arm64 relay tunnelhw-relay-darwin-arm64
echo "done — binaries in dist/"
