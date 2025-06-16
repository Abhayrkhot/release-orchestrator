package manifest

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"
)

// A bundle is a directory holding everything one release needs:
//
//	release.json       the manifest
//	release.json.sig   its signature
//	migrations/*.sql   the schema migrations the manifest lists, by checksum
const (
	ManifestFile  = "release.json"
	SignatureFile = "release.json.sig"
)

type Bundle struct {
	Dir       string
	Manifest  *Manifest
	Raw       []byte
	Signature []byte
	KeyID     string
}

// OpenBundle verifies the signature and every artifact checksum, then lints.
// Nothing from an unverified bundle is returned.
func OpenBundle(dir, trustDir string) (*Bundle, error) {
	raw, err := os.ReadFile(filepath.Join(dir, ManifestFile))
	if err != nil {
		return nil, err
	}
	sig, err := os.ReadFile(filepath.Join(dir, SignatureFile))
	if err != nil {
		return nil, fmt.Errorf("signature: %w", err)
	}
	keyID, err := Verify(raw, sig, trustDir)
	if err != nil {
		return nil, err
	}
	m, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	if err := VerifyArtifacts(m, dir); err != nil {
		return nil, err
	}
	if err := m.Lint(); err != nil {
		return nil, fmt.Errorf("lint: %w", err)
	}
	return &Bundle{Dir: dir, Manifest: m, Raw: raw, Signature: sig, KeyID: keyID}, nil
}

// VerifyArtifacts recomputes the checksum of every file the manifest lists.
func VerifyArtifacts(m *Manifest, dir string) error {
	for _, mg := range m.Schema.Migrations {
		for _, f := range []struct{ path, want string }{{mg.File, mg.SHA256}, {mg.DownFile, mg.DownSHA256}} {
			if f.path == "" {
				continue
			}
			got, err := SHA256File(filepath.Join(dir, f.path))
			if err != nil {
				return fmt.Errorf("artifact %s: %w", f.path, err)
			}
			if got != f.want {
				return fmt.Errorf("artifact %s: checksum %s, manifest says %s", f.path, got, f.want)
			}
		}
	}
	return nil
}

// Spec is the hand-written part of a release: how each service runs.
// Digests, checksums and migrations are filled in by CreateBundle.
type Spec struct {
	Registry string        `json:"registry"`
	Services []SpecService `json:"services"`
}

type SpecService struct {
	Name       string            `json:"name"`
	Count      int               `json:"count"`
	DependsOn  []string          `json:"depends_on,omitempty"`
	MinSchema  int               `json:"min_schema"`
	MaxSchema  int               `json:"max_schema"`
	CPU        int               `json:"cpu"`
	MemoryMB   int               `json:"memory_mb"`
	HealthPath string            `json:"health_path"`
	Env        map[string]string `json:"env,omitempty"`
}

func LoadSpec(path string) (*Spec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Spec
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("parse spec: %w", err)
	}
	return &s, nil
}

// ImageDescriptor is what `relctl image build` writes for each image.
type ImageDescriptor struct {
	Name         string `json:"name"`
	Version      string `json:"version"`
	Arch         string `json:"arch"`
	Repository   string `json:"repository"`
	Digest       string `json:"digest"`
	BinarySHA256 string `json:"binary_sha256"`
	ConfigDigest string `json:"config_digest"`
	LayerDigest  string `json:"layer_digest"`
}

func DescriptorPath(dir, name, version, arch string) string {
	return filepath.Join(dir, fmt.Sprintf("%s-%s-%s.json", name, version, arch))
}

func LoadDescriptor(path string) (*ImageDescriptor, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var d ImageDescriptor
	return &d, json.Unmarshal(raw, &d)
}

type CreateOptions struct {
	Version         string
	Spec            *Spec
	ServiceVersions map[string]string // service -> image version; "" key is the default
	ImagesDir       string
	Arch            string
	MigrationsDir   string
	SchemaTarget    int // 0 means every migration found
	Commit          string
	CreatedAt       time.Time
	OutDir          string
}

