# id: PF03
# name: Bundle artifact checksums
# category: artifact
# needs: PF01
check() {
  local n=0 bad=() file want got
  while IFS=$'\t' read -r file want; do
    [ -n "$file" ] || continue
    n=$((n + 1))
    if [ ! -f "$BUNDLE_DIR/$file" ]; then
      bad+=("$file missing")
      continue
    fi
    got=$(sha256 "$BUNDLE_DIR/$file")
    [ "$got" = "$want" ] || bad+=("$file sha256 ${got:0:12} != signed ${want:0:12}")
  done < <(jq -r '.schema.migrations[] | [.file, .sha256], (select(.down_file != null) | [.down_file, .down_sha256]) | @tsv' "$MANIFEST")
  if [ ${#bad[@]} -gt 0 ]; then
    printf '%s; ' "${bad[@]}"
    echo
    return 1
  fi
  echo "$n files match the signed checksums"
}
