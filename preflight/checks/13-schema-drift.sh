# id: PF13
# name: No out-of-band schema change
# category: database
# needs: PF08 PF10
# Compares the live schema with the fingerprint taken after the last migration.
check() {
  table_exists "$STATE_DB" schema_snapshots || { echo "no snapshot yet"; return 0; }
  local want got sv
  read -r sv want < <(mysql_q "$STATE_DB" "SELECT schema_version, fingerprint FROM schema_snapshots ORDER BY id DESC LIMIT 1")
  [ -n "${want:-}" ] || { echo "no snapshot yet"; return 0; }
  got=$(mysql_q "$APP_DB" "SET SESSION group_concat_max_len = 1048576; $(cat "$PREFLIGHT_DIR/sql/schema_fingerprint.sql")")
  if [ "$got" != "$want" ]; then
    echo "live schema fingerprint ${got:0:12} differs from ${want:0:12} recorded after migration $sv: someone changed the schema outside a release"
    return 1
  fi
  echo "schema matches snapshot ${want:0:12} (v$sv)"
}
