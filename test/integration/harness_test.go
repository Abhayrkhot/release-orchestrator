//go:build integration

// Package integration runs recovery scenarios against the live cluster from
// the ops box: every scenario breaks something during an upgrade and checks
// the system ends on one consistent, healthy, verified release.
//
// Build and run with `make recovery` (RUN=R07 to pick scenarios).
package integration

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/Abhayrkhot/release-orchestrator/internal/controller"
	"github.com/Abhayrkhot/release-orchestrator/internal/manifest"
	"github.com/Abhayrkhot/release-orchestrator/internal/nomadx"
	"github.com/Abhayrkhot/release-orchestrator/internal/semver"
)

var h *harness

type harness struct {
	root    string // repository root, mounted at /work
	work    string
	arch    string
	relctl  string
	key     string
	trust   string
	nomad   *nomadx.Client
	db      *sql.DB // root connection, for injecting faults and reading evidence
	mu      sync.Mutex
	results []scenario
	started time.Time
}

type scenario struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Passed          bool     `json:"passed"`
	Outcome         string   `json:"outcome"`
	Release         string   `json:"release,omitempty"`
	RollbackSeconds float64  `json:"rollback_seconds,omitempty"`
	RecoverySeconds float64  `json:"recovery_seconds,omitempty"`
	Seconds         float64  `json:"seconds"`
	Notes           []string `json:"notes,omitempty"`
}

