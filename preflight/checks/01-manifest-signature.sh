# id: PF01
# name: Manifest signature
# category: artifact
# Verified with openssl, independently of relctl's own Go verification.
check() {
  [ -f "$MANIFEST" ] || { echo "manifest not found: $MANIFEST"; return 1; }
  [ -f "${SIGNATURE:-}" ] || { echo "signature not found: ${SIGNATURE:-<unset>}"; return 1; }
  local alg keyid want got pub sig
  alg=$(jq -r .algorithm "$SIGNATURE") || { echo "signature file is not JSON"; return 1; }
  keyid=$(jq -r .key_id "$SIGNATURE")
  want=$(jq -r .manifest_sha256 "$SIGNATURE")
  [ "$alg" = ed25519 ] || { echo "unsupported algorithm $alg"; return 1; }
  [[ "$keyid" =~ ^[0-9a-f]{16}$ ]] || { echo "bad key id $keyid"; return 1; }
  got=$(sha256 "$MANIFEST")
  if [ "$got" != "$want" ]; then
    echo "manifest changed after signing: sha256 $got, signature covers $want"
    return 1
  fi
  pub="$TRUST_DIR/$keyid.pem"
  [ -f "$pub" ] || { echo "signed by key $keyid, which is not in the trust store"; return 1; }
  sig=$(mktemp)
  jq -r .signature "$SIGNATURE" | base64 -d >"$sig"
  if openssl pkeyutl -verify -pubin -inkey "$pub" -rawin -in "$MANIFEST" -sigfile "$sig" >/dev/null 2>&1; then
    rm -f "$sig"
    echo "ed25519 signature by $keyid verifies"
    return 0
  fi
  rm -f "$sig"
  echo "ed25519 signature by $keyid does not verify"
  return 1
}
