# id: PF17
# name: Running release healthy
# category: cluster
# needs: PF05
# Do not start a rollout on top of a degraded baseline: a rollback would have
# nowhere healthy to go. One failed check sample is noise, so the baseline is
# sampled up to BASELINE_SAMPLES times, 1s apart, and must be healthy once.
check() {
  if [ "$CURRENT_UNKNOWN" = 1 ]; then
    echo "running release unknown: state database unreachable"
    return 2
  fi
  [ -n "$CURRENT_MANIFEST" ] || { echo "nothing deployed yet"; return 0; }
  local samples=${BASELINE_SAMPLES:-3} i
  for i in $(seq 1 "$samples"); do
    if baseline_sample; then
      echo "$BASELINE_N allocations of $CURRENT_VERSION passing checks"
      return 0
    fi
    [ "$i" -lt "$samples" ] && sleep 1
  done
  printf 'degraded in %s samples: %s; ' "$samples" "${BASELINE_BAD[@]}"
  echo
  return 1
}

# baseline_sample sets BASELINE_BAD and BASELINE_N; returns 0 if healthy.
baseline_sample() {
  local s want allocs running a checks
  BASELINE_BAD=()
  BASELINE_N=0
  for s in $(current_services); do
    want=$(svc_field "$CURRENT_MANIFEST" "$s" .count)
    allocs=$(nomad_get "/v1/job/$s/allocations") || {
      BASELINE_BAD+=("$s: job not found")
      continue
    }
    running=$(echo "$allocs" | jq -r '.[] | select(.DesiredStatus == "run" and .ClientStatus == "running") | .ID')
    if [ "$(echo "$running" | grep -c .)" -lt "$want" ]; then
      BASELINE_BAD+=("$s: $(echo "$running" | grep -c .)/$want running")
    fi
    for a in $running; do
      checks=$(nomad_get "/v1/client/allocation/$a/checks") || {
        BASELINE_BAD+=("$s ${a:0:8}: no check status")
        continue
      }
      BASELINE_N=$((BASELINE_N + 1))
      if [ "$(echo "$checks" | jq '[.[] | select(.Status != "success")] | length')" != 0 ] || [ "$(echo "$checks" | jq length)" = 0 ]; then
        BASELINE_BAD+=("$s ${a:0:8} on $(echo "$allocs" | jq -r --arg a "$a" '.[] | select(.ID == $a) | .NodeName'): $(echo "$checks" | jq -r '[.[] | .Status] | join(",")')")
      fi
    done
  done
  [ ${#BASELINE_BAD[@]} -eq 0 ]
}
