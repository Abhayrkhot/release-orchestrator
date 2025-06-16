# id: PF04
# name: Image digests resolvable
# category: artifact
# needs: PF01
check() {
  if ! curl -fsS --max-time "$HTTP_TIMEOUT" -o /dev/null "$REGISTRY_URL/v2/"; then
    echo "registry $REGISTRY_URL is not reachable"
    return 1
  fi
  local s image digest repo code bad=() n=0
  for s in $(services); do
    image=$(svc_field "$MANIFEST" "$s" .image)
    digest=$(svc_field "$MANIFEST" "$s" .digest)
    repo=${image#*/}
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time "$HTTP_TIMEOUT" -I \
      -H 'Accept: application/vnd.oci.image.manifest.v1+json' "$REGISTRY_URL/v2/$repo/manifests/$digest")
    n=$((n + 1))
    [ "$code" = 200 ] || bad+=("$s $repo@${digest:7:12} -> HTTP $code")
  done
  if [ ${#bad[@]} -gt 0 ]; then
    printf 'not in registry: %s; ' "${bad[@]}"
    echo
    return 1
  fi
  echo "$n images resolve by digest"
}
