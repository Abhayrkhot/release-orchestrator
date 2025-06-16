// Package ociimage builds single-binary OCI images byte-for-byte reproducibly
// and pushes them to a registry over the distribution HTTP API. No Docker
// daemon is involved, so any machine with the same Go toolchain and the same
// binary produces the same image digest.
package ociimage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	MediaManifest = "application/vnd.oci.image.manifest.v1+json"
	MediaConfig   = "application/vnd.oci.image.config.v1+json"
	MediaLayer    = "application/vnd.oci.image.layer.v1.tar+gzip"
)

type Blob struct {
	MediaType string
	Digest    string
	Data      []byte
}

type Image struct {
	Manifest     Blob
	Config       Blob
	Layer        Blob
	BinarySHA256 string
}

type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int    `json:"size"`
}

type BuildOptions struct {
	Binary  string            // path to a static linux binary
	Name    string            // installed as /<Name>
	Arch    string            // amd64 | arm64
	Created time.Time         // fixed timestamp (SOURCE_DATE_EPOCH)
	Labels  map[string]string // image labels
	User    string            // e.g. "65534:65534"
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func Build(o BuildOptions) (*Image, error) {
	bin, err := os.ReadFile(o.Binary)
	if err != nil {
		return nil, err
	}
	created := o.Created.UTC()

	// Layer: one file, fixed owner, mode and mtime.
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	hdr := &tar.Header{
		Name: o.Name, Mode: 0o755, Size: int64(len(bin)),
		ModTime: created, Typeflag: tar.TypeReg, Format: tar.FormatPAX,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return nil, err
	}
	if _, err := tw.Write(bin); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	diffID := digestOf(tarBuf.Bytes())

	var gz bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&gz, gzip.BestCompression)
	zw.ModTime = time.Unix(0, 0) // keep the gzip header free of wall-clock time
	if _, err := zw.Write(tarBuf.Bytes()); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	layer := Blob{MediaLayer, digestOf(gz.Bytes()), gz.Bytes()}

	cfg := map[string]any{
		"created":      created.Format(time.RFC3339),
		"architecture": o.Arch,
		"os":           "linux",
		"config": map[string]any{
			"Entrypoint": []string{"/" + o.Name},
			"User":       o.User,
			"Labels":     o.Labels,
		},
		"rootfs":  map[string]any{"type": "layers", "diff_ids": []string{diffID}},
		"history": []map[string]any{{"created": created.Format(time.RFC3339), "created_by": "relctl image build"}},
	}
	cfgBytes, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	config := Blob{MediaConfig, digestOf(cfgBytes), cfgBytes}

	man := map[string]any{
		"schemaVersion": 2,
		"mediaType":     MediaManifest,
		"config":        descriptor{MediaConfig, config.Digest, len(config.Data)},
		"layers":        []descriptor{{MediaLayer, layer.Digest, len(layer.Data)}},
		"annotations":   map[string]string{"org.opencontainers.image.created": created.Format(time.RFC3339)},
	}
	manBytes, err := json.Marshal(man)
	if err != nil {
		return nil, err
	}
	binSum := sha256.Sum256(bin)
	return &Image{
		Manifest:     Blob{MediaManifest, digestOf(manBytes), manBytes},
		Config:       config,
		Layer:        layer,
		BinarySHA256: hex.EncodeToString(binSum[:]),
	}, nil
}

// WriteLayout writes the image as an OCI image layout directory.
func (img *Image) WriteLayout(dir string) error {
	blobs := filepath.Join(dir, "blobs", "sha256")
	if err := os.MkdirAll(blobs, 0o755); err != nil {
		return err
	}
	for _, b := range []Blob{img.Manifest, img.Config, img.Layer} {
		if err := os.WriteFile(filepath.Join(blobs, strings.TrimPrefix(b.Digest, "sha256:")), b.Data, 0o644); err != nil {
			return err
		}
	}
	idx, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"manifests":     []descriptor{{MediaManifest, img.Manifest.Digest, len(img.Manifest.Data)}},
	})
	if err := os.WriteFile(filepath.Join(dir, "index.json"), idx, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o644)
}

// Registry talks to a registry over plain HTTP or HTTPS without auth, which
// is what the local cluster's registry offers.
type Registry struct {
	Base   string // e.g. http://localhost:5050
	Client *http.Client
}