func TestMain(m *testing.M) {
	root, _ := os.Getwd()
	for !fileExists(filepath.Join(root, "go.mod")) && root != "/" {
		root = filepath.Dir(root)
	}
	if fileExists("/work/go.mod") {
		root = "/work"
	}
	nc, err := nomadx.New(os.Getenv("NOMAD_ADDR"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	db, err := sql.Open("mysql", fmt.Sprintf("root:%s@tcp(mysql:3306)/?parseTime=true&multiStatements=true&timeout=3s",
		envOr("MYSQL_ROOT_PASSWORD", "rootpw")))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	relctl, _ := exec.LookPath("relctl")
	h = &harness{
		root: root, work: filepath.Join(root, "test/integration/work"), arch: runtime.GOARCH,
		relctl: relctl, key: filepath.Join(root, "keys/release.key"), trust: filepath.Join(root, "keys/trusted"),
		nomad: nc, db: db, started: time.Now(),
	}
	code := m.Run()
	h.writeResults()
	os.Exit(code)
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func (h *harness) writeResults() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.results) == 0 {
		return
	}
	sort.Slice(h.results, func(i, j int) bool { return h.results[i].ID < h.results[j].ID })
	passed := 0
	var rb, rec []float64
	for _, r := range h.results {
		if r.Passed {
			passed++
		}
		if r.RollbackSeconds > 0 {
			rb = append(rb, r.RollbackSeconds)
		}
		if r.RecoverySeconds > 0 {
			rec = append(rec, r.RecoverySeconds)
		}
	}
	out := map[string]any{
		"passed": passed, "total": len(h.results), "ran_at": h.started.UTC().Format(time.RFC3339),
		"rollback_seconds": stats(rb), "recovery_seconds": stats(rec), "scenarios": h.results,
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	name := "results/recovery.json"
	if os.Getenv("RESULTS_NAME") != "" {
		name = "results/" + os.Getenv("RESULTS_NAME")
	}
	_ = os.WriteFile(filepath.Join(h.root, name), append(b, '\n'), 0o644)
	fmt.Printf("\n%d/%d recovery scenarios passed; rollback seconds %v; recovery seconds %v\n", passed, len(h.results), stats(rb), stats(rec))
}

func stats(xs []float64) map[string]float64 {
	if len(xs) == 0 {
		return nil
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	pct := func(p float64) float64 {
		i := int(p*float64(len(s)-1) + 0.5)
		return round(s[i])
	}
	return map[string]float64{"n": float64(len(s)), "min": round(s[0]), "p50": pct(0.5), "p95": pct(0.95), "max": round(s[len(s)-1])}
}

func round(f float64) float64 { return float64(int(f*10+0.5)) / 10 }

// rec is the per-scenario record; finish() stores it when the test ends.
type rec struct {
	t  *testing.T
	s  scenario
	t0 time.Time
}

func begin(t *testing.T, id, name string) *rec {
	t.Helper()
	t.Logf("=== %s %s", id, name)
	r := &rec{t: t, s: scenario{ID: id, Name: name}, t0: time.Now()}
	t.Cleanup(func() {
		r.s.Passed = !t.Failed()
		r.s.Seconds = round(time.Since(r.t0).Seconds())
		h.mu.Lock()
		h.results = append(h.results, r.s)
		h.mu.Unlock()
	})
	h.ensureBaseline(t)
	return r
}

func (r *rec) note(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	r.t.Log(msg)
	r.s.Notes = append(r.s.Notes, msg)
}

func (r *rec) took(res *controller.Result) {
	r.s.Outcome = res.Status
	r.s.Release = res.Version
	if res.RollbackSeconds > 0 {
		r.s.RollbackSeconds = round(res.RollbackSeconds)
	}
	if res.RecoverySeconds > 0 {
		r.s.RecoverySeconds = round(res.RecoverySeconds)
	}
}

// ---- running relctl ----

type cmdResult struct {
	Stdout, Stderr string
	Code           int
}

func (h *harness) run(ctx context.Context, args ...string) cmdResult {
	cmd := exec.CommandContext(ctx, h.relctl, args...)
	cmd.Dir = h.root
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		code = -1
		e.WriteString(err.Error())
	}
	return cmdResult{o.String(), e.String(), code}
}

func (h *harness) relctlOK(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	r := h.run(ctx, args...)
	if r.Code != 0 {
		t.Fatalf("relctl %s: exit %d\n%s", strings.Join(args, " "), r.Code, tail(r.Stderr, 30))
	}
	return r.Stdout
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// deploy runs `relctl deploy --json` and returns the parsed result.
func (h *harness) deploy(t *testing.T, bundle string, extra ...string) (*controller.Result, cmdResult) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	r := h.run(ctx, append([]string{"deploy", "--json", "--bundle", bundle}, extra...)...)
	t.Logf("deploy %s: exit %d\n%s", filepath.Base(bundle), r.Code, tail(r.Stderr, 25))
	var res controller.Result
	if err := json.Unmarshal([]byte(r.Stdout), &res); err != nil {
		t.Fatalf("deploy output is not a result: %v\n%s", err, r.Stdout)
	}
	return &res, r
}

type bg struct {
	cmd  *exec.Cmd
	out  bytes.Buffer
	errb bytes.Buffer
	done chan struct{}
	err  error
}

// deployBG starts a deploy and returns at once.
func (h *harness) deployBG(t *testing.T, bundle string) *bg {
	t.Helper()
	b := &bg{done: make(chan struct{})}
	b.cmd = exec.Command(h.relctl, "deploy", "--json", "--bundle", bundle)
	b.cmd.Dir = h.root
	b.cmd.Stdout, b.cmd.Stderr = &b.out, &b.errb
	if err := b.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { b.err = b.cmd.Wait(); close(b.done) }()
	return b
}

func (b *bg) wait(t *testing.T, limit time.Duration) (*controller.Result, int) {
	t.Helper()
	select {
	case <-b.done:
	case <-time.After(limit):
		_ = b.cmd.Process.Kill()
		t.Fatalf("background deploy still running after %s", limit)
	}
	t.Logf("background deploy:\n%s", tail(b.errb.String(), 25))
	code := 0
	if ee, ok := b.err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	var res controller.Result
	_ = json.Unmarshal(b.out.Bytes(), &res)
	return &res, code
}

// kill sends SIGKILL: the controller gets no chance to clean up.
func (b *bg) kill(t *testing.T) {
	t.Helper()
	_ = b.cmd.Process.Kill()
	<-b.done
	t.Logf("controller killed; its log:\n%s", tail(b.errb.String(), 15))
}

// waitStage waits until the in-progress release reaches a stage starting
// with prefix (for example "rollout:catalog-api" or "rollback").
func (h *harness) waitStage(t *testing.T, prefix string, limit time.Duration) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		var stage string
		err := h.db.QueryRow(`SELECT stage FROM relctl.releases WHERE status = 'in_progress' ORDER BY id DESC LIMIT 1`).Scan(&stage)
		if err == nil && strings.HasPrefix(stage, prefix) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("release never reached stage %q", prefix)
}

// ---- building releases ----

type status struct {
	Release  string `json:"release"`
	Schema   int    `json:"schema"`
	Dirty    bool   `json:"dirty"`
	Lock     string `json:"lock"`
	Services []struct {
		Name      string            `json:"name"`
		Want      string            `json:"want_version"`
		Endpoints []nomadx.Endpoint `json:"endpoints"`
		Healthy   bool              `json:"healthy"`
		Detail    string            `json:"detail"`
	} `json:"services"`
}

func (h *harness) status(t *testing.T) *status {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	r := h.run(ctx, "status", "--json")
	var s status
	if r.Code != 0 || json.Unmarshal([]byte(r.Stdout), &s) != nil {
		t.Fatalf("relctl status: exit %d %s", r.Code, r.Stderr)
	}
	return &s
}

// nextVersion is one patch above the running release.
func (h *harness) nextVersion(t *testing.T) string {
	cur := h.status(t).Release
	v, err := semver.Parse(cur)
	if err != nil {
		t.Fatalf("running release %q: %v", cur, err)
	}
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch+1)
}

