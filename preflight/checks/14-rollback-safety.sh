# id: PF14
# name: Rollback stays possible
# category: release
# needs: PF01
# Migrations are not rolled back, so the running release must accept the new schema.
check() {
  if [ "$CURRENT_UNKNOWN" = 1 ]; then
    echo "running release unknown: state database unreachable"
    return 2
  fi
  [ -n "$CURRENT_MANIFEST" ] || { echo "first release, nothing to roll back to"; return 0; }
  local target bad
  target=$(jq -r .schema.target "$MANIFEST")
  bad=$(jq -r --argjson t "$target" '.services[] | select(.max_schema < $t) | "\(.name) v\(.version) reads schema <= \(.max_schema)"' "$CURRENT_MANIFEST" | paste -sd ',' -)
  if [ -n "$bad" ]; then
    echo "release migrates to schema $target but the running $bad: a rollback would leave it broken"
    return 1
  fi
  echo "running release $CURRENT_VERSION accepts schema $target"
}
