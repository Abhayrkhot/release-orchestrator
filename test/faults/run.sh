#!/usr/bin/env bash
# shellcheck disable=SC2016 # jq filters use $vars passed with --arg
# Preflight fault suite. Runs on the ops box against the live cluster.
#
# For each fault: inject it, run a real `relctl deploy`, then require that
#   1. the deploy is blocked (exit 3) by the preflight check meant to catch it,
#   2. nothing changed: no Nomad job version, schema row or release record,
#   3. after the fault is removed, the same candidate release passes preflight.
#
# Usage: bash test/faults/run.sh [FAULT_ID ...]
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 2

WORK=test/faults/work
RESULTS=results/faults.jsonl
mkdir -p "$WORK" results
: >"$RESULTS"
: "${MYSQL_ROOT_PASSWORD:=rootpw}"
ONLY=" ${*:-} "
SERVICES=(catalog-api order-worker edge-gateway)

log() { printf '%s  %s\n' "$(date +%H:%M:%S)" "$*" >&2; }
die() {
  log "FATAL: $*"
  exit 2
}
root_sql() { MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql -h mysql -uroot -N -B -e "$1"; }

case "$(uname -m)" in
  aarch64 | arm64) ARCH=arm64 ;;
  *) ARCH=amd64 ;;
esac

status=$(relctl status --json) || die "relctl status failed"
CURRENT=$(jq -r .release <<<"$status")
SVC_VERSION=$(jq -r '.services[] | select(.name == "catalog-api") | .want_version' <<<"$status")
TARGET=$(jq -r .schema <<<"$status")
[ -n "$CURRENT" ] && [ "$CURRENT" != null ] || die "deploy a release first"
IFS=. read -r MA MI PA <<<"$CURRENT"
NEXT="$MA.$MI.$((PA + 1))"
log "running release $CURRENT (services $SVC_VERSION, schema $TARGET); candidate $NEXT"

# make_bundle DIR [manifest create args...]: a signed candidate release.
make_bundle() {
  local dir=$1
  shift
  relctl manifest create --clean --spec releases/spec.json --version "$NEXT" --service-version "$SVC_VERSION" \
    --arch "$ARCH" --images dist/images --migrations migrations --schema-target "$TARGET" --out "$dir" "$@" >/dev/null &&
    relctl manifest sign --bundle "$dir" --key keys/release.key >/dev/null
}

# resign DIR: sign an edited manifest with the trusted key.
resign() { relctl manifest sign --bundle "$1" --key keys/release.key >/dev/null; }

# edit DIR [jq args...] FILTER: rewrite release.json in place.
edit() {
  local dir=$1 tmp
  shift
  tmp=$(mktemp)
  jq "$@" "$dir/release.json" >"$tmp" && cat "$tmp" >"$dir/release.json" && rm -f "$tmp"
}

VALID=$WORK/valid
make_bundle "$VALID" || die "cannot build the valid candidate bundle"

# Fingerprint of everything a rollout could change.
state() {
  local s
  for s in "${SERVICES[@]}"; do
    printf '%s=%s ' "$s" "$(curl -fsS --max-time 3 "$NOMAD_ADDR/v1/job/$s" | jq -r .Version)"
  done
  printf 'schema=%s ' "$(root_sql "SELECT GROUP_CONCAT(CONCAT(version, ':', LEFT(checksum, 8), ':', dirty) ORDER BY version) FROM app.schema_migrations")"
  printf 'releases=%s' "$(root_sql "SELECT COUNT(*) FROM relctl.releases")"
}

# wait_clean: the valid candidate passes preflight again (retries while the
# cluster settles after a fault is removed).
wait_clean() {
  for _ in $(seq 1 60); do
    if relctl preflight --bundle "$VALID" >"$WORK/clean.log" 2>&1; then
      return 0
    fi
    sleep 2
  done
  log "cluster did not return to a clean preflight:"
  grep -E "FAIL|relctl" "$WORK/clean.log" >&2
  return 1
}

wait_for() { # wait_for SECONDS CMD...
  local n=$1
  shift
  for _ in $(seq 1 "$n"); do
    "$@" >/dev/null 2>&1 && return 0
    sleep 1
  done
  return 1
}
nomad_ready() {
  [ "$(curl -fsS "$NOMAD_ADDR/v1/nodes" | jq '[.[] | select(.Status == "ready" and .SchedulingEligibility == "eligible")] | length')" -ge 3 ]
}

