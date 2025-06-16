# id: PF05
# name: Nomad has a leader
# category: cluster
check() {
  local leader
  leader=$(nomad_get /v1/status/leader 2>&1) || { echo "Nomad API $NOMAD_ADDR unreachable: $leader"; return 1; }
  leader=$(echo "$leader" | jq -r .)
  [ -n "$leader" ] || { echo "no leader elected"; return 1; }
  echo "leader $leader"
}
