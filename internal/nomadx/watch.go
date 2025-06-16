package nomadx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/nomad/api"
)

type Client struct {
	API  *api.Client
	Poll time.Duration
	HTTP *http.Client
}

func New(addr string) (*Client, error) {
	cfg := api.DefaultConfig()
	if addr != "" {
		cfg.Address = addr
	}
	c, err := api.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	return &Client{API: c, Poll: 500 * time.Millisecond, HTTP: &http.Client{Timeout: 2 * time.Second}}, nil
}

// maxRetry bounds every retried call, even under a context that never ends:
// a rollback runs under context.WithoutCancel so it finishes after the
// caller gives up, and must still not spin forever.
const maxRetry = 2 * time.Minute

// retry runs fn until it succeeds, fails permanently, or ctx or maxRetry
// ends. The Nomad server can restart in the middle of a rollout; a few
// seconds of failed calls should not turn into a failed release.
func retry[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	var zero T
	delay := 250 * time.Millisecond
	deadline := time.Now().Add(maxRetry)
	for {
		v, err := fn()
		if err == nil {
			return v, nil
		}
		if !transient(err) {
			return zero, err
		}
		if time.Now().After(deadline) {
			return zero, fmt.Errorf("gave up after %s: %w", maxRetry, err)
		}
		select {
		case <-ctx.Done():
			return zero, fmt.Errorf("%w (last error: %v)", ctx.Err(), err)
		case <-time.After(delay):
		}
		if delay < 2*time.Second {
			delay *= 2
		}
	}
}

// transient reports whether an error is worth retrying. Nomad answers HTTP
// 500 both for server trouble and for requests that can never succeed (for
// example "can't fail terminal deployment"), so a 500 is retried only if its
// message points at the server, the network or leadership.
func transient(err error) bool {
	var ue api.UnexpectedResponseError
	if errors.As(err, &ue) {
		switch c := ue.StatusCode(); c {
		case 429, 502, 503, 504:
			return true
		case 500:
			return transientText(ue.Error())
		default:
			return false
		}
	}
	return transientText(err.Error())
}

