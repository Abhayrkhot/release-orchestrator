# id: PF10
# name: No dirty migration
# category: database
# needs: PF08
check() {
  table_exists "$APP_DB" schema_migrations || { echo "fresh database"; return 0; }
  local dirty
  dirty=$(mysql_q "$APP_DB" "SELECT CONCAT(version, ' ', name) FROM schema_migrations WHERE dirty")
  if [ -n "$dirty" ]; then
    echo "migration $dirty failed part way and left the schema dirty; repair it before deploying"
    return 1
  fi
  echo "no dirty migrations"
}
