# id: PF20
# name: Release is newer than running
# category: release
# needs: PF01
check() {
  if [ "$CURRENT_UNKNOWN" = 1 ]; then
    echo "running release unknown: state database unreachable"
    return 2
  fi
  local v
  v=$(jq -r .version "$MANIFEST")
  [ -n "$CURRENT_VERSION" ] || { echo "first release $v"; return 0; }
  if [ "$v" = "$CURRENT_VERSION" ]; then
    echo "$v is already the running release"
    return 1
  fi
  if [ "$(printf '%s\n%s\n' "$v" "$CURRENT_VERSION" | sort -V | tail -1)" != "$v" ]; then
    if [ "$ALLOW_DOWNGRADE" = 1 ]; then
      echo "downgrade $CURRENT_VERSION -> $v allowed by flag"
      return 0
    fi
    echo "$v is older than running $CURRENT_VERSION; use relctl rollback, or --allow-downgrade"
    return 1
  fi
  echo "$CURRENT_VERSION -> $v"
}
