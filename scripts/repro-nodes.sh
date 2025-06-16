#!/usr/bin/env bash
# Build the release on each of the cluster's three Linux nodes (and on this
# machine) with the official Go toolchain, then require every SHA256SUMS to be
# identical. Writes results/reproducibility.json.
set -euo pipefail
cd "$(dirname "$0")/.."

GO_VERSION=$(sed -n 's/^go //p' go.mod)
arch=$(go env GOARCH)
# Published at https://go.dev/dl/?mode=json for this toolchain version.
declare -A GO_SHA256=(
  [go1.27.3.linux-amd64.tar.gz]=65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40
  [go1.27.3.linux-arm64.tar.gz]=a06e99309dbcbdca1a8ccd2282c3ee146e55930886e26b4f1193269587165f49
)
tarball="go$GO_VERSION.linux-$arch.tar.gz"
want=${GO_SHA256[$tarball]:?no pinned checksum for $tarball}
epoch=${SOURCE_DATE_EPOCH:-$(git log -1 --format=%ct 2>/dev/null || echo 0)}

mkdir -p dist/toolchain dist/repro results
if [ ! -f "dist/toolchain/$tarball" ]; then
  curl -fsSL "https://go.dev/dl/$tarball" -o "dist/toolchain/$tarball.part"
  mv "dist/toolchain/$tarball.part" "dist/toolchain/$tarball"
fi
got=$(shasum -a 256 "dist/toolchain/$tarball" | cut -d' ' -f1)
[ "$got" = "$want" ] || { echo "toolchain checksum mismatch: $got" >&2; exit 1; }

# The source, as the nodes will see it (no build outputs, keys or bundles).
COPYFILE_DISABLE=1 tar -czf dist/repro/src.tgz --exclude ./dist --exclude ./keys --exclude ./releases/out \
  --exclude ./.git --exclude ./test/faults/work --exclude ./results .

echo "host: $(uname -sm)"
SOURCE_DATE_EPOCH=$epoch bash scripts/build.sh "$arch" dist/repro/host >/dev/null
cp dist/repro/host/SHA256SUMS dist/repro/SHA256SUMS.host

# Each node keeps the toolchain and module cache under $base between runs;
# the source and outputs go to a fresh directory every time.
base=/var/tmp/relorch-repro
for n in node1 node2 node3; do
  c=relorch-$n
  docker exec "$c" mkdir -p "$base"
  if ! docker exec "$c" test -x "$base/go/bin/go"; then
    docker cp -q "dist/toolchain/$tarball" "$c:$base/go.tgz"
    docker exec "$c" tar -C "$base" -xzf "$base/go.tgz"
  fi
  dir=$(docker exec "$c" mktemp -d "$base/run.XXXXXX")
  docker cp -q dist/repro/src.tgz "$c:$dir/src.tgz"
  docker exec "$c" bash -c "
    set -e
    mkdir -p $dir/src && tar -C $dir/src -xzf $dir/src.tgz 2>/dev/null
    cd $dir/src
    PATH=$base/go/bin:\$PATH GOPATH=$base/gopath GOCACHE=$base/cache GOFLAGS=-modcacherw SOURCE_DATE_EPOCH=$epoch \
      bash scripts/build.sh $arch $dir/out
    echo \"$n: \$(uname -sm), \$(. /etc/os-release && echo \$PRETTY_NAME)\"
  " | tail -2
  docker cp -q "$c:$dir/out/SHA256SUMS" "dist/repro/SHA256SUMS.$n"
done

ref=dist/repro/SHA256SUMS.node1
ok=true
for f in dist/repro/SHA256SUMS.*; do
  if ! cmp -s "$ref" "$f"; then
    ok=false
    echo "DIFFERS: $f" >&2
    diff "$ref" "$f" >&2 || true
  fi
done
# The images already in the registry were built on the host by `make images`;
# they must match too.
pushed=$(for f in dist/images/*-"$arch".json; do jq -r '"\(.digest | ltrimstr("sha256:"))  image:\(.name):\(.version)"' "$f"; done | LC_ALL=C sort -k2)
if [ "$pushed" != "$(grep ' image:' "$ref")" ]; then
  ok=false
  echo "DIFFERS: image digests in the registry vs node builds" >&2
fi

jq -n --arg arch "$arch" --arg go "$GO_VERSION" --argjson epoch "$epoch" --argjson identical "$ok" \
  --rawfile sums "$ref" \
  '{arch: $arch, go: $go, source_date_epoch: $epoch, builders: ["node1", "node2", "node3", "host"],
    identical: $identical, artifacts: ($sums | split("\n") | map(select(length > 0) | split("  ") | {sha256: .[0], name: .[1]}))}' \
  >results/reproducibility.json
if $ok; then
  echo "identical: $(grep -c . "$ref") artifacts on node1, node2, node3 and the host"
else
  exit 1
fi
