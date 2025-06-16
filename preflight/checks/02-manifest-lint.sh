# id: PF02
# name: Manifest is well-formed
# category: artifact
# needs: PF01
# Fields, versions, digests, contiguous migrations, dependency graph.
check() {
  local out
  if out=$("$RELCTL" manifest lint "$MANIFEST" 2>&1); then
    echo "$(jq '.services | length' "$MANIFEST") services, order: $("$RELCTL" manifest order "$MANIFEST" | paste -sd ' ' -)"
    return 0
  fi
  echo "$out" | tr '\n' ';'
  return 1
}
