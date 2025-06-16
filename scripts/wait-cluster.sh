#!/usr/bin/env bash
# Wait until MySQL answers and three Nomad nodes are ready with Docker.
set -euo pipefail
want=${REQUIRED_NODES:-3}
ready=0
for _ in $(seq 1 120); do
  ready=$(curl -fsS http://127.0.0.1:4646/v1/nodes 2>/dev/null |
    jq '[.[] | select(.Status == "ready" and (.Drivers.docker.Healthy // false))] | length' 2>/dev/null || echo 0)
  if [ "$ready" -ge "$want" ] && docker exec relorch-mysql mysqladmin ping -uroot -prootpw --silent >/dev/null 2>&1; then
    echo "cluster ready: $ready nodes"
    exit 0
  fi
  sleep 1
done
echo "cluster not ready after 120s (nodes ready: $ready)" >&2
exit 1