PASSED=0
TOTAL=0
# run_fault ID EXPECTED_CHECK "description" BUNDLE INJECT CLEANUP
run_fault() {
  local id=$1 expect=$2 desc=$3 bundle=$4 inject=$5 cleanup=$6
  if [ "$ONLY" != "  " ] && [[ "$ONLY" != *" $id "* ]]; then
    return
  fi
  TOTAL=$((TOTAL + 1))
  log "$id  $desc"
  local before after out rc failed detail st start ms clean=true
  before=$(state)
  if ! $inject; then
    log "$id  injection failed"
    jq -nc --arg id "$id" --arg desc "$desc" --arg expect "$expect" \
      '{id: $id, fault: $desc, expected: $expect, caught: false, error: "injection failed"}' >>"$RESULTS"
    $cleanup
    return
  fi
  start=$(date +%s%3N)
  out=$(relctl deploy --bundle "$bundle" --json 2>"$WORK/$id.log")
  rc=$?
  ms=$(($(date +%s%3N) - start))
  $cleanup
  wait_clean || clean=false
  after=$(state)
  st=$(jq -r '.status // "none"' <<<"$out" 2>/dev/null)
  failed=$(jq -r '[.preflight.checks[]? | select(.status == "fail") | .id] | join(",")' <<<"$out" 2>/dev/null)
  detail=$(jq -r --arg c "$expect" '.preflight.checks[]? | select(.id == $c) | .detail' <<<"$out" 2>/dev/null)
  local caught=false
  if [ "$rc" = 3 ] && [ "$st" = blocked ] && [[ ",$failed," == *",$expect,"* ]] && [ "$before" = "$after" ] && $clean; then
    caught=true
    PASSED=$((PASSED + 1))
  fi
  log "$id  caught=$caught exit=$rc status=$st failed=[$failed] unchanged=$([ "$before" = "$after" ] && echo yes || echo NO) clean_after=$clean (${ms}ms)"
  log "     $expect: $detail"
  if [ "$before" != "$after" ]; then
    log "     before: $before"
    log "     after:  $after"
  fi
  jq -nc --arg id "$id" --arg desc "$desc" --arg expect "$expect" --arg failed "$failed" --arg detail "$detail" \
    --argjson caught "$caught" --argjson rc "$rc" --arg status "$st" --argjson ms "$ms" \
    --argjson unchanged "$([ "$before" = "$after" ] && echo true || echo false)" --argjson clean "$clean" \
    '{id: $id, fault: $desc, expected: $expect, caught: $caught, failed_checks: ($failed | split(",")), detail: $detail,
      exit_code: $rc, status: $status, cluster_unchanged: $unchanged, clean_after: $clean, ms: $ms}' >>"$RESULTS"
}

noop() { :; }

# ---- artifact faults ----------------------------------------------------

F01=$WORK/f01
f01_inject() { # change an image digest after signing
  make_bundle "$F01" || return 1
  local other
  other=$(jq -r '.services[] | select(.name == "edge-gateway") | .digest' "$F01/release.json")
  edit "$F01" --arg d "$other" '(.services[] | select(.name == "catalog-api") | .digest) |= $d'
}
run_fault F01 PF01 "Manifest edited after signing (catalog-api digest swapped)" "$F01" f01_inject noop

F02=$WORK/f02
f02_inject() {
  make_bundle "$F02" || return 1
  [ -f "$WORK/rogue.key" ] || relctl keygen --out "$WORK/rogue" >/dev/null || return 1
  relctl manifest sign --bundle "$F02" --key "$WORK/rogue.key" >/dev/null
}
run_fault F02 PF01 "Manifest signed by a key that is not trusted" "$F02" f02_inject noop

F03=$WORK/f03
f03_inject() {
  make_bundle "$F03" || return 1
  edit "$F03" '(.services[] | select(.name == "catalog-api") | .depends_on) = ["order-worker"]' && resign "$F03"
}
run_fault F03 PF02 "Service dependency cycle (catalog-api <-> order-worker)" "$F03" f03_inject noop

F04=$WORK/f04
f04_inject() {
  make_bundle "$F04" || return 1
  local f
  f=$(find "$F04/migrations" -name '*.up.sql' | sort | tail -1)
  echo "DROP TABLE orders; -- slipped in after signing" >>"$f"
}
run_fault F04 PF03 "Migration file modified after signing" "$F04" f04_inject noop

F05=$WORK/f05
f05_inject() { # an image built but never pushed
  make_bundle "$F05" || return 1
  relctl image build --binary "dist/build/$ARCH/catalog-api-$SVC_VERSION" --name catalog-api --version "$NEXT-unpushed" \
    --arch "$ARCH" --out "$WORK/images" >/dev/null || return 1
  local d
  d=$(jq -r .digest "$WORK/images/catalog-api-$NEXT-unpushed-$ARCH.json")
  edit "$F05" --arg d "$d" '(.services[] | select(.name == "catalog-api") | .digest) |= $d' && resign "$F05"
}
run_fault F05 PF04 "Image digest in manifest was never pushed" "$F05" f05_inject noop

# ---- release / schema compatibility faults ------------------------------

