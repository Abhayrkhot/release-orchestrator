#!/usr/bin/env bash
# Time N automatic rollbacks. Each run deploys a release whose catalog-api
# fails (MODE=crash_on_start by default, or unhealthy), lets the controller
# roll it back, and records the controller's own timings:
#   rollback_seconds  failure detected -> previous release healthy everywhere
#   recovery_seconds  bad rollout began -> previous release healthy everywhere
# Runs on the ops box. Writes results/rollback-bench-<mode>.json.
set -euo pipefail
cd "$(dirname "$0")/.."

n=${1:-10}
mode=${MODE:-crash_on_start}
case "$(uname -m)" in aarch64 | arm64) arch=arm64 ;; *) arch=amd64 ;; esac
work=test/faults/work/bench
mkdir -p "$work" results
runs=$work/runs-$mode.jsonl
: >"$runs"

for i in $(seq 1 "$n"); do
  status=$(relctl status --json)
  cur=$(jq -r .release <<<"$status")
  schema=$(jq -r .schema <<<"$status")
  IFS=. read -r ma mi pa <<<"$cur"
  next="$ma.$mi.$((pa + 1))"
  relctl manifest create --clean --spec releases/spec.json --version "$next" --service-version 1.1.0 --arch "$arch" \
    --images dist/images --migrations migrations --schema-target "$schema" --out "$work/bundle" \
    --env "catalog-api:FAULT_MODE=$mode" --env "catalog-api:BENCH_RUN=$i" >/dev/null
  relctl manifest sign --bundle "$work/bundle" --key keys/release.key >/dev/null
  res=$(relctl deploy --bundle "$work/bundle" --json 2>/dev/null || true)
  jq -c --argjson run "$i" '{run: $run, status, reason, rollback_seconds, recovery_seconds}' <<<"$res" | tee -a "$runs"
  if [ "$(jq -r .status <<<"$res")" != rolled_back ]; then
    echo "run $i did not roll back" >&2
    exit 1
  fi
done

jq -s --arg mode "$mode" '
  def stats(f): (map(f) | sort) as $s | ($s | length) as $n |
    {n: $n, min: $s[0], p50: $s[($n - 1) * 0.5 | round], p95: $s[($n - 1) * 0.95 | round], max: $s[-1]}
    | with_entries(.value |= (if type == "number" then (. * 10 | round / 10) else . end));
  {mode: $mode, runs: ., rollback_seconds: stats(.rollback_seconds), recovery_seconds: stats(.recovery_seconds)}
' "$runs" >"results/rollback-bench-$mode.json"
jq '{mode, rollback_seconds, recovery_seconds}' "results/rollback-bench-$mode.json"