func transientText(s string) bool {
	s = strings.ToLower(s)
	// "server misbehaving" and "no such host" are what Docker's DNS answers
	// while the Nomad server's container restarts.
	for _, m := range []string{"connection refused", "eof", "no cluster leader", "no leader", "timeout",
		"connection reset", "no such host", "server misbehaving", "temporary failure in name resolution",
		"rpc error", "broken pipe", "no path to region"} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// Register submits the job and returns the job version Nomad assigned.
func (c *Client) Register(ctx context.Context, job *api.Job) (uint64, error) {
	resp, err := retry(ctx, func() (*api.JobRegisterResponse, error) {
		r, _, err := c.API.Jobs().Register(job, nil)
		return r, err
	})
	if err != nil {
		return 0, fmt.Errorf("register %s: %w", *job.ID, err)
	}
	for {
		j, err := retry(ctx, func() (*api.Job, error) {
			j, _, err := c.API.Jobs().Info(*job.ID, nil)
			return j, err
		})
		if err != nil {
			return 0, err
		}
		if *j.JobModifyIndex >= resp.JobModifyIndex {
			return *j.Version, nil
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Job returns the registered job, or nil if it does not exist.
func (c *Client) Job(ctx context.Context, id string) (*api.Job, error) {
	j, err := retry(ctx, func() (*api.Job, error) {
		j, _, err := c.API.Jobs().Info(id, nil)
		return j, err
	})
	var ue api.UnexpectedResponseError
	if errors.As(err, &ue) && ue.StatusCode() == http.StatusNotFound {
		return nil, nil
	}
	return j, err
}

func (c *Client) Allocs(ctx context.Context, jobID string) ([]*api.AllocationListStub, error) {
	return retry(ctx, func() ([]*api.AllocationListStub, error) {
		a, _, err := c.API.Jobs().Allocations(jobID, false, nil)
		return a, err
	})
}

type Outcome struct {
	JobVersion   uint64        `json:"job_version"`
	DeploymentID string        `json:"deployment_id,omitempty"`
	Healthy      bool          `json:"healthy"`
	Reason       string        `json:"reason,omitempty"`
	Alloc        string        `json:"alloc,omitempty"`
	Node         string        `json:"node,omitempty"`
	Elapsed      time.Duration `json:"elapsed"`
}

// WaitHealthy follows the deployment for jobID@version until it succeeds,
// fails, or one of its allocations shows a failure Nomad has not acted on
// yet: a crash, a restart, or an unhealthy verdict.
func (c *Client) WaitHealthy(ctx context.Context, jobID string, version uint64, timeout time.Duration) Outcome {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out := Outcome{JobVersion: version}
	done := func(ok bool, reason string) Outcome {
		out.Healthy, out.Reason, out.Elapsed = ok, reason, time.Since(start)
		return out
	}
	for {
		dep, err := c.deploymentFor(ctx, jobID, version)
		if err != nil {
			return done(false, "query deployment: "+err.Error())
		}
		allocs, err := c.Allocs(ctx, jobID)
		if err != nil {
			return done(false, "query allocations: "+err.Error())
		}
		if dep != nil {
			out.DeploymentID = dep.ID
		}
		for _, a := range allocs {
			if a.JobVersion != version {
				continue
			}
			if reason := allocFailure(a, start); reason != "" {
				out.Alloc, out.Node = a.ID, a.NodeName
				return done(false, reason)
			}
		}
		if dep != nil {
			switch dep.Status {
			case "successful":
				return done(true, "")
			case "failed", "cancelled":
				return done(false, "deployment "+dep.Status+": "+dep.StatusDescription)
			}
		} else if time.Since(start) > 5*time.Second && allHealthyAt(allocs, version) {
			// A job version that only changed non-task fields can finish
			// without a deployment of its own.
			return done(true, "")
		}
		select {
		case <-ctx.Done():
			return done(false, fmt.Sprintf("not healthy within %s", timeout))
		case <-time.After(c.Poll):
		}
	}
}

func (c *Client) deploymentFor(ctx context.Context, jobID string, version uint64) (*api.Deployment, error) {
	deps, err := retry(ctx, func() ([]*api.Deployment, error) {
		d, _, err := c.API.Jobs().Deployments(jobID, false, nil)
		return d, err
	})
	if err != nil {
		return nil, err
	}
	var best *api.Deployment
	for _, d := range deps {
		if d.JobVersion == version && (best == nil || d.CreateIndex > best.CreateIndex) {
			best = d
		}
	}
	return best, nil
}

// allocFailure ignores restarts and health verdicts from before since: an
// allocation updated in place during a rollback keeps its task history.
func allocFailure(a *api.AllocationListStub, since time.Time) string {
	if a.DesiredStatus != "run" {
		return ""
	}
	if a.ClientStatus == "failed" || a.ClientStatus == "lost" {
		return fmt.Sprintf("allocation %s on %s %s: %s", short(a.ID), a.NodeName, a.ClientStatus, lastEvent(a))
	}
	for name, ts := range a.TaskStates {
		if ts.Failed {
			return fmt.Sprintf("task %s on %s failed: %s", name, a.NodeName, lastEvent(a))
		}
		if ts.Restarts > 0 && ts.LastRestart.After(since) {
			return fmt.Sprintf("task %s on %s restarted: %s", name, a.NodeName, lastEvent(a))
		}
	}
	if ds := a.DeploymentStatus; ds != nil && ds.Healthy != nil && !*ds.Healthy && ds.Timestamp.After(since) {
		return fmt.Sprintf("allocation %s on %s marked unhealthy: %s", short(a.ID), a.NodeName, lastEvent(a))
	}
	return ""
}

func allHealthyAt(allocs []*api.AllocationListStub, version uint64) bool {
	n := 0
	for _, a := range allocs {
		if a.DesiredStatus != "run" {
			continue
		}
		if a.JobVersion != version || a.ClientStatus != "running" {
			return false
		}
		if a.DeploymentStatus == nil || a.DeploymentStatus.Healthy == nil || !*a.DeploymentStatus.Healthy {
			return false
		}
		n++
	}
	return n > 0
}

func lastEvent(a *api.AllocationListStub) string {
	var evs []*api.TaskEvent
	for _, ts := range a.TaskStates {
		evs = append(evs, ts.Events...)
	}
	sort.Slice(evs, func(i, j int) bool { return evs[i].Time < evs[j].Time })
	for i := len(evs) - 1; i >= 0; i-- {
		e := evs[i]
		msg := e.DisplayMessage
		if msg == "" {
			msg = e.Type
		}
		switch e.Type {
		case "Terminated", "Driver Failure", "Killing", "Not Restarting", "Alloc Unhealthy", "Setup Failure", "Template":
			return e.Type + ": " + msg
		}
	}
	if len(evs) > 0 {
		return evs[len(evs)-1].Type
	}
	return "no task events"
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// FailDeployment stops Nomad from placing more allocations of a bad version.
// A deployment Nomad already ended needs nothing. Best effort, bounded.
func (c *Client) FailDeployment(ctx context.Context, id string) {
	if id == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	d, err := retry(ctx, func() (*api.Deployment, error) {
		d, _, err := c.API.Deployments().Info(id, nil)
		return d, err
	})
	if err != nil || d == nil {
		return
	}
	switch d.Status {
	case "successful", "failed", "cancelled":
		return
	}
	_, _ = retry(ctx, func() (*api.DeploymentUpdateResponse, error) {
		r, _, err := c.API.Deployments().Fail(id, nil)
		return r, err
	})
}

// Stop deregisters a job (used when a service's first deploy fails and there
// is no earlier version to return to) and waits for its allocations to stop.
func (c *Client) Stop(ctx context.Context, jobID string, timeout time.Duration) error {
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if _, err := retry(dctx, func() (string, error) {
		id, _, err := c.API.Jobs().Deregister(jobID, false, nil)
		return id, err
	}); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		allocs, err := c.Allocs(ctx, jobID)
		if err != nil {
			return err
		}
		running := 0
		for _, a := range allocs {
			if a.ClientStatus == "running" || a.ClientStatus == "pending" {
				running++
			}
		}
		if running == 0 {
			return nil
		}
		time.Sleep(c.Poll)
	}
	return fmt.Errorf("allocations of %s still running after %s", jobID, timeout)
}

// Endpoint is one running allocation and the address it serves on.
type Endpoint struct {
	Alloc      string `json:"alloc"`
	Node       string `json:"node"`
	Addr       string `json:"addr"`
	JobVersion uint64 `json:"job_version"`
}

func (c *Client) Endpoints(ctx context.Context, jobID string) ([]Endpoint, error) {
	stubs, err := c.Allocs(ctx, jobID)
	if err != nil {
		return nil, err
	}
	var out []Endpoint
	for _, s := range stubs {
		if s.DesiredStatus != "run" || s.ClientStatus != "running" {
			continue
		}
		a, err := retry(ctx, func() (*api.Allocation, error) {
			a, _, err := c.API.Allocations().Info(s.ID, nil)
			return a, err
		})
		if err != nil {
			return nil, err
		}
		addr := ""
		if ar := a.AllocatedResources; ar != nil {
			for _, p := range ar.Shared.Ports {
				if p.Label == "http" {
					addr = fmt.Sprintf("%s:%d", p.HostIP, p.Value)
				}
			}
			if addr == "" {
				for _, n := range ar.Shared.Networks {
					for _, p := range n.DynamicPorts {
						if p.Label == "http" {
							addr = fmt.Sprintf("%s:%d", n.IP, p.Value)
						}
					}
				}
			}
		}
		out = append(out, Endpoint{Alloc: s.ID, Node: s.NodeName, Addr: addr, JobVersion: s.JobVersion})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Node+out[i].Alloc < out[j].Node+out[j].Alloc })
	return out, nil
}

// VersionInfo is what each service returns from GET /version.
type VersionInfo struct {
	Service      string `json:"service"`
	Version      string `json:"version"`
	BuildVersion string `json:"build_version"`
	Release      string `json:"release"`
	BinarySHA256 string `json:"binary_sha256"`
	Alloc        string `json:"alloc"`
	Node         string `json:"node"`
	Schema       int    `json:"schema"`
}

func (c *Client) FetchVersion(ctx context.Context, addr string) (*VersionInfo, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/version", nil)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /version on %s: %s", addr, resp.Status)
	}
	var v VersionInfo
	return &v, json.NewDecoder(resp.Body).Decode(&v)
}

// ChecksPassing reports whether every Nomad service check of every running
// allocation of jobID passes.
func (c *Client) ChecksPassing(ctx context.Context, jobID string) (bool, string, error) {
	stubs, err := c.Allocs(ctx, jobID)
	if err != nil {
		return false, "", err
	}
	running := 0
	for _, s := range stubs {
		if s.DesiredStatus != "run" {
			continue
		}
		if s.ClientStatus != "running" {
			return false, fmt.Sprintf("allocation %s on %s is %s", short(s.ID), s.NodeName, s.ClientStatus), nil
		}
		running++
		checks, err := retry(ctx, func() (api.AllocCheckStatuses, error) {
			return c.API.Allocations().Checks(s.ID, nil)
		})
		if err != nil {
			return false, "", err
		}
		if len(checks) == 0 {
			return false, fmt.Sprintf("allocation %s on %s has no check results yet", short(s.ID), s.NodeName), nil
		}
		for _, ch := range checks {
			if ch.Status != "success" {
				return false, fmt.Sprintf("check %s on %s (%s) is %s: %s", ch.Check, s.NodeName, short(s.ID), ch.Status, strings.TrimSpace(ch.Output)), nil
			}
		}
	}
	if running == 0 {
		return false, "no running allocations", nil
	}
	return true, "", nil
}

// ForceReschedule asks Nomad to replace failed allocations of jobID even
// though the job's reschedule policy is exhausted. Re-registering an
// identical job does not do that: it creates no new version.
func (c *Client) ForceReschedule(ctx context.Context, jobID string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err := retry(ctx, func() (string, error) {
		id, _, err := c.API.Jobs().EvaluateWithOpts(jobID, api.EvalOptions{ForceReschedule: true}, nil)
		return id, err
	})
	return err
}

// WaitSteady waits until jobID has count running allocations whose checks
// all pass. It is used when there is no deployment to follow.
func (c *Client) WaitSteady(ctx context.Context, jobID string, count int, timeout time.Duration) Outcome {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	why := ""
	for {
		allocs, err := c.Allocs(ctx, jobID)
		if err == nil {
			running := 0
			for _, a := range allocs {
				if a.DesiredStatus == "run" && a.ClientStatus == "running" {
					running++
				}
			}
			if running >= count {
				ok, detail, err := c.ChecksPassing(ctx, jobID)
				if err == nil && ok {
					return Outcome{Healthy: true, Elapsed: time.Since(start)}
				}
				why = detail
			} else {
				why = fmt.Sprintf("%d of %d allocations running", running, count)
			}
		} else {
			why = err.Error()
		}
		select {
		case <-ctx.Done():
			return Outcome{Reason: "not steady within " + timeout.String() + ": " + why, Elapsed: time.Since(start)}
		case <-time.After(c.Poll):
		}
	}
}
