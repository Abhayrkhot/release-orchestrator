#!/usr/bin/env bash
# Reproducible build: relctl and every service version for linux/ARCH, plus
# the OCI image digest of each service, summarized in OUT/SHA256SUMS.
# Two machines with the same Go toolchain and source must produce the same
# file; CI and scripts/repro-nodes.sh compare them.
#
# Usage: scripts/build.sh ARCH OUT
set -euo pipefail
arch=${1:?usage: build.sh ARCH OUT}
out=${2:?usage: build.sh ARCH OUT}
: "${SOURCE_DATE_EPOCH:=0}"
export SOURCE_DATE_EPOCH CGO_ENABLED=0 GOFLAGS=-mod=readonly

services="catalog-api order-worker edge-gateway"
versions="1.0.0 1.1.0"
flags=(-trimpath -buildvcs=false)
ld="-s -w -buildid="

mkdir -p "$out/bin" "$out/images"
: >"$out/digests"
GOOS=linux GOARCH="$arch" go build "${flags[@]}" -ldflags "$ld" -o "$out/bin/relctl" ./cmd/relctl
# A relctl for this machine, to assemble the images.
go build "${flags[@]}" -ldflags "$ld" -o "$out/relctl-build" ./cmd/relctl
for v in $versions; do
  for s in $services; do
    GOOS=linux GOARCH="$arch" go build "${flags[@]}" -ldflags "$ld -X main.version=$v" -o "$out/bin/$s-$v" "./cmd/$s"
    "$out/relctl-build" image build --binary "$out/bin/$s-$v" --name "$s" --version "$v" --arch "$arch" --out "$out/images" |
      awk -v n="image:$s:$v" '{ sub(/^sha256:/, "", $3); print $3 "  " n }' >>"$out/digests"
  done
done
(cd "$out" && sha256sum bin/* && cat digests) | LC_ALL=C sort -k2 >"$out/SHA256SUMS"
echo "$(wc -l <"$out/SHA256SUMS" | tr -d ' ') artifacts -> $out/SHA256SUMS ($(go version | cut -d' ' -f3), $(uname -sm))"
