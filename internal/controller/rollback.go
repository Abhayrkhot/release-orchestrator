package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/Abhayrkhot/release-orchestrator/internal/manifest"
	"github.com/Abhayrkhot/release-orchestrator/internal/migrate"
	"github.com/Abhayrkhot/release-orchestrator/internal/nomadx"
	"github.com/Abhayrkhot/release-orchestrator/internal/store"
)

// Rollback returns the cluster to an earlier succeeded release: the one
// before the current release, or the newest succeeded release with version
// `to`. Rolling back to the release that is already current is a no-op.
func (c *Controller) Rollback(ctx context.Context, to string) (*Result, error) {
	res := &Result{Kind: store.KindRollback, Timings: Timings{Started: time.Now()}}
	defer func() { res.Timings.Finished = time.Now() }()

	ctx, l, err := c.acquire(ctx)
	if err != nil {
		res.Status, res.Reason = "locked", err.Error()
		return res, err
	}
	defer c.release(l)

	if inprog, err := c.State.InProgress(ctx); err != nil {
		return res, err
	} else if len(inprog) > 0 {
		res.Status = "blocked"
		res.Reason = fmt.Sprintf("release %s (id %d) was interrupted; run `relctl reconcile` first", inprog[0].Version, inprog[0].ID)
		return res, errors.New(res.Reason)
	}

	cur, curM, err := c.currentManifest(ctx)
	if err != nil {
		return res, err
	}
	if cur == nil {
		res.Status, res.Reason = "blocked", "nothing is deployed"
		return res, errors.New(res.Reason)
	}
	res.Previous = cur.Version

	var target *store.Release
	if to == "" {
		target, err = c.State.SucceededBefore(ctx, cur.ID, cur.Version)
	} else {
		target, err = c.State.LatestSucceededVersion(ctx, to)
	}
	if err != nil {
		return res, err
	}
	if target == nil {
		res.Status, res.Reason = "blocked", "no earlier succeeded release to roll back to"
		if to != "" {
			res.Reason = fmt.Sprintf("release %s never succeeded here", to)
		}
		return res, errors.New(res.Reason)
	}
	res.Version = target.Version
	if target.Version == cur.Version {
		res.Status, res.Reason = "noop", "release "+cur.Version+" is already current"
		return res, nil
	}
	tm, err := c.storedManifest(target)
	if err != nil {
		res.Status, res.Reason = "blocked", err.Error()
		return res, err
	}
	// The schema only moves forward, so the target's services must accept
	// the schema the database has now.
	schema, dirty, err := (&migrate.Migrator{DB: c.AppDB}).Current(ctx)
	if err != nil {
		return res, err
	}
	if dirty {
		res.Status, res.Reason = "blocked", "schema is dirty"
		return res, errors.New(res.Reason)
	}
	for _, s := range tm.Services {
		if schema < s.MinSchema || schema > s.MaxSchema {
			res.Status = "blocked"
			res.Reason = fmt.Sprintf("%s v%s supports schema %d-%d, database is at %d", s.Name, s.Version, s.MinSchema, s.MaxSchema, schema)
			return res, errors.New(res.Reason)
		}
	}

	id, err := c.State.Begin(ctx, store.KindRollback, target.Version, target.ManifestSHA256, target.Manifest, target.Signature, target.KeyID, cur)
	if err != nil {
		return res, err
	}
	res.ReleaseID = id
	_ = c.State.AttachLock(ctx, c.Owner, id)
	c.event(ctx, id, "", "start", "rollback %s -> %s", cur.Version, target.Version)
	res.Timings.FailureDetected = time.Now() // an operator rollback starts the clock here

	// Reverse dependency order of the release being left, then anything the
	// target has that the current release does not.
	var names []string
	curOrder, _ := curM.Order()
	for _, s := range curOrder {
		names = append(names, s.Name)
	}
	slices.Reverse(names)
	tOrder, _ := tm.Order()
	for _, s := range tOrder {
		if !slices.Contains(names, s.Name) {
			names = append(names, s.Name)
		}
	}
	var todo []string
	for _, n := range names {
		want, inTarget := tm.Service(n)
		job, err := c.Nomad.Job(ctx, n)
		if err != nil {
			return res, err
		}
		if inTarget && nomadx.UpToDate(job, want) {
			continue
		}
		todo = append(todo, n)
	}
	res.Changed = todo
	err = c.restore(context.WithoutCancel(ctx), id, todo, tm)
	res.Timings.RollbackDone = time.Now()
	res.RollbackSeconds = res.Timings.RollbackSeconds()
	res.RolledBack = todo
	fctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err != nil {
		res.Status, res.Reason = store.StatusFailed, err.Error()
		_ = c.State.Finish(fctx, id, store.StatusFailed, err.Error())
		return res, err
	}
	res.Status = store.StatusSucceeded
	_ = c.State.Finish(fctx, id, store.StatusSucceeded, "")
	c.event(fctx, id, "", "succeeded", "now on %s after %.1fs", target.Version, res.RollbackSeconds)
	return res, nil
}