var migrationRE = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.up\.sql$`)

// FindMigrations lists *.up.sql files in version order.
func FindMigrations(dir string) ([]Migration, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []Migration
	for _, e := range ents {
		mt := migrationRE.FindStringSubmatch(e.Name())
		if mt == nil {
			continue
		}
		v, _ := strconv.Atoi(mt[1])
		mg := Migration{Version: v, Name: mt[2], File: filepath.Join("migrations", e.Name())}
		down := fmt.Sprintf("%s_%s.down.sql", mt[1], mt[2])
		if _, err := os.Stat(filepath.Join(dir, down)); err == nil {
			mg.DownFile = filepath.Join("migrations", down)
		}
		out = append(out, mg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// CleanBundle removes the files a bundle consists of (and nothing else) so
// a bundle directory can be rebuilt without stale migrations.
func CleanBundle(dir string) error {
	for _, f := range []string{ManifestFile, SignatureFile} {
		if err := os.Remove(filepath.Join(dir, f)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	ents, err := os.ReadDir(filepath.Join(dir, "migrations"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range ents {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".sql" {
			if err := os.Remove(filepath.Join(dir, "migrations", e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// CreateBundle writes an unsigned bundle to opts.OutDir.
func CreateBundle(o CreateOptions) (*Manifest, error) {
	all, err := FindMigrations(o.MigrationsDir)
	if err != nil {
		return nil, err
	}
	target := o.SchemaTarget
	if target == 0 {
		target = len(all)
	}
	if target > len(all) {
		return nil, fmt.Errorf("schema target %d but only %d migrations in %s", target, len(all), o.MigrationsDir)
	}
	if err := os.MkdirAll(filepath.Join(o.OutDir, "migrations"), 0o755); err != nil {
		return nil, err
	}
	m := &Manifest{
		FormatVersion: FormatVersion,
		Version:       o.Version,
		Commit:        o.Commit,
		CreatedAt:     o.CreatedAt.UTC().Format(time.RFC3339),
		Schema:        Schema{Target: target},
	}
	for _, mg := range all[:target] {
		for _, f := range []struct {
			rel string
			sum *string
		}{{mg.File, &mg.SHA256}, {mg.DownFile, &mg.DownSHA256}} {
			if f.rel == "" {
				continue
			}
			b, err := os.ReadFile(filepath.Join(o.MigrationsDir, filepath.Base(f.rel)))
			if err != nil {
				return nil, err
			}
			if err := os.WriteFile(filepath.Join(o.OutDir, f.rel), b, 0o644); err != nil {
				return nil, err
			}
			*f.sum = SHA256Bytes(b)
		}
		m.Schema.Migrations = append(m.Schema.Migrations, mg)
	}
	for _, ss := range o.Spec.Services {
		v := o.ServiceVersions[ss.Name]
		if v == "" {
			v = o.ServiceVersions[""]
		}
		d, err := LoadDescriptor(DescriptorPath(o.ImagesDir, ss.Name, v, o.Arch))
		if err != nil {
			return nil, fmt.Errorf("service %s %s: %w (run `make images`?)", ss.Name, v, err)
		}
		m.Services = append(m.Services, Service{
			Name: ss.Name, Version: v,
			Image:  o.Spec.Registry + "/" + d.Repository,
			Digest: d.Digest, BinarySHA256: d.BinarySHA256,
			Count: ss.Count, DependsOn: ss.DependsOn,
			MinSchema: ss.MinSchema, MaxSchema: ss.MaxSchema,
			CPU: ss.CPU, MemoryMB: ss.MemoryMB, HealthPath: ss.HealthPath,
			Env: ss.Env,
		})
	}
	if err := m.Lint(); err != nil {
		return nil, err
	}
	return m, WriteManifest(o.OutDir, m)
}

func WriteManifest(dir string, m *Manifest) error {
	b, err := m.Marshal()
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, ManifestFile), b, 0o644)
}

// SignBundle signs dir/release.json in place.
func SignBundle(dir, keyPath string) (string, error) {
	priv, err := LoadPrivateKey(keyPath)
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(filepath.Join(dir, ManifestFile))
	if err != nil {
		return "", err
	}
	if _, err := Parse(raw); err != nil {
		return "", err
	}
	sig, err := Sign(raw, priv)
	if err != nil {
		return "", err
	}
	return KeyID(priv.Public().(ed25519.PublicKey)), os.WriteFile(filepath.Join(dir, SignatureFile), sig, 0o644)
}
