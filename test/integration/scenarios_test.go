//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Abhayrkhot/release-orchestrator/internal/manifest"
	"github.com/Abhayrkhot/release-orchestrator/internal/ociimage"
)

// ---- a new version fails on its own ----------------------------------------

func TestR01CrashOnStart(t *testing.T) {
	r := begin(t, "R01", "New catalog-api version crashes on start")
	prev := h.status(t).Release
	res, c := h.deploy(t, h.release(t, "bad", relOpts{mut: withEnv("catalog-api", "FAULT_MODE", "crash_on_start")}))
	h.expectRolledBack(t, r, res, c.Code, prev)
}

func TestR02FailsHealthChecks(t *testing.T) {
	r := begin(t, "R02", "New version runs but never passes its health check")
	prev := h.status(t).Release
	res, c := h.deploy(t, h.release(t, "bad", relOpts{mut: withEnv("catalog-api", "FAULT_MODE", "unhealthy")}))
	h.expectRolledBack(t, r, res, c.Code, prev)
}

func TestR03SlowStart(t *testing.T) {
	r := begin(t, "R03", "New version starts slower than the health deadline")
	prev := h.status(t).Release
	res, c := h.deploy(t, h.release(t, "bad", relOpts{mut: withEnv("catalog-api", "FAULT_MODE", "slow_start", "FAULT_DELAY", "60s")}))
	h.expectRolledBack(t, r, res, c.Code, prev)
}

func TestR04CrashAfterHealthy(t *testing.T) {
	r := begin(t, "R04", "New version passes checks, then crashes mid-rollout")
	prev := h.status(t).Release
	res, c := h.deploy(t, h.release(t, "bad", relOpts{mut: withEnv("catalog-api", "FAULT_MODE", "crash_after", "FAULT_DELAY", "8s")}))
	h.expectRolledBack(t, r, res, c.Code, prev)
}

func TestR05OutOfMemory(t *testing.T) {
	r := begin(t, "R05", "New version exceeds its memory limit and is OOM-killed")
	prev := h.status(t).Release
	res, c := h.deploy(t, h.release(t, "bad", relOpts{mut: withEnv("catalog-api", "FAULT_MODE", "oom")}))
	h.expectRolledBack(t, r, res, c.Code, prev)
}

func TestR06BinaryRejectsSchema(t *testing.T) {
	r := begin(t, "R06", "Binary refuses the live schema although its manifest accepts it")
	prev := h.status(t).Release
	res, c := h.deploy(t, h.release(t, "bad", relOpts{mut: withEnv("edge-gateway", "FAULT_MODE", "bad_schema")}))
	h.expectRolledBack(t, r, res, c.Code, prev)
}

func TestR07FailsOnOneNode(t *testing.T) {
	r := begin(t, "R07", "New version fails on one node only (partial failure)")
	prev := h.status(t).Release
	res, c := h.deploy(t, h.release(t, "bad", relOpts{mut: withEnv("catalog-api", "FAULT_MODE", "crash_on_start", "FAULT_NODE", "node2")}))
	h.expectRolledBack(t, r, res, c.Code, prev)
	r.note("allocations already upgraded on other nodes were reverted too")
}

// ---- restart ordering across services ------------------------------------

func TestR08LastServiceFails(t *testing.T) {
	r := begin(t, "R08", "Last service in dependency order fails; all three roll back in reverse")
	prev := h.status(t).Release
	res, c := h.deploy(t, h.release(t, "bad", relOpts{mut: chain(touchAll(t.Name()), withEnv("order-worker", "FAULT_MODE", "crash_on_start"))}))
	h.expectRolledBack(t, r, res, c.Code, prev)
	got := h.rollbackOrder(t, res.ReleaseID)
	want := []string{"order-worker", "edge-gateway", "catalog-api"}
	if !slices.Equal(got, want) {
		t.Fatalf("rollback order %v, want %v", got, want)
	}
	r.note("rollback order %v", got)
}

