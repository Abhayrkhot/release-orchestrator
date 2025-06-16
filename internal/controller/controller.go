// Package controller runs a release: lock, preflight, verify, migrate, roll
// services out in dependency order, and on any failure put every touched
// service back on the previous signed release in reverse order.
package controller

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/hashicorp/nomad/api"

	"github.com/Abhayrkhot/release-orchestrator/internal/manifest"
	"github.com/Abhayrkhot/release-orchestrator/internal/migrate"
	"github.com/Abhayrkhot/release-orchestrator/internal/nomadx"
	"github.com/Abhayrkhot/release-orchestrator/internal/semver"
	"github.com/Abhayrkhot/release-orchestrator/internal/store"
	"github.com/Abhayrkhot/release-orchestrator/preflight"
)

type Controller struct {
	State          *store.Store
	AppDB          *sql.DB
	Nomad          *nomadx.Client
	TrustDir       string
	Preflight      *preflight.Runner
	PreflightEnv   map[string]string
	Owner          string
	LockTTL        time.Duration
	ServiceTimeout time.Duration
	Timing         nomadx.Timing
	Log            io.Writer
}

var (
	ErrPreflight = errors.New("preflight failed")
	ErrRejected  = errors.New("release rejected")
)

type Timings struct {
	Started         time.Time `json:"started"`
	PreflightDone   time.Time `json:"preflight_done,omitzero"`
	MigrationsDone  time.Time `json:"migrations_done,omitzero"`
	FailureStart    time.Time `json:"failure_rollout_started,omitzero"` // the failing service's rollout began
	FailureDetected time.Time `json:"failure_detected,omitzero"`
	RollbackDone    time.Time `json:"rollback_done,omitzero"`
	Finished        time.Time `json:"finished"`
}

// RollbackSeconds is failure detected -> previous release healthy again.
func (t Timings) RollbackSeconds() float64 {
	if t.FailureDetected.IsZero() || t.RollbackDone.IsZero() {
		return 0
	}
	return t.RollbackDone.Sub(t.FailureDetected).Seconds()
}

// RecoverySeconds is bad version submitted -> previous release healthy again.
func (t Timings) RecoverySeconds() float64 {
	if t.FailureStart.IsZero() || t.RollbackDone.IsZero() {
		return 0
	}
	return t.RollbackDone.Sub(t.FailureStart).Seconds()
}

type Result struct {
	ReleaseID       int64                `json:"release_id,omitempty"`
	Kind            string               `json:"kind"`
	Version         string               `json:"version"`
	Previous        string               `json:"previous,omitempty"`
	Status          string               `json:"status"`
	Reason          string               `json:"reason,omitempty"`
	FailedService   string               `json:"failed_service,omitempty"`
	Changed         []string             `json:"changed,omitempty"`
	RolledBack      []string             `json:"rolled_back,omitempty"`
	Migrations      *migrate.Result      `json:"migrations,omitempty"`
	Preflight       *preflight.Report    `json:"preflight,omitempty"`
	Verified        []nomadx.VersionInfo `json:"verified,omitempty"`
	Timings         Timings              `json:"timings"`
	RollbackSeconds float64              `json:"rollback_seconds,omitempty"`
	RecoverySeconds float64              `json:"recovery_seconds,omitempty"`
}

func (c *Controller) logf(format string, a ...any) {
	if c.Log != nil {
		fmt.Fprintf(c.Log, "%s  "+format+"\n", append([]any{time.Now().Format("15:04:05.000")}, a...)...)
	}
}

