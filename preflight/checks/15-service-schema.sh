# id: PF15
# name: Services accept target schema
# category: release
# needs: PF01
check() {
  local target bad
  target=$(jq -r .schema.target "$MANIFEST")
  bad=$(jq -r --argjson t "$target" '.services[] | select(.min_schema > $t or .max_schema < $t) | "\(.name) v\(.version) needs \(.min_schema)-\(.max_schema)"' "$MANIFEST" | paste -sd ',' -)
  if [ -n "$bad" ]; then
    echo "schema target $target outside supported range: $bad"
    return 1
  fi
  echo "every service accepts schema $target"
}
