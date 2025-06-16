# id: PF11
# name: Applied migrations match release
# category: database
# needs: PF01 PF08
check() {
  table_exists "$APP_DB" schema_migrations || { echo "fresh database"; return 0; }
  local version checksum want bad=() n=0
  while IFS=$'\t' read -r version checksum; do
    [ -n "$version" ] || continue
    want=$(jq -r --argjson v "$version" '.schema.migrations[] | select(.version == $v) | .sha256' "$MANIFEST")
    [ -n "$want" ] || continue # newer than this release; PF12 reports that
    n=$((n + 1))
    [ "$want" = "$checksum" ] || bad+=("migration $version applied as ${checksum:0:12}, release has ${want:0:12}")
  done < <(mysql_q "$APP_DB" "SELECT version, checksum FROM schema_migrations ORDER BY version")
  if [ ${#bad[@]} -gt 0 ]; then
    printf '%s; ' "${bad[@]}"
    echo
    return 1
  fi
  echo "$n applied migrations match"
}