// lease holds the deploy lock and renews it until released. If renewal
// fails the context is cancelled so the rollout stops instead of racing
// another controller.
type lease struct {
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

func (c *Controller) acquire(ctx context.Context) (context.Context, *lease, error) {
	if err := c.State.Acquire(ctx, c.Owner, c.LockTTL); err != nil {
		return nil, nil, err
	}
	lctx, cancel := context.WithCancel(ctx)
	l := &lease{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(l.done)
		t := time.NewTicker(c.LockTTL / 3)
		defer t.Stop()
		for {
			select {
			case <-lctx.Done():
				return
			case <-t.C:
				hctx, hc := context.WithTimeout(context.Background(), 5*time.Second)
				err := c.State.Heartbeat(hctx, c.Owner)
				hc()
				if err != nil && strings.Contains(err.Error(), "lost") {
					l.err = err
					cancel()
					return
				}
				// A database blip is tolerated until the TTL runs out.
			}
		}
	}()
	return lctx, l, nil
}

func (c *Controller) release(l *lease) {
	l.cancel()
	<-l.done
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = c.State.Release(ctx, c.Owner)
}

type DeployOptions struct {
	SkipPreflight  bool
	AllowDowngrade bool
}

func (c *Controller) event(ctx context.Context, id int64, svc, kind, format string, a ...any) {
	detail := fmt.Sprintf(format, a...)
	c.logf("%-10s %-14s %s", kind, svc, detail)
	if id == 0 {
		return
	}
	// Events are best effort; use a fresh context so they are written even
	// when the rollout context was cancelled.
	ectx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.State.AddEvent(ectx, id, svc, kind, detail)
}

// currentManifest loads and re-verifies the manifest of the current release.
func (c *Controller) currentManifest(ctx context.Context) (*store.Release, *manifest.Manifest, error) {
	cur, err := c.State.Current(ctx)
	if err != nil || cur == nil {
		return nil, nil, err
	}
	m, err := c.storedManifest(cur)
	return cur, m, err
}

func (c *Controller) storedManifest(r *store.Release) (*manifest.Manifest, error) {
	if _, err := manifest.Verify(r.Manifest, r.Signature, c.TrustDir); err != nil {
		return nil, fmt.Errorf("stored manifest for release %s (id %d): %w", r.Version, r.ID, err)
	}
	return manifest.Parse(r.Manifest)
}

func (c *Controller) Deploy(ctx context.Context, bundleDir string, o DeployOptions) (*Result, error) {
	res := &Result{Kind: store.KindDeploy, Timings: Timings{Started: time.Now()}}
	defer func() { res.Timings.Finished = time.Now() }()

	lctx, l, err := c.acquire(ctx)
	if err != nil {
		if errors.Is(err, store.ErrLocked) {
			res.Status, res.Reason = "locked", err.Error()
			return res, err
		}
		// The state database is unreachable. Nothing can be deployed, but
		// run the checks anyway so the operator sees the cause, not just a
		// lock error.
		res.Status, res.Reason = "error", "cannot take the deploy lock: "+err.Error()
		if o.SkipPreflight {
			return res, err
		}
		rep, perr := c.runPreflight(ctx, bundleDir, nil, o, true)
		res.Preflight = rep
		if perr == nil && !rep.Passed {
			res.Status = "blocked"
			res.Reason = "preflight failed: " + strings.Join(rep.FailedIDs(), ", ")
			return res, fmt.Errorf("%w: %s", ErrPreflight, strings.Join(rep.FailedIDs(), ", "))
		}
		return res, err
	}
	ctx = lctx
	defer c.release(l)

	cur, curM, err := c.currentManifest(ctx)
	if err != nil {
		return res, err
	}
	if cur != nil {
		res.Previous = cur.Version
	}

	if !o.SkipPreflight {
		rep, err := c.runPreflight(ctx, bundleDir, cur, o, false)
		res.Preflight = rep
		res.Timings.PreflightDone = time.Now()
		if err != nil {
			res.Status = "error"
			res.Reason = err.Error()
			return res, err
		}
		if !rep.Passed {
			res.Status = "blocked"
			res.Reason = "preflight failed: " + strings.Join(rep.FailedIDs(), ", ")
			return res, fmt.Errorf("%w: %s", ErrPreflight, strings.Join(rep.FailedIDs(), ", "))
		}
	}

	// The Bash checks already verified the signature; verify again in Go
	// with the bytes that will actually be used.
	b, err := manifest.OpenBundle(bundleDir, c.TrustDir)
	if err != nil {
		res.Status = "blocked"
		res.Reason = err.Error()
		return res, fmt.Errorf("%w: %v", ErrRejected, err)
	}
	m := b.Manifest
	res.Version = m.Version
	if cur != nil && !o.AllowDowngrade {
		if cmp, err := semver.Compare(m.Version, cur.Version); err == nil && cmp <= 0 {
			res.Status = "blocked"
			res.Reason = fmt.Sprintf("release %s is not newer than current %s", m.Version, cur.Version)
			return res, fmt.Errorf("%w: %s", ErrRejected, res.Reason)
		}
	}

	id, err := c.State.Begin(ctx, store.KindDeploy, m.Version, manifest.SHA256Bytes(b.Raw), b.Raw, b.Signature, b.KeyID, cur)
	if err != nil {
		return res, err
	}
	res.ReleaseID = id
	_ = c.State.AttachLock(ctx, c.Owner, id)
	c.event(ctx, id, "", "start", "release %s (previous %s), signed by %s", m.Version, res.Previous, b.KeyID)

	finish := func(status, reason string) {
		res.Status, res.Reason = status, reason
		fctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = c.State.Finish(fctx, id, status, reason)
		c.event(fctx, id, "", status, "%s", reason)
	}

	// Schema first. Migrations are expand-only, so the previous release keeps
	// working on the new schema (preflight PF14 checks that) and a service
	// rollback never needs a schema rollback.
	_ = c.State.SetStage(ctx, id, "migrate")
	mig := &migrate.Migrator{DB: c.AppDB}
	mr, err := mig.Up(ctx, b)
	res.Migrations = mr
	if err != nil {
		finish(store.StatusAborted, "migration: "+err.Error())
		return res, err
	}
	if fp, err := mig.Fingerprint(ctx); err == nil {
		_ = c.State.SaveSnapshot(ctx, m.Schema.Target, fp, id)
	}
	res.Timings.MigrationsDone = time.Now()
	c.event(ctx, id, "", "migrated", "schema %d -> %d", mr.From, mr.To)

	order, _ := m.Order()
	var changed []manifest.Service
	for _, svc := range order {
		job, err := c.Nomad.Job(ctx, svc.Name)
		if err != nil {
			finish(store.StatusAborted, err.Error())
			return res, err
		}
		if nomadx.UpToDate(job, svc) {
			c.event(ctx, id, svc.Name, "unchanged", "v%s already running", svc.Version)
			continue
		}
		changed = append(changed, svc)
		res.Changed = append(res.Changed, svc.Name)
	}

	var touched []string
	var failure error
	for _, svc := range changed {
		_ = c.State.SetStage(ctx, id, "rollout:"+svc.Name)
		c.event(ctx, id, svc.Name, "rollout", "v%s %s x%d", svc.Version, short(svc.Digest), svc.Count)
		started := time.Now()
		touched = append(touched, svc.Name)
		ver, err := c.register(ctx, nomadx.Render(svc, nomadx.RenderOptions{ReleaseVersion: m.Version, Timing: c.Timing}))
		if err != nil {
			res.Timings.FailureStart, res.Timings.FailureDetected = started, time.Now()
			failure = err
			res.FailedService = svc.Name
			c.event(ctx, id, svc.Name, "failed", "register: %v", err)
			break
		}
		out := c.Nomad.WaitHealthy(ctx, svc.Name, ver, c.ServiceTimeout)
		if !out.Healthy {
			res.Timings.FailureStart, res.Timings.FailureDetected = started, time.Now()
			c.Nomad.FailDeployment(context.WithoutCancel(ctx), out.DeploymentID)
			failure = errors.New(out.Reason)
			res.FailedService = svc.Name
			c.event(ctx, id, svc.Name, "failed", "%s (after %s)", out.Reason, out.Elapsed.Round(time.Millisecond))
			break
		}
		c.event(ctx, id, svc.Name, "healthy", "job version %d in %s", ver, out.Elapsed.Round(time.Millisecond))
	}

	if failure == nil {
		_ = c.State.SetStage(ctx, id, "verify")
		vers, err := c.VerifyRuntime(ctx, m, res.Changed)
		res.Verified = vers
		if err != nil {
			res.Timings.FailureStart, res.Timings.FailureDetected = res.Timings.MigrationsDone, time.Now()
			failure = fmt.Errorf("runtime verification: %w", err)
			c.event(ctx, id, "", "failed", "%v", failure)
		} else {
			c.event(ctx, id, "", "verified", "%d allocations run the signed binaries", len(vers))
		}
	}

	if failure == nil {
		finish(store.StatusSucceeded, "")
		return res, nil
	}

	// Roll back everything this release touched, newest first.
	rctx := context.WithoutCancel(ctx) // finish the rollback even if the caller gave up
	_ = c.State.SetStage(rctx, id, "rollback")
	slices.Reverse(touched)
	rbErr := c.restore(rctx, id, touched, curM)
	res.RolledBack = touched
	res.Timings.RollbackDone = time.Now()
	res.RollbackSeconds = res.Timings.RollbackSeconds()
	res.RecoverySeconds = res.Timings.RecoverySeconds()
	if rbErr != nil {
		finish(store.StatusFailed, fmt.Sprintf("%v; rollback failed: %v", failure, rbErr))
		return res, fmt.Errorf("release failed and rollback failed: %w", rbErr)
	}
	c.event(rctx, id, "", "restored", "previous release %s healthy %.1fs after failure was detected", res.Previous, res.RollbackSeconds)
	finish(store.StatusRolledBack, failure.Error())
	return res, fmt.Errorf("release %s rolled back: %w", m.Version, failure)
}

// register bounds how long a registration may wait for an unreachable
// Nomad server.
func (c *Controller) register(ctx context.Context, job *api.Job) (uint64, error) {
	rctx, cancel := context.WithTimeout(ctx, c.ServiceTimeout)
	defer cancel()
	return c.Nomad.Register(rctx, job)
}

// restore puts each named service back on its spec in prev (in the order
// given), or stops it if prev does not have it. Every bad allocation is
// replaced at once rather than one at a time.
func (c *Controller) restore(ctx context.Context, id int64, services []string, prev *manifest.Manifest) error {
	var errs []error
	for _, name := range services {
		var svc manifest.Service
		ok := false
		if prev != nil {
			svc, ok = prev.Service(name)
		}
		if !ok {
			c.event(ctx, id, name, "stop", "no previous version; stopping")
			if err := c.Nomad.Stop(ctx, name, c.ServiceTimeout); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", name, err))
			}
			continue
		}
		start := time.Now()
		c.event(ctx, id, name, "rollback", "to v%s (release %s)", svc.Version, prev.Version)
		before, _ := c.Nomad.Job(ctx, name)
		ver, err := c.register(ctx, nomadx.Render(svc, nomadx.RenderOptions{
			ReleaseVersion: prev.Version, MaxParallel: svc.Count, Timing: c.Timing,
		}))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		var out nomadx.Outcome
		if before != nil && before.Version != nil && *before.Version == ver {
			// The job already had this spec, so Nomad made no new version and
			// will not replace allocations that died. Make it.
			if err := c.Nomad.ForceReschedule(ctx, name); err != nil {
				errs = append(errs, fmt.Errorf("%s: force reschedule: %w", name, err))
				continue
			}
			out = c.Nomad.WaitSteady(ctx, name, svc.Count, c.ServiceTimeout)
		} else {
			out = c.Nomad.WaitHealthy(ctx, name, ver, c.ServiceTimeout)
		}
		if !out.Healthy {
			errs = append(errs, fmt.Errorf("%s: %s", name, out.Reason))
			c.event(ctx, id, name, "rollback-failed", "%s", out.Reason)
			continue
		}
		c.event(ctx, id, name, "restored", "v%s healthy in %s", svc.Version, time.Since(start).Round(time.Millisecond))
	}
	return errors.Join(errs...)
}

