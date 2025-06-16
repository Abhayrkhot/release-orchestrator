package ociimage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBuildIsReproducible(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "svc")
	os.WriteFile(bin, []byte("\x7fELF not really a binary"), 0o755)
	opts := BuildOptions{Binary: bin, Name: "svc", Arch: "arm64", Created: time.Unix(1700000000, 0), User: "65534:65534",
		Labels: map[string]string{"b": "2", "a": "1"}}
	a, err := Build(opts)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Build(opts)
	if err != nil {
		t.Fatal(err)
	}
	if a.Manifest.Digest != b.Manifest.Digest {
		t.Fatalf("digests differ: %s vs %s", a.Manifest.Digest, b.Manifest.Digest)
	}
	opts.Created = opts.Created.Add(time.Second)
	c, _ := Build(opts)
	if c.Manifest.Digest == a.Manifest.Digest {
		t.Fatal("timestamp does not reach the digest")
	}
}

func TestLayoutRoundTrip(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "svc")
	os.WriteFile(bin, []byte("binary"), 0o755)
	img, err := Build(BuildOptions{Binary: bin, Name: "svc", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	if err := img.WriteLayout(filepath.Join(dir, "oci")); err != nil {
		t.Fatal(err)
	}
	got, err := ReadLayout(filepath.Join(dir, "oci"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Manifest.Digest != img.Manifest.Digest || got.Layer.Digest != img.Layer.Digest {
		t.Fatal("layout round trip changed digests")
	}
}