func (r *Registry) client() *http.Client {
	if r.Client != nil {
		return r.Client
	}
	return &http.Client{Timeout: 60 * time.Second}
}

func (r *Registry) Push(ctx context.Context, repo, tag string, img *Image) error {
	for _, b := range []Blob{img.Layer, img.Config} {
		if err := r.pushBlob(ctx, repo, b); err != nil {
			return err
		}
	}
	for _, ref := range []string{tag, img.Manifest.Digest} {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPut, fmt.Sprintf("%s/v2/%s/manifests/%s", r.Base, repo, ref), bytes.NewReader(img.Manifest.Data))
		req.Header.Set("Content-Type", MediaManifest)
		resp, err := r.client().Do(req)
		if err != nil {
			return err
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			return fmt.Errorf("put manifest %s:%s: %s %s", repo, ref, resp.Status, body)
		}
		if got := resp.Header.Get("Docker-Content-Digest"); got != "" && got != img.Manifest.Digest {
			return fmt.Errorf("registry stored manifest as %s, expected %s", got, img.Manifest.Digest)
		}
	}
	return nil
}

func (r *Registry) pushBlob(ctx context.Context, repo string, b Blob) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodHead, fmt.Sprintf("%s/v2/%s/blobs/%s", r.Base, repo, b.Digest), nil)
	resp, err := r.client().Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	req, _ = http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/v2/%s/blobs/uploads/", r.Base, repo), nil)
	resp, err = r.client().Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("start upload %s: %s", repo, resp.Status)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		return err
	}
	base, _ := url.Parse(r.Base)
	loc = base.ResolveReference(loc)
	q := loc.Query()
	q.Set("digest", b.Digest)
	loc.RawQuery = q.Encode()
	req, _ = http.NewRequestWithContext(ctx, http.MethodPut, loc.String(), bytes.NewReader(b.Data))
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err = r.client().Do(req)
	if err != nil {
		return err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("upload blob %s: %s %s", b.Digest, resp.Status, body)
	}
	return nil
}

// HasManifest reports whether repo@digest resolves in the registry.
func (r *Registry) HasManifest(ctx context.Context, repo, digest string) (bool, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodHead, fmt.Sprintf("%s/v2/%s/manifests/%s", r.Base, repo, digest), nil)
	req.Header.Set("Accept", MediaManifest)
	resp, err := r.client().Do(req)
	if err != nil {
		return false, err
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK, nil
}

// DeleteManifest removes repo@digest (registry must allow deletes). Used by
// fault injection to simulate an artifact vanishing after it was signed.
func (r *Registry) DeleteManifest(ctx context.Context, repo, digest string) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, fmt.Sprintf("%s/v2/%s/manifests/%s", r.Base, repo, digest), nil)
	resp, err := r.client().Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("delete %s@%s: %s", repo, digest, resp.Status)
	}
	return nil
}

// ReadLayout loads an image written by WriteLayout.
func ReadLayout(dir string) (*Image, error) {
	var idx struct {
		Manifests []descriptor `json:"manifests"`
	}
	raw, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &idx); err != nil || len(idx.Manifests) != 1 {
		return nil, fmt.Errorf("%s: want exactly one manifest in index.json", dir)
	}
	blob := func(d string) ([]byte, error) {
		b, err := os.ReadFile(filepath.Join(dir, "blobs", "sha256", strings.TrimPrefix(d, "sha256:")))
		if err == nil && digestOf(b) != d {
			err = fmt.Errorf("blob %s does not match its digest", d)
		}
		return b, err
	}
	manBytes, err := blob(idx.Manifests[0].Digest)
	if err != nil {
		return nil, err
	}
	var man struct {
		Config descriptor   `json:"config"`
		Layers []descriptor `json:"layers"`
	}
	if err := json.Unmarshal(manBytes, &man); err != nil || len(man.Layers) != 1 {
		return nil, fmt.Errorf("%s: want a single-layer manifest", dir)
	}
	cfg, err := blob(man.Config.Digest)
	if err != nil {
		return nil, err
	}
	layer, err := blob(man.Layers[0].Digest)
	if err != nil {
		return nil, err
	}
	return &Image{
		Manifest: Blob{MediaManifest, idx.Manifests[0].Digest, manBytes},
		Config:   Blob{MediaConfig, man.Config.Digest, cfg},
		Layer:    Blob{MediaLayer, man.Layers[0].Digest, layer},
	}, nil
}
