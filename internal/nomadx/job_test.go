package nomadx

import (
	"errors"
	"strings"
	"testing"

	"github.com/Abhayrkhot/release-orchestrator/internal/manifest"
)

func TestRenderPinsDigestAndWiresUpstreams(t *testing.T) {
	svc := manifest.Service{
		Name: "order-worker", Version: "1.1.0", Image: "registry:5000/order-worker",
		Digest: "sha256:" + strings.Repeat("a", 64), BinarySHA256: strings.Repeat("b", 64),
		Count: 3, DependsOn: []string{"catalog-api"}, MinSchema: 1, MaxSchema: 4, CPU: 100, MemoryMB: 64, HealthPath: "/healthz",
	}
	job := Render(svc, RenderOptions{ReleaseVersion: "2.0.0"})
	task := job.TaskGroups[0].Tasks[0]
	if img := task.Config["image"]; img != svc.ImageRef() {
		t.Fatalf("image %v", img)
	}
	if task.Env["CATALOG_API_UPSTREAMS"] != "/local/upstreams/catalog-api" {
		t.Fatalf("upstream env %v", task.Env)
	}
	if *job.TaskGroups[0].Update.MaxParallel != 1 || *job.TaskGroups[0].Update.AutoRevert {
		t.Fatal("rollout must be one at a time with Nomad auto-revert off")
	}
	rb := Render(svc, RenderOptions{ReleaseVersion: "1.0.0", MaxParallel: 3})
	if *rb.TaskGroups[0].Update.MaxParallel != 3 {
		t.Fatal("rollback should replace all allocations at once")
	}
	if job.Meta[MetaSpecHash] != svc.SpecHash() {
		t.Fatal("spec hash not in job meta")
	}
}

func TestTransient(t *testing.T) {
	for _, c := range []struct {
		err  string
		want bool
	}{
		{"Unexpected response code: 500 (can't fail terminal deployment)", false},
		{"Unexpected response code: 500 (No cluster leader)", true},
		{`Put "http://nomad-server:4646/v1/jobs": dial tcp 172.28.0.4:4646: connect: connection refused`, true},
		{"Unexpected response code: 404 (job not found)", false},
	} {
		if got := transient(errors.New(c.err)); got != c.want {
			t.Errorf("transient(%q) = %v, want %v", c.err, got, c.want)
		}
	}
}