func TestR09MiddleServiceFails(t *testing.T) {
	r := begin(t, "R09", "Middle service fails; services after it are never touched")
	prev := h.status(t).Release
	before := h.jobVersion(t, "order-worker")
	res, c := h.deploy(t, h.release(t, "bad", relOpts{mut: chain(touchAll(t.Name()), withEnv("edge-gateway", "FAULT_MODE", "unhealthy"))}))
	h.expectRolledBack(t, r, res, c.Code, prev)
	if got := h.rollbackOrder(t, res.ReleaseID); !slices.Equal(got, []string{"edge-gateway", "catalog-api"}) {
		t.Fatalf("rollback order %v", got)
	}
	if after := h.jobVersion(t, "order-worker"); after != before {
		t.Fatalf("order-worker job changed from version %d to %d", before, after)
	}
	r.note("order-worker job version unchanged at %d", before)
}

func TestR25DependentsWaitForUpstream(t *testing.T) {
	r := begin(t, "R25", "Dependents roll out only after catalog-api is healthy")
	res, c := h.deploy(t, h.release(t, "good", relOpts{mut: touchAll(t.Name())}))
	r.took(res)
	if c.Code != 0 {
		t.Fatalf("deploy: %s %s", res.Status, res.Reason)
	}
	var apiHealthy time.Time
	for _, e := range h.events(t, res.ReleaseID) {
		if e.Service == "catalog-api" && e.Kind == "healthy" {
			apiHealthy = e.At
		}
	}
	if apiHealthy.IsZero() {
		t.Fatal("no healthy event for catalog-api")
	}
	// Release versions repeat after a rollback, so only count events since
	// this attempt began.
	var began time.Time
	if err := h.db.QueryRow(`SELECT started_at FROM relctl.releases WHERE id = ?`, res.ReleaseID).Scan(&began); err != nil {
		t.Fatal(err)
	}
	var early int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM app.service_events
		WHERE release_version = ? AND event = 'start' AND service IN ('order-worker', 'edge-gateway') AND at >= ? AND at < ?`,
		res.Version, began, apiHealthy).Scan(&early); err != nil {
		t.Fatal(err)
	}
	if early > 0 {
		t.Fatalf("%d dependent instances started before catalog-api was healthy", early)
	}
	var started int
	h.db.QueryRow(`SELECT COUNT(*) FROM app.service_events WHERE release_version = ? AND event = 'start'
		AND service IN ('order-worker', 'edge-gateway') AND at >= ?`, res.Version, began).Scan(&started)
	if started < 6 {
		t.Fatalf("only %d dependent starts recorded for %s", started, res.Version)
	}
	r.note("all %d dependent instances started after catalog-api was healthy at %s", started, apiHealthy.Format("15:04:05.000"))
	h.requireConverged(t, res.Version, time.Minute)
}

func TestR23ColdRestartOrdering(t *testing.T) {
	r := begin(t, "R23", "All three nodes restart at once; dependents become ready only after catalog-api")
	cur := h.status(t).Release
	var t0 time.Time
	h.db.QueryRow(`SELECT NOW(3)`).Scan(&t0)
	docker(t, "restart", "-t", "2", "relorch-node1", "relorch-node2", "relorch-node3")
	start := time.Now()
	deadline := start.Add(4 * time.Minute)
	reconciled := false
	for {
		if ok, _ := h.converged(t); ok {
			break
		}
		if time.Now().After(deadline) {
			ok, why := h.converged(t)
			t.Fatalf("not recovered after %s (reconcile used: %v): %v %s", time.Since(start), reconciled, ok, why)
		}
		time.Sleep(3 * time.Second)
	}
	r.s.Outcome = "recovered"
	r.s.RecoverySeconds = round(time.Since(start).Seconds())
	r.note("all services healthy and verified %.0fs after the restart, without operator action", time.Since(start).Seconds())
	h.requireConverged(t, cur, time.Minute)

	var apiReady, depReady time.Time
	var nDep int
	h.db.QueryRow(`SELECT IFNULL(MIN(at), NOW(3)) FROM app.service_events WHERE service = 'catalog-api' AND event = 'ready' AND at > ?`, t0).Scan(&apiReady)
	h.db.QueryRow(`SELECT IFNULL(MIN(at), NOW(3)), COUNT(*) FROM app.service_events WHERE service IN ('order-worker', 'edge-gateway') AND event = 'ready' AND at > ?`, t0).Scan(&depReady, &nDep)
	if nDep == 0 {
		t.Fatal("no dependent became ready after the restart")
	}
	if depReady.Before(apiReady) {
		t.Fatalf("a dependent was ready at %s, before the first catalog-api at %s", depReady.Format("15:04:05.000"), apiReady.Format("15:04:05.000"))
	}
	r.note("first catalog-api ready %s; first of %d dependents ready %s", apiReady.Format("15:04:05.000"), nDep, depReady.Format("15:04:05.000"))
}

func TestR24TransientUpstreamOutage(t *testing.T) {
	r := begin(t, "R24", "catalog-api briefly unavailable during an order-worker rollout; no false rollback")
	b := h.deployBG(t, h.release(t, "good", relOpts{fromCurrent: true, mut: withEnv("order-worker", "RELEASE_TAG", t.Name())}))
	h.waitStage(t, "rollout:order-worker", 2*time.Minute)
	time.Sleep(time.Second)
	eps, _ := h.nomad.Endpoints(context.Background(), "catalog-api")
	for _, e := range eps {
		exec.Command("curl", "-fsS", "-m", "2", "-X", "POST", "http://"+e.Addr+"/admin/fault?mode=unhealthy").Run()
	}
	r.note("all %d catalog-api instances failing health checks for 6s", len(eps))
	time.Sleep(6 * time.Second)
	h.healAPI(t)
	res, code := b.wait(t, 5*time.Minute)
	r.took(res)
	if code != 0 || res.Status != "succeeded" {
		t.Fatalf("want succeeded, got %s (exit %d): %s", res.Status, code, res.Reason)
	}
	h.requireConverged(t, res.Version, time.Minute)
}

// ---- infrastructure fails mid-rollout -------------------------------------

// freshImage pushes a copy of a service image under a new digest, so nodes
// have to pull it during the rollout.
func freshImage(t *testing.T, svc string) (digest string, layout string) {
	t.Helper()
	img, err := ociimage.Build(ociimage.BuildOptions{
		Binary: filepath.Join(h.root, "dist/build", h.arch, svc+"-1.1.0"), Name: svc, Arch: h.arch,
		Created: time.Now(), User: "65534:65534", Labels: map[string]string{"scenario": t.Name()},
	})
	if err != nil {
		t.Fatal(err)
	}
	layout = filepath.Join(h.work, t.Name(), "oci-"+svc)
	if err := img.WriteLayout(layout); err != nil {
		t.Fatal(err)
	}
	if err := (&ociimage.Registry{Base: "http://registry:5000"}).Push(context.Background(), svc, "scenario", img); err != nil {
		t.Fatal(err)
	}
	return img.Manifest.Digest, layout
}

func withDigest(svc, digest string) func(*manifest.Manifest) {
	return func(m *manifest.Manifest) {
		for i := range m.Services {
			if m.Services[i].Name == svc {
				m.Services[i].Digest = digest
			}
		}
	}
}

func TestR10ImageVanishesMidRollout(t *testing.T) {
	r := begin(t, "R10", "Image deleted from the registry after preflight, before its service rolls out")
	prev := h.status(t).Release
	api, _ := freshImage(t, "catalog-api")
	gw, _ := freshImage(t, "edge-gateway")
	b := h.deployBG(t, h.release(t, "race", relOpts{mut: chain(withDigest("catalog-api", api), withDigest("edge-gateway", gw))}))
	h.waitStage(t, "rollout:catalog-api", 2*time.Minute)
	if err := (&ociimage.Registry{Base: "http://registry:5000"}).DeleteManifest(context.Background(), "edge-gateway", gw); err != nil {
		t.Fatal(err)
	}
	r.note("deleted edge-gateway@%s while catalog-api was rolling out", gw[7:19])
	res, code := b.wait(t, 5*time.Minute)
	h.expectRolledBack(t, r, res, code, prev)
}

func TestR11RegistryDownMidRollout(t *testing.T) {
	r := begin(t, "R11", "Registry goes down mid-rollout; rollback works from node image caches")
	prev := h.status(t).Release
	api, _ := freshImage(t, "catalog-api")
	b := h.deployBG(t, h.release(t, "race", relOpts{mut: withDigest("catalog-api", api)}))
	h.waitStage(t, "rollout:catalog-api", 2*time.Minute)
	time.Sleep(500 * time.Millisecond)
	docker(t, "stop", "-t", "1", "relorch-registry")
	r.note("registry stopped 0.5s into the catalog-api rollout")
	res, code := b.wait(t, 5*time.Minute)
	r.took(res)
	switch {
	case code == 4 && res.Status == "rolled_back":
		r.note("rolled back: %s", res.Reason)
		r.note("rollback %.1fs after detection, %.1fs after the bad rollout began, registry still down", res.RollbackSeconds, res.RecoverySeconds)
		// Converged with the registry still stopped: old images came from node caches.
		h.requireConvergedNoRegistry(t, prev)
		if res.RecoverySeconds >= 60 {
			t.Errorf("recovery took %.1fs", res.RecoverySeconds)
		}
	case code == 0:
		// Possible if every node pulled the image before the registry stopped.
		r.note("every node had pulled the image before the registry stopped; release succeeded")
		h.requireConvergedNoRegistry(t, res.Version)
	default:
		t.Fatalf("got %s (exit %d): %s", res.Status, code, res.Reason)
	}
}

// requireConvergedNoRegistry checks health and binaries without the registry.
func (h *harness) requireConvergedNoRegistry(t *testing.T, want string) {
	t.Helper()
	h.requireConverged(t, want, 90*time.Second)
	if exec.Command("curl", "-fsS", "-m", "2", "-o", "/dev/null", "http://registry:5000/v2/").Run() == nil {
		t.Fatal("registry came back during the check")
	}
}

func TestR12NodeLostMidRollout(t *testing.T) {
	r := begin(t, "R12", "A node dies mid-rollout")
	prev := h.status(t).Release
	b := h.deployBG(t, h.release(t, "good", relOpts{mut: touchAll(t.Name())}))
	h.waitStage(t, "rollout:catalog-api", 2*time.Minute)
	time.Sleep(2 * time.Second)
	docker(t, "kill", "relorch-node3")
	r.note("node3 killed during the catalog-api rollout")
	res, code := b.wait(t, 6*time.Minute)
	r.took(res)
	want := prev
	switch {
	case code == 0:
		want = res.Version
		r.note("release completed on the two remaining nodes")
	case code == 4:
		r.note("rolled back (%s) in %.1fs", res.Reason, res.RecoverySeconds)
	default:
		t.Fatalf("got %s (exit %d): %s", res.Status, code, res.Reason)
	}
	h.requireConverged(t, want, 2*time.Minute)
	for _, s := range h.status(t).Services {
		for _, e := range s.Endpoints {
			if e.Node == "node3" {
				t.Fatalf("%s still has an allocation on the dead node", s.Name)
			}
		}
	}
	r.note("all 9 allocations healthy on node1 and node2 with node3 down")
}

func TestR13NodeDrainedMidRollout(t *testing.T) {
	r := begin(t, "R13", "A node is drained mid-rollout")
	b := h.deployBG(t, h.release(t, "good", relOpts{mut: touchAll(t.Name())}))
	h.waitStage(t, "rollout:catalog-api", 2*time.Minute)
	time.Sleep(2 * time.Second)
	id := h.nodeID(t, "node2")
	out, err := exec.Command("nomad", "node", "drain", "-enable", "-yes", "-detach", "-deadline", "30s", id).CombinedOutput()
	if err != nil {
		t.Fatalf("drain: %v %s", err, out)
	}
	r.note("node2 drained during the catalog-api rollout")
	res, code := b.wait(t, 6*time.Minute)
	r.took(res)
	want := h.status(t).Release
	if code == 0 {
		want = res.Version
	} else if code != 4 {
		t.Fatalf("got %s (exit %d): %s", res.Status, code, res.Reason)
	}
	r.note("deploy %s", res.Status)
	waitFor(t, "allocations to leave node2", 2*time.Minute, func() bool {
		for _, s := range h.status(t).Services {
			for _, e := range s.Endpoints {
				if e.Node == "node2" {
					return false
				}
			}
		}
		return true
	})
	h.requireConverged(t, want, 2*time.Minute)
	r.note("converged on %s with node2 drained", want)
}

func TestR14NomadServerRestart(t *testing.T) {
	r := begin(t, "R14", "Nomad server restarts mid-rollout")
	b := h.deployBG(t, h.release(t, "good", relOpts{mut: touchAll(t.Name())}))
	h.waitStage(t, "rollout:catalog-api", 2*time.Minute)
	time.Sleep(2 * time.Second)
	docker(t, "restart", "-t", "2", "relorch-nomad-server")
	r.note("Nomad server restarted during the catalog-api rollout")
	res, code := b.wait(t, 6*time.Minute)
	r.took(res)
	if code != 0 && code != 4 {
		t.Fatalf("got %s (exit %d): %s", res.Status, code, res.Reason)
	}
	want := res.Version
	if code == 4 {
		want = res.Previous
	}
	r.note("deploy %s", res.Status)
	h.requireConverged(t, want, 2*time.Minute)
}

func TestR15MySQLRestart(t *testing.T) {
	r := begin(t, "R15", "MySQL restarts mid-rollout")
	b := h.deployBG(t, h.release(t, "good", relOpts{mut: touchAll(t.Name())}))
	h.waitStage(t, "rollout:catalog-api", 2*time.Minute)
	time.Sleep(2 * time.Second)
	docker(t, "restart", "-t", "2", "relorch-mysql")
	r.note("MySQL restarted during the catalog-api rollout")
	res, code := b.wait(t, 6*time.Minute)
	r.took(res)
	if code != 0 && code != 4 {
		t.Fatalf("got %s (exit %d): %s", res.Status, code, res.Reason)
	}
	want := res.Version
	if code == 4 {
		want = res.Previous
	}
	r.note("deploy %s", res.Status)
	h.requireConverged(t, want, 2*time.Minute)
}

// ---- the controller itself fails --------------------------------------------

func TestR16ControllerKilledMidRollout(t *testing.T) {
	r := begin(t, "R16", "Controller killed mid-rollout; reconcile restores the last good release")
	prev := h.status(t).Release
	b := h.deployBG(t, h.release(t, "good", relOpts{mut: touchAll(t.Name())}))
	h.waitStage(t, "rollout:edge-gateway", 3*time.Minute)
	time.Sleep(2 * time.Second)
	b.kill(t)
	r.note("controller SIGKILLed during the edge-gateway rollout (catalog-api already upgraded)")

	// A second controller must not start while the lease is live, and then
	// must not deploy over the interrupted release.
	res, c := h.deploy(t, h.release(t, "next", relOpts{}))
	if c.Code != 6 && c.Code != 3 {
		t.Fatalf("deploy during interrupted release: %s (exit %d)", res.Status, c.Code)
	}
	r.note("a new deploy was refused (%s)", res.Status)

	start := time.Now()
	var rc cmdResult
	waitFor(t, "lease to expire and reconcile to run", 2*time.Minute, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()
		rc = h.run(ctx, "reconcile", "--json")
		return rc.Code != 6
	})
	if rc.Code != 0 {
		t.Fatalf("reconcile: exit %d %s", rc.Code, tail(rc.Stderr, 10))
	}
	r.s.Outcome = "reconciled"
	r.s.RecoverySeconds = round(time.Since(start).Seconds())
	r.note("reconcile restored %s %.1fs after it was started (including waiting out the 15s lease)", prev, time.Since(start).Seconds())
	h.requireConverged(t, prev, time.Minute)
	var st string
	h.db.QueryRow(`SELECT status FROM relctl.releases WHERE kind = 'deploy' ORDER BY id DESC LIMIT 1 OFFSET 0`).Scan(&st)
	r.note("interrupted release marked %q", st)
}

func TestR17ControllerKilledMidRollback(t *testing.T) {
	r := begin(t, "R17", "Controller killed while rolling back; reconcile finishes the job")
	prev := h.status(t).Release
	b := h.deployBG(t, h.release(t, "bad", relOpts{mut: chain(touchAll(t.Name()), withEnv("edge-gateway", "FAULT_MODE", "crash_on_start"))}))
	h.waitStage(t, "rollback", 3*time.Minute)
	b.kill(t)
	r.note("controller SIGKILLed as soon as it started rolling back")
	start := time.Now()
	var rc cmdResult
	waitFor(t, "reconcile", 2*time.Minute, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()
		rc = h.run(ctx, "reconcile", "--json")
		return rc.Code != 6
	})
	if rc.Code != 0 {
		t.Fatalf("reconcile: exit %d %s", rc.Code, tail(rc.Stderr, 10))
	}
	r.s.Outcome = "reconciled"
	r.s.RecoverySeconds = round(time.Since(start).Seconds())
	h.requireConverged(t, prev, time.Minute)
	r.note("back on %s %.1fs after reconcile started", prev, time.Since(start).Seconds())
}

func TestR18ConcurrentDeploys(t *testing.T) {
	r := begin(t, "R18", "Two controllers deploy at once; the lock admits one")
	a := h.deployBG(t, h.release(t, "a", relOpts{fromCurrent: true, mut: withEnv("edge-gateway", "RELEASE_TAG", t.Name())}))
	h.waitStage(t, "rollout", 2*time.Minute)
	res, c := h.deploy(t, h.release(t, "b", relOpts{version: "99.0.0"}))
	if c.Code != 6 || res.Status != "locked" {
		t.Fatalf("second deploy: %s (exit %d), want locked (exit 6)", res.Status, c.Code)
	}
	r.note("second deploy refused: %s", res.Reason)
	resA, code := a.wait(t, 5*time.Minute)
	r.took(resA)
	if code != 0 {
		t.Fatalf("first deploy: %s %s", resA.Status, resA.Reason)
	}
	h.requireConverged(t, resA.Version, time.Minute)
}

// ---- schema -------------------------------------------------------------------

func TestR19MigrationFailsPartWay(t *testing.T) {
	r := begin(t, "R19", "Migration fails half way; release aborts before any service changes, then repair")
	prev := h.status(t).Release
	migs := filepath.Join(h.work, t.Name(), "migrations")
	os.MkdirAll(migs, 0o755)
	ents, _ := filepath.Glob(filepath.Join(h.root, "migrations", "*.sql"))
	for _, f := range ents {
		b, _ := os.ReadFile(f)
		os.WriteFile(filepath.Join(migs, filepath.Base(f)), b, 0o644)
	}
	// First statement succeeds, second fails: MySQL cannot roll back the DDL.
	os.WriteFile(filepath.Join(migs, "0005_create_audit_log.up.sql"),
		[]byte("CREATE TABLE audit_log (id BIGINT PRIMARY KEY);\nALTER TABLE items ADD COLUMN broken INT NOT NULL DEFAULT 'not a number';\n"), 0o644)
	before := map[string]int{}
	for _, s := range []string{"catalog-api", "order-worker", "edge-gateway"} {
		before[s] = h.jobVersion(t, s)
	}
	res, c := h.deploy(t, h.release(t, "bad", relOpts{schemaTarget: 5, migrationsDir: migs, mut: touchAll(t.Name())}))
	r.took(res)
	if c.Code == 0 || res.Status != "aborted" {
		t.Fatalf("want aborted, got %s (exit %d): %s", res.Status, c.Code, res.Reason)
	}
	r.note("aborted: %s", res.Reason)
	for s, v := range before {
		if got := h.jobVersion(t, s); got != v {
			t.Fatalf("%s job version changed %d -> %d", s, v, got)
		}
	}
	r.note("no service job changed")

	// The next release is blocked until the schema is repaired.
	res2, c2 := h.deploy(t, h.release(t, "next", relOpts{}))
	if c2.Code != 3 || res2.Preflight == nil || !slices.Contains(res2.Preflight.FailedIDs(), "PF10") {
		t.Fatalf("want blocked by PF10, got %s (exit %d)", res2.Status, c2.Code)
	}
	r.note("next deploy blocked by %v", res2.Preflight.FailedIDs())

	h.sqlExec(t, "DROP TABLE app.audit_log")
	h.relctlOK(t, "migrate", "repair", "--version", "5", "--drop")
	r.note("operator dropped the half-created table and ran `relctl migrate repair --version 5 --drop`")
	res3, c3 := h.deploy(t, h.release(t, "fixed", relOpts{mut: touchAll(t.Name() + "-fixed")}))
	if c3.Code != 0 {
		t.Fatalf("deploy after repair: %s %s", res3.Status, res3.Reason)
	}
	h.requireConverged(t, res3.Version, time.Minute)
	r.note("%s deployed after repair (previous good release was %s)", res3.Version, prev)
}

// ---- first deploy of a service ----------------------------------------------

func TestR20NewServiceFirstDeployFails(t *testing.T) {
	r := begin(t, "R20", "A brand-new service fails its first deploy; it is removed, the rest untouched")
	prev := h.status(t).Release
	h.relctlOK(t, "bootstrap", "--services", "report-worker")
	t.Cleanup(func() { h.nomad.API.Jobs().Deregister("report-worker", true, nil) })
	res, c := h.deploy(t, h.release(t, "bad", relOpts{fromCurrent: true, mut: func(m *manifest.Manifest) {
		w, _ := m.Service("order-worker")
		w.Name = "report-worker"
		w.Env = map[string]string{"FAULT_MODE": "crash_on_start"}
		m.Services = append(m.Services, w)
	}}))
	h.expectRolledBack(t, r, res, c.Code, prev)
	j, err := h.nomad.Job(context.Background(), "report-worker")
	if err != nil {
		t.Fatal(err)
	}
	if j != nil && (j.Stop == nil || !*j.Stop) {
		t.Fatalf("report-worker still registered and not stopped (status %s)", *j.Status)
	}
	if got := h.rollbackOrder(t, res.ReleaseID); !slices.Equal(got, []string{"report-worker"}) {
		t.Fatalf("rolled back %v, want only report-worker", got)
	}
	r.note("report-worker stopped; no other service touched")
}

// ---- operator rollback ------------------------------------------------------

func TestR21OperatorRollback(t *testing.T) {
	r := begin(t, "R21", "Operator rolls back a release that deployed fine")
	prev := h.status(t).Release
	res, c := h.deploy(t, h.release(t, "good", relOpts{mut: touchAll(t.Name())}))
	if c.Code != 0 {
		t.Fatalf("deploy: %s %s", res.Status, res.Reason)
	}
	h.requireConverged(t, res.Version, time.Minute)
	out := h.relctlOK(t, "rollback", "--json")
	var rb struct {
		Status          string  `json:"status"`
		RollbackSeconds float64 `json:"rollback_seconds"`
		Version         string  `json:"version"`
	}
	if err := json.Unmarshal([]byte(out), &rb); err != nil {
		t.Fatal(err)
	}
	r.s.Outcome = rb.Status
	r.s.Release = rb.Version
	r.s.RollbackSeconds = round(rb.RollbackSeconds)
	if rb.Version != prev || rb.Status != "succeeded" {
		t.Fatalf("rollback went to %s (%s), want %s", rb.Version, rb.Status, prev)
	}
	if rb.RollbackSeconds >= 60 {
		t.Errorf("rollback took %.1fs", rb.RollbackSeconds)
	}
	r.note("all three services back on %s in %.1fs", prev, rb.RollbackSeconds)
	h.requireConverged(t, prev, time.Minute)
}

func TestR22RollbackIdempotent(t *testing.T) {
	r := begin(t, "R22", "Rolling back to the release already running changes nothing")
	cur := h.status(t).Release
	before := map[string]int{}
	for _, s := range []string{"catalog-api", "order-worker", "edge-gateway"} {
		before[s] = h.jobVersion(t, s)
	}
	for i := 0; i < 2; i++ {
		out := h.relctlOK(t, "rollback", "--to", cur, "--json")
		if !strings.Contains(out, `"status": "noop"`) {
			t.Fatalf("rollback --to %s: %s", cur, out)
		}
	}
	for s, v := range before {
		if got := h.jobVersion(t, s); got != v {
			t.Fatalf("%s job version changed %d -> %d", s, v, got)
		}
	}
	r.s.Outcome = "noop"
	r.note("two `rollback --to %s` calls were no-ops; no job version changed", cur)
	h.requireConverged(t, cur, time.Minute)
}