// Reconcile cleans up after a controller that died mid-release: every
// service touched by an interrupted release goes back to the current
// release (or is stopped if the current release does not have it), and the
// interrupted records are closed. It also takes over a lock whose holder
// stopped heartbeating.
func (c *Controller) Reconcile(ctx context.Context) (*Result, error) {
	res := &Result{Kind: store.KindReconcile, Timings: Timings{Started: time.Now()}}
	defer func() { res.Timings.Finished = time.Now() }()

	ctx, l, err := c.acquire(ctx)
	if err != nil {
		res.Status, res.Reason = "locked", err.Error()
		return res, err
	}
	defer c.release(l)

	inprog, err := c.State.InProgress(ctx)
	if err != nil {
		return res, err
	}
	cur, curM, err := c.currentManifest(ctx)
	if err != nil {
		return res, err
	}
	if cur != nil {
		res.Version = cur.Version
	}

	// Services to look at: everything in the current release, plus anything
	// an interrupted release might have started.
	var names []string
	add := func(m *manifest.Manifest) {
		order, _ := m.Order()
		for i := len(order) - 1; i >= 0; i-- {
			if !slices.Contains(names, order[i].Name) {
				names = append(names, order[i].Name)
			}
		}
	}
	for _, r := range inprog {
		if m, err := manifest.Parse(r.Manifest); err == nil {
			add(m)
		}
	}
	if curM != nil {
		add(curM)
	}

	var todo []string
	for _, n := range names {
		job, err := c.Nomad.Job(ctx, n)
		if err != nil {
			return res, err
		}
		var want manifest.Service
		inCur := false
		if curM != nil {
			want, inCur = curM.Service(n)
		}
		switch {
		case !inCur && (job == nil || (job.Stop != nil && *job.Stop)):
			continue // nothing running that should not be
		case inCur && nomadx.UpToDate(job, want):
			if ok, _, err := c.Nomad.ChecksPassing(ctx, n); err == nil && ok {
				continue
			}
		}
		todo = append(todo, n)
	}

	var id int64
	if len(inprog) > 0 {
		id = inprog[len(inprog)-1].ID
	}
	c.event(ctx, id, "", "reconcile", "%d interrupted release(s); restoring %v to %s", len(inprog), todo, res.Version)
	res.Timings.FailureDetected = time.Now()
	err = c.restore(context.WithoutCancel(ctx), id, todo, curM)
	res.Timings.RollbackDone = time.Now()
	res.RollbackSeconds = res.Timings.RollbackSeconds()
	res.RolledBack = todo
	fctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err != nil {
		res.Status, res.Reason = store.StatusFailed, err.Error()
		return res, err
	}
	for _, r := range inprog {
		_ = c.State.Finish(fctx, r.ID, store.StatusReconciled, fmt.Sprintf("interrupted at stage %q; restored %s", r.Stage, res.Version))
	}
	res.Status = store.StatusReconciled
	if len(inprog) == 0 && len(todo) == 0 {
		res.Status = "noop"
	}
	return res, nil
}
