# id: PF06
# name: Enough ready Docker nodes
# category: cluster
# needs: PF05
check() {
  local nodes ready names bad
  nodes=$(nomad_get /v1/nodes) || { echo "cannot list nodes"; return 1; }
  ready=$(echo "$nodes" | jq '[.[] | select(.Status == "ready" and .SchedulingEligibility == "eligible" and (.Drivers.docker.Healthy // false))] | length')
  names=$(echo "$nodes" | jq -r '[.[] | select(.Status == "ready" and .SchedulingEligibility == "eligible" and (.Drivers.docker.Healthy // false)) | .Name] | sort | join(",")')
  bad=$(echo "$nodes" | jq -r '[.[] | select(.Status != "ready" or .SchedulingEligibility != "eligible" or ((.Drivers.docker.Healthy // false) | not)) | "\(.Name)=\(.Status)/\(.SchedulingEligibility)"] | join(",")')
  if [ "$ready" -lt "$REQUIRED_NODES" ]; then
    echo "$ready of $REQUIRED_NODES required nodes ready+eligible with Docker [$names]; not usable: [$bad]"
    return 1
  fi
  echo "$ready nodes ready [$names]"
}
