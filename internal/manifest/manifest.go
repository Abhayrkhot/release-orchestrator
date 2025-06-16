// Package manifest defines the signed release manifest: which image digest
// each service runs, which schema migrations ship with the release, and the
// checksums that tie the two together.
package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/Abhayrkhot/release-orchestrator/internal/semver"
)

const FormatVersion = 1

type Manifest struct {
	FormatVersion int       `json:"format_version"`
	Version       string    `json:"version"`
	Commit        string    `json:"commit,omitempty"`
	CreatedAt     string    `json:"created_at"`
	Schema        Schema    `json:"schema"`
	Services      []Service `json:"services"`
}

type Schema struct {
	Target     int         `json:"target"`
	Migrations []Migration `json:"migrations"`
}

type Migration struct {
	Version    int    `json:"version"`
	Name       string `json:"name"`
	File       string `json:"file"`
	SHA256     string `json:"sha256"`
	DownFile   string `json:"down_file,omitempty"`
	DownSHA256 string `json:"down_sha256,omitempty"`
}

type Service struct {
	Name         string            `json:"name"`
	Version      string            `json:"version"`
	Image        string            `json:"image"`
	Digest       string            `json:"digest"`
	BinarySHA256 string            `json:"binary_sha256"`
	Count        int               `json:"count"`
	DependsOn    []string          `json:"depends_on,omitempty"`
	MinSchema    int               `json:"min_schema"`
	MaxSchema    int               `json:"max_schema"`
	CPU          int               `json:"cpu"`
	MemoryMB     int               `json:"memory_mb"`
	HealthPath   string            `json:"health_path"`
	Env          map[string]string `json:"env,omitempty"`
}

// ImageRef is the pull reference pinned by digest.
func (s Service) ImageRef() string { return s.Image + "@" + s.Digest }

// SpecHash identifies everything that changes what runs. Two releases with the
// same SpecHash for a service leave that service untouched.
func (s Service) SpecHash() string {
	c := s
	c.DependsOn = append([]string(nil), s.DependsOn...)
	sort.Strings(c.DependsOn)
	b, _ := json.Marshal(c) // map keys are sorted by encoding/json
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (m *Manifest) Service(name string) (Service, bool) {
	for _, s := range m.Services {
		if s.Name == name {
			return s, true
		}
	}
	return Service{}, false
}

func Parse(raw []byte) (*Manifest, error) {
	var m Manifest
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	return &m, nil
}

func Load(path string) (*Manifest, []byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	m, err := Parse(raw)
	return m, raw, err
}

func (m *Manifest) Marshal() ([]byte, error) {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

var (
	nameRE   = regexp.MustCompile(`^[a-z][a-z0-9-]{1,40}$`)
	digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	hexRE    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Lint checks the manifest is internally consistent. It does not look at the
// cluster, the database or the signature.
func (m *Manifest) Lint() error {
	var errs []error
	add := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }

	if m.FormatVersion != FormatVersion {
		add("format_version %d, want %d", m.FormatVersion, FormatVersion)
	}
	if _, err := semver.Parse(m.Version); err != nil {
		add("release %v", err)
	}
	if len(m.Services) == 0 {
		add("no services")
	}
	for i, mg := range m.Schema.Migrations {
		if mg.Version != i+1 {
			add("migration %d: versions must be contiguous from 1, got %d at position %d", mg.Version, mg.Version, i+1)
		}
		if !hexRE.MatchString(mg.SHA256) {
			add("migration %d: bad sha256", mg.Version)
		}
		if mg.File == "" {
			add("migration %d: no file", mg.Version)
		}
	}
	if m.Schema.Target != len(m.Schema.Migrations) {
		add("schema target %d but %d migrations listed", m.Schema.Target, len(m.Schema.Migrations))
	}
	seen := map[string]bool{}
	for _, s := range m.Services {
		if !nameRE.MatchString(s.Name) {
			add("service %q: bad name", s.Name)
		}
		if seen[s.Name] {
			add("service %q: duplicate", s.Name)
		}
		seen[s.Name] = true
		if _, err := semver.Parse(s.Version); err != nil {
			add("service %s: %v", s.Name, err)
		}
		if !digestRE.MatchString(s.Digest) {
			add("service %s: digest %q is not sha256:<64 hex>", s.Name, s.Digest)
		}
		if !hexRE.MatchString(s.BinarySHA256) {
			add("service %s: bad binary_sha256", s.Name)
		}
		if s.Image == "" || strings.Contains(s.Image, "@") {
			add("service %s: image must be a repository without tag or digest", s.Name)
		}
		if s.Count < 1 {
			add("service %s: count must be >= 1", s.Name)
		}
		if s.MinSchema < 0 || s.MaxSchema < s.MinSchema {
			add("service %s: schema range [%d,%d] invalid", s.Name, s.MinSchema, s.MaxSchema)
		}
		if s.CPU <= 0 || s.MemoryMB <= 0 {
			add("service %s: cpu and memory_mb must be > 0", s.Name)
		}
		if !strings.HasPrefix(s.HealthPath, "/") {
			add("service %s: health_path must start with /", s.Name)
		}
	}
	for _, s := range m.Services {
		for _, d := range s.DependsOn {
			if !seen[d] {
				add("service %s depends on unknown service %q", s.Name, d)
			}
		}
	}
	if len(errs) == 0 {
		if _, err := m.Order(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Order returns services so every service comes after its dependencies.
// Ties are broken by name so the order is deterministic.
func (m *Manifest) Order() ([]Service, error) {
	byName := map[string]Service{}
	indeg := map[string]int{}
	users := map[string][]string{}
	for _, s := range m.Services {
		byName[s.Name] = s
		indeg[s.Name] += 0
		for _, d := range s.DependsOn {
			indeg[s.Name]++
			users[d] = append(users[d], s.Name)
		}
	}
	var ready []string
	for n, d := range indeg {
		if d == 0 {
			ready = append(ready, n)
		}
	}
	var out []Service
	for len(ready) > 0 {
		sort.Strings(ready)
		n := ready[0]
		ready = ready[1:]
		out = append(out, byName[n])
		for _, u := range users[n] {
			indeg[u]--
			if indeg[u] == 0 {
				ready = append(ready, u)
			}
		}
	}
	if len(out) != len(m.Services) {
		var stuck []string
		for n, d := range indeg {
			if d > 0 {
				stuck = append(stuck, n)
			}
		}
		sort.Strings(stuck)
		return nil, fmt.Errorf("dependency cycle: cannot order %s", strings.Join(stuck, ", "))
	}
	return out, nil
}

func SHA256File(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func SHA256Bytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