type relOpts struct {
	version       string
	svcVersion    string // image version of every service (default 1.1.0)
	set           map[string]string
	schemaTarget  int
	migrationsDir string
	specMut       func(*manifest.Spec)
	// fromCurrent starts from the running release's services instead of the
	// spec, so only what mut changes differs from what is running.
	fromCurrent bool
	mut         func(*manifest.Manifest)
}

// currentManifest is the manifest of the running release.
func (h *harness) currentManifest(t *testing.T) *manifest.Manifest {
	t.Helper()
	var raw []byte
	if err := h.db.QueryRow(`SELECT manifest FROM relctl.releases WHERE status = 'succeeded' ORDER BY id DESC LIMIT 1`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// release builds and signs a bundle; opts.mut edits the manifest before signing.
func (h *harness) release(t *testing.T, name string, o relOpts) string {
	t.Helper()
	if o.version == "" {
		o.version = h.nextVersion(t)
	}
	if o.svcVersion == "" {
		o.svcVersion = "1.1.0"
	}
	if o.migrationsDir == "" {
		o.migrationsDir = filepath.Join(h.root, "migrations")
	}
	if o.schemaTarget == 0 {
		o.schemaTarget = 4
	}
	spec, err := manifest.LoadSpec(filepath.Join(h.root, "releases/spec.json"))
	if err != nil {
		t.Fatal(err)
	}
	if o.specMut != nil {
		o.specMut(spec)
	}
	dir := filepath.Join(h.work, t.Name(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := manifest.CleanBundle(dir); err != nil {
		t.Fatal(err)
	}
	sv := map[string]string{"": o.svcVersion}
	for k, v := range o.set {
		sv[k] = v
	}
	m, err := manifest.CreateBundle(manifest.CreateOptions{
		Version: o.version, Spec: spec, ServiceVersions: sv, ImagesDir: filepath.Join(h.root, "dist/images"),
		Arch: h.arch, MigrationsDir: o.migrationsDir, SchemaTarget: o.schemaTarget, CreatedAt: time.Now(), OutDir: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if o.fromCurrent {
		m.Services = h.currentManifest(t).Services
	}
	if o.mut != nil {
		o.mut(m)
	}
	if o.fromCurrent || o.mut != nil {
		if err := manifest.WriteManifest(dir, m); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := manifest.SignBundle(dir, h.key); err != nil {
		t.Fatal(err)
	}
	return dir
}

// withEnv sets env vars on one service.
func withEnv(svc string, kv ...string) func(*manifest.Manifest) {
	return func(m *manifest.Manifest) {
		for i := range m.Services {
			if m.Services[i].Name != svc {
				continue
			}
			if m.Services[i].Env == nil {
				m.Services[i].Env = map[string]string{}
			}
			for j := 0; j+1 < len(kv); j += 2 {
				m.Services[i].Env[kv[j]] = kv[j+1]
			}
		}
	}
}

func chain(fs ...func(*manifest.Manifest)) func(*manifest.Manifest) {
	return func(m *manifest.Manifest) {
		for _, f := range fs {
			f(m)
		}
	}
}

// touchAll changes every service's spec so the release rolls all three.
func touchAll(tag string) func(*manifest.Manifest) {
	return func(m *manifest.Manifest) {
		for _, s := range []string{"catalog-api", "order-worker", "edge-gateway"} {
			withEnv(s, "RELEASE_TAG", tag)(m)
		}
	}
}

// ---- cluster control ----

func docker(t *testing.T, args ...string) {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Logf("docker %s: %v %s", strings.Join(args, " "), err, out)
	}
}

func (h *harness) sqlExec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := h.db.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func (h *harness) nodeID(t *testing.T, name string) string {
	t.Helper()
	nodes, _, err := h.nomad.API.Nodes().List(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.Name == name && n.Status != "down" {
			return n.ID
		}
	}
	t.Fatalf("node %s not found", name)
	return ""
}

func waitFor(t *testing.T, what string, limit time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("timed out after %s waiting for %s", limit, what)
}

func (h *harness) clusterUp(t *testing.T) {
	t.Helper()
	docker(t, "start", "relorch-mysql", "relorch-registry", "relorch-nomad-server", "relorch-node1", "relorch-node2", "relorch-node3")
	waitFor(t, "mysql", 2*time.Minute, func() bool { return h.db.Ping() == nil })
	waitFor(t, "3 ready nodes", 3*time.Minute, func() bool {
		nodes, _, err := h.nomad.API.Nodes().List(nil)
		if err != nil {
			return false
		}
		ready := 0
		for _, n := range nodes {
			if n.Status != "ready" {
				continue
			}
			if n.SchedulingEligibility != "eligible" || n.Drain {
				_, _ = h.nomad.API.Nodes().UpdateDrain(n.ID, nil, false, nil)
				_, _ = h.nomad.API.Nodes().ToggleEligibility(n.ID, true, nil)
				continue
			}
			ready++
		}
		return ready >= 3
	})
	waitFor(t, "registry", time.Minute, func() bool {
		return exec.Command("curl", "-fsS", "-o", "/dev/null", "http://registry:5000/v2/").Run() == nil
	})
}

// healAPI clears runtime faults injected through the admin endpoint.
func (h *harness) healAPI(t *testing.T) {
	eps, _ := h.nomad.Endpoints(context.Background(), "catalog-api")
	for _, e := range eps {
		_ = exec.Command("curl", "-fsS", "-m", "2", "-X", "POST", "http://"+e.Addr+"/admin/fault?mode=healthy").Run()
	}
}

// converged: the running release is healthy everywhere and every allocation
// runs exactly the binary its signed manifest names.
func (h *harness) converged(t *testing.T) (bool, string) {
	t.Helper()
	s := h.status(t)
	if s.Release == "" {
		return false, "nothing deployed"
	}
	if s.Lock != "" {
		return false, "lock held by " + s.Lock
	}
	for _, svc := range s.Services {
		if !svc.Healthy {
			return false, svc.Name + ": " + svc.Detail
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	r := h.run(ctx, "verify-runtime")
	if r.Code != 0 {
		return false, "verify-runtime: " + tail(r.Stderr, 3)
	}
	var n int
	_ = h.db.QueryRow(`SELECT COUNT(*) FROM relctl.releases WHERE status = 'in_progress'`).Scan(&n)
	if n > 0 {
		return false, "release in progress"
	}
	return true, ""
}

// requireConverged waits for convergence on release want.
func (h *harness) requireConverged(t *testing.T, want string, limit time.Duration) {
	t.Helper()
	var why string
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		var ok bool
		ok, why = h.converged(t)
		if ok {
			if got := h.status(t).Release; want != "" && got != want {
				t.Fatalf("converged on release %s, want %s", got, want)
			}
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("not converged on %s after %s: %s", want, limit, why)
}

// ensureBaseline puts the cluster back to a healthy, verified release before
// a scenario: infrastructure up, no interrupted release, no stray faults.
func (h *harness) ensureBaseline(t *testing.T) {
	t.Helper()
	h.clusterUp(t)
	h.healAPI(t)
	if ok, why := h.converged(t); ok {
		return
	} else {
		t.Logf("baseline not converged (%s); reconciling", why)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for i := 0; i < 20; i++ { // a dead controller's lease expires after its TTL
		r := h.run(ctx, "reconcile")
		if r.Code != 6 {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if ok, _ := h.converged(t); ok {
		return
	}
	t.Log("still not converged; deploying a fresh good release")
	res, r := h.deploy(t, h.release(t, "baseline", relOpts{mut: touchAll(fmt.Sprint(time.Now().UnixNano()))}))
	if r.Code != 0 {
		t.Fatalf("baseline deploy %s: %s", res.Status, res.Reason)
	}
	h.requireConverged(t, res.Version, 2*time.Minute)
}

// jobVersion returns a job's current version, or -1 if it does not exist.
func (h *harness) jobVersion(t *testing.T, id string) int {
	j, err := h.nomad.Job(context.Background(), id)
	if err != nil || j == nil {
		return -1
	}
	return int(*j.Version)
}

func (h *harness) events(t *testing.T, releaseID int64) []struct {
	At      time.Time `json:"at"`
	Service string    `json:"service"`
	Kind    string    `json:"kind"`
	Detail  string    `json:"detail"`
} {
	t.Helper()
	out := h.relctlOK(t, "events", "--json", "--release", fmt.Sprint(releaseID))
	var evs []struct {
		At      time.Time `json:"at"`
		Service string    `json:"service"`
		Kind    string    `json:"kind"`
		Detail  string    `json:"detail"`
	}
	if err := json.Unmarshal([]byte(out), &evs); err != nil {
		t.Fatal(err)
	}
	return evs
}

// rollbackOrder lists services in the order the controller rolled them back.
func (h *harness) rollbackOrder(t *testing.T, releaseID int64) []string {
	var out []string
	for _, e := range h.events(t, releaseID) {
		if e.Kind == "rollback" || e.Kind == "stop" {
			out = append(out, e.Service)
		}
	}
	return out
}

// expectRolledBack checks a failed release left the previous one in place.
func (h *harness) expectRolledBack(t *testing.T, r *rec, res *controller.Result, code int, prev string) {
	t.Helper()
	r.took(res)
	if code != 4 || res.Status != "rolled_back" {
		t.Fatalf("want rolled_back (exit 4), got %s (exit %d): %s", res.Status, code, res.Reason)
	}
	r.note("failure: %s", res.Reason)
	r.note("rollback %.1fs after detection, %.1fs after the bad rollout began", res.RollbackSeconds, res.RecoverySeconds)
	if res.RecoverySeconds >= 60 {
		t.Errorf("recovery took %.1fs, want < 60s", res.RecoverySeconds)
	}
	h.requireConverged(t, prev, 90*time.Second)
}
