# id: PF07
# name: Nomad can place every service
# category: cluster
# needs: PF02 PF05 PF06
# Dry-run plan of each job as it would be submitted.
check() {
  local s job plan failed bad=()
  for s in $(services); do
    job=$("$RELCTL" render --manifest "$MANIFEST" --service "$s") || { echo "render $s failed"; return 1; }
    plan=$(curl -fsS --max-time 10 -X POST -d "{\"Job\": $job, \"Diff\": false}" "$NOMAD_ADDR/v1/job/$s/plan") || {
      echo "plan request for $s failed"
      return 1
    }
    failed=$(echo "$plan" | jq -r '(.FailedTGAllocs // {}) | to_entries[] | "\(.key): \(.value.CoalescedFailures + 1) unplaceable (\((.value.DimensionExhausted // {}) | to_entries | map("\(.key)=\(.value)") | join(",")))"')
    [ -z "$failed" ] || bad+=("$s $failed")
  done
  if [ ${#bad[@]} -gt 0 ]; then
    printf '%s; ' "${bad[@]}"
    echo
    return 1
  fi
  echo "all $(services | wc -l | tr -d ' ') jobs placeable"
}
