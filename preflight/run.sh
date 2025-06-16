#!/usr/bin/env bash
# Runs every check in checks/ in order and exits 1 if any fails.
#
# Each check file declares:
#   # id: PFnn
#   # name: what it checks
#   # category: artifact | cluster | database | release
#   # needs: PFxx PFyy      (optional; skipped if any of those failed)
# and a check() function that prints one line of detail and returns
# 0 (pass), 1 (fail) or 2 (skip).
#
# Results go to stdout as a table and, if PREFLIGHT_REPORT is set, to that
# file as JSON lines.
set -uo pipefail

DIR="${PREFLIGHT_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)}"
export PREFLIGHT_DIR="$DIR"
REPORT="${PREFLIGHT_REPORT:-/dev/null}"
CHECK_TIMEOUT="${CHECK_TIMEOUT:-30}"
ONLY="${PREFLIGHT_ONLY:-}"

for tool in jq curl mysql openssl sha256sum timeout; do
  command -v "$tool" >/dev/null || { echo "preflight: missing required tool: $tool" >&2; exit 3; }
done
[ -n "${MANIFEST:-}" ] || { echo "preflight: MANIFEST is not set" >&2; exit 3; }

declare -A RESULT
failed=0
now_ms() { date +%s%3N; }

printf '%-5s %-5s %-34s %s\n' ID RESULT CHECK DETAIL
for f in "$DIR"/checks/*.sh; do
  id=$(sed -n 's/^# id: *//p' "$f")
  name=$(sed -n 's/^# name: *//p' "$f")
  category=$(sed -n 's/^# category: *//p' "$f")
  needs=$(sed -n 's/^# needs: *//p' "$f")
  if [ -n "$ONLY" ] && [[ " $ONLY " != *" $id "* ]]; then
    continue
  fi

  start=$(now_ms)
  status="" detail=""
  for n in $needs; do
    if [ "${RESULT[$n]:-}" = fail ] || [ "${RESULT[$n]:-}" = skip ]; then
      status=skip detail="skipped: $n did not pass"
      break
    fi
  done
  if [ -z "$status" ]; then
    # shellcheck disable=SC2016 # $1 and $2 are expanded by the inner bash
    out=$(timeout "$CHECK_TIMEOUT" bash -c 'set -uo pipefail; source "$1"; source "$2"; check' _ "$DIR/lib.sh" "$f" 2>&1)
    rc=$?
    detail=$(printf '%s' "$out" | grep -v '^[[:space:]]*$' | tail -n 4 | paste -sd ' ' -)
    case $rc in
      0) status=pass ;;
      2) status=skip ;;
      124) status=fail detail="timed out after ${CHECK_TIMEOUT}s; $detail" ;;
      *) status=fail ;;
    esac
  fi
  RESULT[$id]=$status
  [ "$status" = fail ] && failed=1
  ms=$(( $(now_ms) - start ))
  printf '%-5s %-5s %-34s %s\n' "$id" "${status^^}" "$name" "$detail"
  jq -nc --arg id "$id" --arg name "$name" --arg status "$status" --arg detail "$detail" \
    --arg category "$category" --argjson ms "$ms" \
    '{id: $id, name: $name, status: $status, detail: $detail, category: $category, ms: $ms}' >>"$REPORT"
done
exit "$failed"
