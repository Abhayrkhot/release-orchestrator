#!/usr/bin/env bash
# Print Markdown summaries of the JSON files in results/, for the README.
set -euo pipefail
cd "$(dirname "$0")/.."

if [ -f results/faults.json ]; then
  jq -r '
    "### Preflight fault suite: \(.caught) of \(.total) faults caught before rollout\n",
    "Each fault is injected for real, then `relctl deploy` is run. A fault counts as caught only if the deploy is blocked (exit 3) by the expected check, no Nomad job version, schema row or release record changed, and the same release passes preflight again once the fault is removed. `\(.checks_exercised | length)` distinct checks were exercised.\n",
    "| Fault | Injected | Caught by | What the check reported |",
    "|---|---|---|---|",
    (.faults[] | "| \(.id) | \(.fault) | \(if .caught then "✅ " else "❌ " end)\(.failed_checks | join(", ")) | \(.detail | gsub("\\|"; "/") | .[0:150]) |")
  ' results/faults.json
  echo
fi

if [ -f results/recovery.json ]; then
  jq -r '
    "### Recovery scenarios: \(.passed) of \(.total) passed\n",
    "Each scenario breaks something during an upgrade and passes only if the cluster ends on one consistent release: every service healthy, the deploy lock free, no release left in progress, and every allocation on every node running the exact binary its signed manifest names.\n",
    "Automatic rollbacks: previous release healthy \(.rollback_seconds.p50)s (median) / \(.rollback_seconds.max)s (max) after the failure was detected, and \(.recovery_seconds.p50)s / \(.recovery_seconds.max)s after the bad rollout began (n=\(.recovery_seconds.n)).\n",
    "| # | Scenario | Result | Outcome | Recovery | Notes |",
    "|---|---|---|---|---|---|",
    (.scenarios[] | "| \(.id) | \(.name) | \(if .passed then "✅" else "❌" end) | \(.outcome) | \(if .recovery_seconds then "\(.recovery_seconds)s" elif .rollback_seconds then "\(.rollback_seconds)s" else "" end) | \((.notes // [])[0:2] | join("; ") | gsub("\\|"; "/") | .[0:160]) |")
  ' results/recovery.json
  echo
fi

for f in results/rollback-bench-*.json; do
  [ -f "$f" ] || continue
  jq -r '"### Rollback benchmark (\(.mode), n=\(.rollback_seconds.n))\n",
    "| | min | p50 | p95 | max |", "|---|---|---|---|---|",
    "| failure detected → previous release healthy | \(.rollback_seconds.min)s | \(.rollback_seconds.p50)s | \(.rollback_seconds.p95)s | \(.rollback_seconds.max)s |",
    "| bad rollout began → previous release healthy | \(.recovery_seconds.min)s | \(.recovery_seconds.p50)s | \(.recovery_seconds.p95)s | \(.recovery_seconds.max)s |", ""' "$f"
done

if [ -f results/reproducibility.json ]; then
  jq -r '"### Reproducible build: \(if .identical then "identical" else "DIFFERENT" end) on \(.builders | join(", "))\n",
    "Go \(.go), linux/\(.arch), SOURCE_DATE_EPOCH=\(.source_date_epoch). \(.artifacts | length) artifacts: relctl, two versions of each service binary, and the OCI image digest of each.\n",
    "| sha256 | artifact |", "|---|---|", (.artifacts[] | "| `\(.sha256[0:16])…` | \(.name) |")' results/reproducibility.json
fi
