package manifest

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testManifest() *Manifest {
	svc := func(name string, deps ...string) Service {
		return Service{
			Name: name, Version: "1.0.0", Image: "registry:5000/" + name,
			Digest: "sha256:" + strings.Repeat("a", 64), BinarySHA256: strings.Repeat("b", 64),
			Count: 3, DependsOn: deps, MinSchema: 1, MaxSchema: 4, CPU: 100, MemoryMB: 64, HealthPath: "/healthz",
		}
	}
	return &Manifest{
		FormatVersion: FormatVersion, Version: "1.2.3", CreatedAt: "1970-01-01T00:00:00Z",
		Schema:   Schema{Target: 1, Migrations: []Migration{{Version: 1, Name: "init", File: "migrations/0001_init.up.sql", SHA256: strings.Repeat("c", 64)}}},
		Services: []Service{svc("order-worker", "catalog-api"), svc("edge-gateway", "catalog-api"), svc("catalog-api")},
	}
}

func TestOrderPutsDependenciesFirst(t *testing.T) {
	order, err := testManifest().Order()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range order {
		names = append(names, s.Name)
	}
	if got := strings.Join(names, ","); got != "catalog-api,edge-gateway,order-worker" {
		t.Fatalf("order = %s", got)
	}
}

func TestLintRejectsCycle(t *testing.T) {
	m := testManifest()
	m.Services[2].DependsOn = []string{"order-worker"}
	err := m.Lint()
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("want cycle error, got %v", err)
	}
}

func TestLintRejectsGapAndBadDigest(t *testing.T) {
	m := testManifest()
	m.Schema.Migrations[0].Version = 2
	m.Services[0].Digest = "latest"
	err := m.Lint()
	if err == nil || !strings.Contains(err.Error(), "contiguous") || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("want contiguity and digest errors, got %v", err)
	}
}

func TestSpecHashIgnoresDependencyOrder(t *testing.T) {
	a := testManifest().Services[0]
	b := a
	a.DependsOn = []string{"x", "y"}
	b.DependsOn = []string{"y", "x"}
	if a.SpecHash() != b.SpecHash() {
		t.Fatal("hash depends on dependency order")
	}
	b.Env = map[string]string{"FAULT_MODE": "crash_on_start"}
	if a.SpecHash() == b.SpecHash() {
		t.Fatal("hash ignores env")
	}
}

func signedBundle(t *testing.T) (dir, trust, key string) {
	t.Helper()
	tmp := t.TempDir()
	key = filepath.Join(tmp, "release.key")
	pub := filepath.Join(tmp, "release.pub")
	if _, err := GenerateKey(key, pub); err != nil {
		t.Fatal(err)
	}
	trust = filepath.Join(tmp, "trusted")
	if _, err := TrustPublicKey(pub, trust); err != nil {
		t.Fatal(err)
	}
	dir = filepath.Join(tmp, "bundle")
	os.MkdirAll(filepath.Join(dir, "migrations"), 0o755)
	body := []byte("CREATE TABLE t (id INT);\n")
	os.WriteFile(filepath.Join(dir, "migrations/0001_init.up.sql"), body, 0o644)
	m := testManifest()
	m.Schema.Migrations[0].SHA256 = SHA256Bytes(body)
	if err := WriteManifest(dir, m); err != nil {
		t.Fatal(err)
	}
	if _, err := SignBundle(dir, key); err != nil {
		t.Fatal(err)
	}
	return dir, trust, key
}

func TestBundleVerifies(t *testing.T) {
	dir, trust, _ := signedBundle(t)
	b, err := OpenBundle(dir, trust)
	if err != nil {
		t.Fatal(err)
	}
	if b.Manifest.Version != "1.2.3" {
		t.Fatalf("version %s", b.Manifest.Version)
	}
}

func TestTamperedManifestRejected(t *testing.T) {
	dir, trust, _ := signedBundle(t)
	p := filepath.Join(dir, ManifestFile)
	raw, _ := os.ReadFile(p)
	os.WriteFile(p, []byte(strings.Replace(string(raw), strings.Repeat("a", 64), strings.Repeat("f", 64), 1)), 0o644)
	if _, err := OpenBundle(dir, trust); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("want ErrBadSignature, got %v", err)
	}
}

func TestUntrustedKeyRejected(t *testing.T) {
	dir, _, _ := signedBundle(t)
	_, otherTrust, _ := signedBundle(t)
	if _, err := OpenBundle(dir, otherTrust); !errors.Is(err, ErrUntrustedKey) {
		t.Fatalf("want ErrUntrustedKey, got %v", err)
	}
}

func TestModifiedArtifactRejected(t *testing.T) {
	dir, trust, _ := signedBundle(t)
	os.WriteFile(filepath.Join(dir, "migrations/0001_init.up.sql"), []byte("DROP TABLE t;\n"), 0o644)
	if _, err := OpenBundle(dir, trust); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("want checksum error, got %v", err)
	}
}