// VerifyRuntime asks every running allocation of the named services (all
// services if names is empty) which binary it runs, and compares the answer
// with the signed manifest. This is what makes "the same artifact on every
// node" a checked fact rather than an assumption.
func (c *Controller) VerifyRuntime(ctx context.Context, m *manifest.Manifest, names []string) ([]nomadx.VersionInfo, error) {
	var out []nomadx.VersionInfo
	var errs []error
	for _, svc := range m.Services {
		if len(names) > 0 && !slices.Contains(names, svc.Name) {
			continue
		}
		eps, err := c.Nomad.Endpoints(ctx, svc.Name)
		if err != nil {
			return out, err
		}
		if len(eps) != svc.Count {
			errs = append(errs, fmt.Errorf("%s: %d running allocations, want %d", svc.Name, len(eps), svc.Count))
		}
		for _, ep := range eps {
			v, err := c.Nomad.FetchVersion(ctx, ep.Addr)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s on %s: %w", svc.Name, ep.Node, err))
				continue
			}
			out = append(out, *v)
			if v.Version != svc.Version {
				errs = append(errs, fmt.Errorf("%s on %s reports version %s, manifest says %s", svc.Name, ep.Node, v.Version, svc.Version))
			}
			if v.BinarySHA256 != svc.BinarySHA256 {
				errs = append(errs, fmt.Errorf("%s on %s runs binary %s, manifest says %s", svc.Name, ep.Node, short(v.BinarySHA256), short(svc.BinarySHA256)))
			}
		}
	}
	return out, errors.Join(errs...)
}

