# id: PF18
# name: Service secrets present
# category: cluster
# needs: PF05
# Each service reads its database DSN from the Nomad Variable nomad/jobs/<service>.
check() {
  local s v missing=()
  for s in $(services); do
    v=$(nomad_get "/v1/var/nomad/jobs/$s" 2>/dev/null | jq -r '.Items.db_dsn // empty')
    [ -n "$v" ] || missing+=("nomad/jobs/$s")
  done
  if [ ${#missing[@]} -gt 0 ]; then
    echo "missing db_dsn in ${missing[*]}: tasks would block waiting for the template"
    return 1
  fi
  echo "db_dsn present for $(services | wc -l | tr -d ' ') services"
}