F09=$WORK/f09
f09_inject() { # 1 GiB more than the largest node has, whatever the hardware
  make_bundle "$F09" || return 1
  local biggest
  biggest=$(curl -fsS "$NOMAD_ADDR/v1/nodes?resources=true" | jq '[.[] | .NodeResources.Memory.MemoryMB] | max')
  [ "${biggest:-0}" -gt 0 ] || return 1
  edit "$F09" --argjson m "$((biggest + 1024))" '(.services[] | select(.name == "catalog-api") | .memory_mb) = $m' && resign "$F09"
}
run_fault F09 PF07 "Service asks for more memory than any node has" "$F09" f09_inject noop

F16=$WORK/f16
f16_inject() { # a destructive migration beyond what the running release accepts
  local migs
  migs=$(mktemp -d "$WORK/migrations-breaking.XXXXXX") # fresh, so no stale files from an older run
  chmod 755 "$migs" # readable outside the container, where go ./... walks the tree
  cp migrations/*.sql "$migs/"
  printf 'CREATE TABLE audit_log (id BIGINT PRIMARY KEY);\n' >"$migs/0005_create_audit_log.up.sql"
  printf -- '-- Renames a column the running services read.\nALTER TABLE items RENAME COLUMN name TO title;\n' \
    >"$migs/0006_rename_items_name.up.sql"
  jq '.services[].max_schema = 6' releases/spec.json >"$WORK/spec-breaking.json"
  relctl manifest create --clean --spec "$WORK/spec-breaking.json" --version "$NEXT" --service-version "$SVC_VERSION" \
    --arch "$ARCH" --images dist/images --migrations "$migs" --schema-target 6 --out "$F16" >/dev/null &&
    resign "$F16"
}
run_fault F16 PF14 "Breaking migration: running release could not be rolled back to" "$F16" f16_inject noop

F17=$WORK/f17
f17_inject() {
  make_bundle "$F17" || return 1
  edit "$F17" '(.services[] | select(.name == "catalog-api")) |= (.min_schema = 5 | .max_schema = 6)' && resign "$F17"
}
run_fault F17 PF15 "Service needs a schema this release does not migrate to" "$F17" f17_inject noop

F22=$WORK/f22
f22_inject() {
  relctl manifest create --clean --spec releases/spec.json --version 0.0.1 --service-version "$SVC_VERSION" \
    --arch "$ARCH" --images dist/images --migrations migrations --schema-target "$TARGET" --out "$F22" >/dev/null && resign "$F22"
}
run_fault F22 PF20 "Release older than the running one (accidental downgrade)" "$F22" f22_inject noop

# ---- database faults -----------------------------------------------------

f11_inject() { root_sql "REVOKE ALTER ON app.* FROM 'relctl'@'%'"; }
f11_cleanup() { root_sql "GRANT ALL PRIVILEGES ON app.* TO 'relctl'@'%'"; }
run_fault F11 PF09 "Migration user lost ALTER on the app schema" "$VALID" f11_inject f11_cleanup

f12_inject() { root_sql "UPDATE app.schema_migrations SET dirty = TRUE WHERE version = $TARGET"; }
f12_cleanup() { root_sql "UPDATE app.schema_migrations SET dirty = FALSE WHERE version = $TARGET"; }
run_fault F12 PF10 "Earlier migration failed part way (dirty flag set)" "$VALID" f12_inject f12_cleanup

F13_SUM=""
f13_inject() {
  F13_SUM=$(root_sql "SELECT checksum FROM app.schema_migrations WHERE version = 2")
  root_sql "UPDATE app.schema_migrations SET checksum = SHA2('edited after it was applied', 256) WHERE version = 2"
}
f13_cleanup() { root_sql "UPDATE app.schema_migrations SET checksum = '$F13_SUM' WHERE version = 2"; }
run_fault F13 PF11 "Applied migration differs from the one in the release" "$VALID" f13_inject f13_cleanup

f14_inject() { root_sql "INSERT INTO app.schema_migrations (version, name, checksum, dirty) VALUES ($((TARGET + 1)), 'from_a_newer_release', SHA2('x', 256), FALSE)"; }
f14_cleanup() { root_sql "DELETE FROM app.schema_migrations WHERE version = $((TARGET + 1))"; }
run_fault F14 PF12 "Database already migrated past this release's schema" "$VALID" f14_inject f14_cleanup

f15_inject() { root_sql "ALTER TABLE app.items ADD COLUMN hotfix_flag TINYINT NULL"; }
f15_cleanup() { root_sql "ALTER TABLE app.items DROP COLUMN hotfix_flag"; }
run_fault F15 PF13 "Column added by hand, outside any migration" "$VALID" f15_inject f15_cleanup

F18_PID=""
f18_inject() { # a session holding row locks in an open transaction
  MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql -h mysql -uroot -e \
    "START TRANSACTION; SELECT sku FROM app.items FOR UPDATE; SELECT SLEEP(600);" >/dev/null 2>&1 &
  F18_PID=$!
  sleep 12 # older than LONG_TRX_SECONDS (10s)
  [ -n "$(root_sql "SELECT trx_id FROM information_schema.innodb_trx")" ]
}
f18_cleanup() {
  local t
  for t in $(root_sql "SELECT trx_mysql_thread_id FROM information_schema.innodb_trx"); do root_sql "KILL $t" || true; done
  kill "$F18_PID" 2>/dev/null
  wait "$F18_PID" 2>/dev/null
  true
}
run_fault F18 PF16 "Long-running transaction holding locks a migration would wait on" "$VALID" f18_inject f18_cleanup

F21_ID=""
f21_inject() {
  F21_ID=$(root_sql "INSERT INTO relctl.releases (kind, version, manifest_sha256, manifest, signature, key_id, status, stage, started_at)
    SELECT 'deploy', '$NEXT', manifest_sha256, manifest, signature, key_id, 'in_progress', 'rollout:catalog-api', NOW(3)
    FROM relctl.releases WHERE status = 'succeeded' ORDER BY id DESC LIMIT 1; SELECT LAST_INSERT_ID();")
  [ -n "$F21_ID" ]
}
f21_cleanup() { root_sql "DELETE FROM relctl.releases WHERE id = $F21_ID"; }
run_fault F21 PF19 "Earlier release interrupted mid-rollout, never reconciled" "$VALID" f21_inject f21_cleanup

# ---- cluster faults -------------------------------------------------------

f08_inject() { nomad node eligibility -disable "$(nomad node status -json | jq -r '.[] | select(.Name == "node3" and .Status == "ready") | .ID')" >/dev/null; }
f08_cleanup() {
  nomad node eligibility -enable "$(nomad node status -json | jq -r '.[] | select(.Name == "node3" and .Status == "ready") | .ID')" >/dev/null
  wait_for 30 nomad_ready
}
run_fault F08 PF06 "A node is ineligible: only 2 of 3 required nodes" "$VALID" f08_inject f08_cleanup

F19_ADDR=""
f19_inject() { # one catalog-api instance starts failing its health check
  F19_ADDR=$(relctl status --json | jq -r '.services[] | select(.name == "catalog-api") | .endpoints[0].addr')
  curl -fsS -X POST "http://$F19_ADDR/admin/fault?mode=unhealthy" >/dev/null || return 1
  local alloc
  alloc=$(relctl status --json | jq -r '.services[] | select(.name == "catalog-api") | .endpoints[0].alloc')
  wait_for 15 bash -c "curl -fsS $NOMAD_ADDR/v1/client/allocation/$alloc/checks | jq -e '[.[] | select(.Status != \"success\")] | length > 0'"
}
f19_cleanup() { curl -fsS -X POST "http://$F19_ADDR/admin/fault?mode=healthy" >/dev/null; }
run_fault F19 PF17 "Running release degraded: a catalog-api instance is failing checks" "$VALID" f19_inject f19_cleanup

f20_inject() { nomad var purge nomad/jobs/edge-gateway >/dev/null; }
f20_cleanup() { relctl bootstrap --services edge-gateway >/dev/null; }
run_fault F20 PF18 "Database credentials variable for edge-gateway deleted" "$VALID" f20_inject f20_cleanup

f06_inject() { docker stop relorch-registry >/dev/null; }
f06_cleanup() {
  docker start relorch-registry >/dev/null
  wait_for 30 curl -fsS "$REGISTRY_URL/v2/"
}
REGISTRY_URL=${RELCTL_REGISTRY_URL:-http://registry:5000}
run_fault F06 PF04 "Registry unreachable" "$VALID" f06_inject f06_cleanup

f07_inject() { docker stop relorch-nomad-server >/dev/null; }
f07_cleanup() {
  docker start relorch-nomad-server >/dev/null
  wait_for 60 bash -c "curl -fsS $NOMAD_ADDR/v1/status/leader | jq -e 'length > 0'"
  wait_for 60 nomad_ready
}
run_fault F07 PF05 "Nomad server down" "$VALID" f07_inject f07_cleanup

f10_inject() { docker stop relorch-mysql >/dev/null; }
f10_cleanup() {
  docker start relorch-mysql >/dev/null
  wait_for 90 root_sql "SELECT 1"
}
run_fault F10 PF08 "MySQL down" "$VALID" f10_inject f10_cleanup

log "caught $PASSED of $TOTAL faults before rollout"
jq -s '{caught: (map(select(.caught)) | length), total: length, checks_exercised: ([.[].expected] | unique), faults: .}' "$RESULTS" >results/faults.json
[ "$PASSED" = "$TOTAL" ]
