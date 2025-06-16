#!/bin/sh
# Start this node's Docker daemon, then the Nomad client.
set -eu
: "${NODE_NAME:?NODE_NAME must be set}"

rm -f /var/run/docker.pid
dockerd-entrypoint.sh dockerd \
  --host unix:///var/run/docker.sock \
  --insecure-registry registry:5000 \
  >/var/log/dockerd.log 2>&1 &

i=0
until docker info >/dev/null 2>&1; do
  i=$((i + 1))
  if [ "$i" -gt 120 ]; then
    echo "dockerd did not start:" >&2
    cat /var/log/dockerd.log >&2
    exit 1
  fi
  sleep 0.5
done
echo "dockerd ready on $NODE_NAME"
exec nomad agent -config /etc/nomad.d -node "$NODE_NAME"