// runPreflight runs the Bash checks. currentUnknown says the state database
// could not be read, so checks that compare with the running release skip.
func (c *Controller) runPreflight(ctx context.Context, bundleDir string, cur *store.Release, o DeployOptions, currentUnknown bool) (*preflight.Report, error) {
	env := map[string]string{}
	for k, v := range c.PreflightEnv {
		env[k] = v
	}
	abs, _ := filepath.Abs(bundleDir)
	env["BUNDLE_DIR"] = abs
	env["MANIFEST"] = filepath.Join(abs, manifest.ManifestFile)
	env["SIGNATURE"] = filepath.Join(abs, manifest.SignatureFile)
	env["TRUST_DIR"] = c.TrustDir
	env["LOCK_OWNER"] = c.Owner
	if o.AllowDowngrade {
		env["ALLOW_DOWNGRADE"] = "1"
	}
	if currentUnknown {
		env["CURRENT_UNKNOWN"] = "1"
	}
	if cur != nil {
		f, err := os.CreateTemp("", "current-manifest-*.json")
		if err != nil {
			return nil, err
		}
		defer os.Remove(f.Name())
		if _, err := f.Write(cur.Manifest); err != nil {
			return nil, err
		}
		f.Close()
		env["CURRENT_MANIFEST"] = f.Name()
		env["CURRENT_VERSION"] = cur.Version
	}
	return c.Preflight.Run(ctx, env)
}

func short(s string) string {
	s = strings.TrimPrefix(s, "sha256:")
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// PreflightOnly runs the checks against bundleDir without taking the lock
// or changing anything.
func (c *Controller) PreflightOnly(ctx context.Context, bundleDir string, cur *store.Release, unknown bool) (*preflight.Report, error) {
	return c.runPreflight(ctx, bundleDir, cur, DeployOptions{}, cur == nil && unknown)
}
