package manifest

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Signature is written next to the manifest as <manifest>.sig. It signs the
// manifest's exact bytes, so the file can be checked with openssl as well:
//
//	openssl pkeyutl -verify -pubin -inkey <key_id>.pem -rawin -in release.json -sigfile sig.bin
type Signature struct {
	Algorithm      string `json:"algorithm"`
	KeyID          string `json:"key_id"`
	ManifestSHA256 string `json:"manifest_sha256"`
	Signature      string `json:"signature"`
}

// KeyID is the first 16 hex characters of sha256(public key).
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

func GenerateKey(privPath, pubPath string) (string, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	pk, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(privPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk}), 0o600); err != nil {
		return "", err
	}
	if err := writePublic(pubPath, pub); err != nil {
		return "", err
	}
	return KeyID(pub), nil
}

func writePublic(path string, pub ed25519.PublicKey) error {
	b, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return err
	}
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: b}), 0o644)
}

func LoadPrivateKey(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(raw)
	if blk == nil {
		return nil, fmt.Errorf("%s: no PEM block", path)
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	priv, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s: not an ed25519 key", path)
	}
	return priv, nil
}

func LoadPublicKey(path string) (ed25519.PublicKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(raw)
	if blk == nil {
		return nil, fmt.Errorf("%s: no PEM block", path)
	}
	k, err := x509.ParsePKIXPublicKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	pub, ok := k.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("%s: not an ed25519 key", path)
	}
	return pub, nil
}

// TrustPublicKey copies a public key into a trust directory as <key_id>.pem.
func TrustPublicKey(pubPath, trustDir string) (string, error) {
	pub, err := LoadPublicKey(pubPath)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(trustDir, 0o755); err != nil {
		return "", err
	}
	id := KeyID(pub)
	return id, writePublic(filepath.Join(trustDir, id+".pem"), pub)
}

func Sign(raw []byte, priv ed25519.PrivateKey) ([]byte, error) {
	pub := priv.Public().(ed25519.PublicKey)
	sig := Signature{
		Algorithm:      "ed25519",
		KeyID:          KeyID(pub),
		ManifestSHA256: SHA256Bytes(raw),
		Signature:      base64.StdEncoding.EncodeToString(ed25519.Sign(priv, raw)),
	}
	b, err := json.MarshalIndent(sig, "", "  ")
	return append(b, '\n'), err
}

var (
	ErrUntrustedKey = errors.New("manifest signed by a key that is not trusted")
	ErrBadSignature = errors.New("manifest signature does not verify")
)

// Verify checks sigRaw is a valid signature over raw by a key in trustDir.
func Verify(raw, sigRaw []byte, trustDir string) (keyID string, err error) {
	var sig Signature
	if err := json.Unmarshal(sigRaw, &sig); err != nil {
		return "", fmt.Errorf("parse signature: %w", err)
	}
	if sig.Algorithm != "ed25519" {
		return sig.KeyID, fmt.Errorf("unsupported signature algorithm %q", sig.Algorithm)
	}
	if sig.ManifestSHA256 != SHA256Bytes(raw) {
		return sig.KeyID, fmt.Errorf("%w: manifest sha256 %s, signature covers %s", ErrBadSignature, SHA256Bytes(raw), sig.ManifestSHA256)
	}
	if !hexKeyID(sig.KeyID) {
		return sig.KeyID, fmt.Errorf("bad key_id %q", sig.KeyID)
	}
	pub, err := LoadPublicKey(filepath.Join(trustDir, sig.KeyID+".pem"))
	if errors.Is(err, os.ErrNotExist) {
		return sig.KeyID, fmt.Errorf("%w (key_id %s)", ErrUntrustedKey, sig.KeyID)
	}
	if err != nil {
		return sig.KeyID, err
	}
	s, err := base64.StdEncoding.DecodeString(sig.Signature)
	if err != nil {
		return sig.KeyID, fmt.Errorf("decode signature: %w", err)
	}
	if !ed25519.Verify(pub, raw, s) {
		return sig.KeyID, ErrBadSignature
	}
	return sig.KeyID, nil
}

func hexKeyID(s string) bool {
	if len(s) != 16 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
