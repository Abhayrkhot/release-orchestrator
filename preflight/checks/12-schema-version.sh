# id: PF12
# name: Schema not ahead of release
# category: database
# needs: PF01 PF08
check() {
  local target current count
  target=$(jq -r .schema.target "$MANIFEST")
  if ! table_exists "$APP_DB" schema_migrations; then
    echo "fresh database -> $target"
    return 0
  fi
  current=$(mysql_q "$APP_DB" "SELECT IFNULL(MAX(version), 0) FROM schema_migrations")
  count=$(mysql_q "$APP_DB" "SELECT COUNT(*) FROM schema_migrations")
  if [ "$count" != "$current" ]; then
    echo "applied migrations are not contiguous: $count rows, max version $current"
    return 1
  fi
  if [ "$current" -gt "$target" ]; then
    echo "database is at schema $current but this release targets $target"
    return 1
  fi
  echo "schema $current -> $target"
}
