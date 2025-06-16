// Package nomadx renders manifest services as Nomad jobs and watches their
// deployments closely enough to call a rollout failed within seconds.
package nomadx

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/nomad/api"

	"github.com/Abhayrkhot/release-orchestrator/internal/manifest"
)

// Timing controls how quickly Nomad judges an allocation.
type Timing struct {
	MinHealthy       time.Duration
	HealthyDeadline  time.Duration
	ProgressDeadline time.Duration
	CheckInterval    time.Duration
	CheckTimeout     time.Duration
}

var DefaultTiming = Timing{
	MinHealthy:       3 * time.Second,
	HealthyDeadline:  20 * time.Second,
	ProgressDeadline: 30 * time.Second,
	CheckInterval:    1 * time.Second,
	CheckTimeout:     1 * time.Second, // Nomad minimum
}

const (
	MetaRelease  = "release_version"
	MetaService  = "service_version"
	MetaSpecHash = "spec_hash"
	MetaDigest   = "image_digest"
	MetaRender   = "render_rev"
)

// RenderRevision changes whenever Render produces a different job for the
// same manifest service, so a controller upgrade re-renders running jobs.
const RenderRevision = "2"

// UpToDate reports whether a registered, running job already runs svc as the
// current renderer would render it.
func UpToDate(job *api.Job, svc manifest.Service) bool {
	return job != nil && job.Meta[MetaSpecHash] == svc.SpecHash() && job.Meta[MetaRender] == RenderRevision &&
		(job.Stop == nil || !*job.Stop) && (job.Status == nil || *job.Status != "dead")
}

// RenderOptions vary between a normal rollout and a rollback.
type RenderOptions struct {
	ReleaseVersion string
	// MaxParallel 0 means one allocation at a time (a rolling update). A
	// rollback sets it to the service count so every bad allocation is
	// replaced at once.
	MaxParallel int
	Timing      Timing
	Datacenters []string
}

func ptr[T any](v T) *T { return &v }

// UpstreamEnv is the env var name a dependent reads its upstream list from.
func UpstreamEnv(dep string) string {
	return strings.ToUpper(strings.ReplaceAll(dep, "-", "_")) + "_UPSTREAMS"
}

func Render(svc manifest.Service, o RenderOptions) *api.Job {
	t := o.Timing
	if t == (Timing{}) {
		t = DefaultTiming
	}
	dcs := o.Datacenters
	if len(dcs) == 0 {
		dcs = []string{"dc1"}
	}
	maxPar := o.MaxParallel
	if maxPar <= 0 {
		maxPar = 1
	}

	job := api.NewServiceJob(svc.Name, svc.Name, "global", 50)
	job.Datacenters = dcs
	job.Meta = map[string]string{
		MetaRelease:  o.ReleaseVersion,
		MetaService:  svc.Version,
		MetaSpecHash: svc.SpecHash(),
		MetaDigest:   svc.Digest,
		MetaRender:   RenderRevision,
	}

	tg := api.NewTaskGroup(svc.Name, svc.Count)
	tg.Networks = []*api.NetworkResource{{Mode: "host", DynamicPorts: []api.Port{{Label: "http"}}}}
	// Prefer one allocation per node, but keep running on fewer nodes if one
	// is lost.
	tg.Spreads = []*api.Spread{{Attribute: "${node.unique.id}", Weight: ptr(int8(100))}}
	tg.Update = &api.UpdateStrategy{
		MaxParallel:      ptr(maxPar),
		HealthCheck:      ptr("checks"),
		MinHealthyTime:   ptr(t.MinHealthy),
		HealthyDeadline:  ptr(t.HealthyDeadline),
		ProgressDeadline: ptr(t.ProgressDeadline),
		AutoRevert:       ptr(false), // the controller owns rollback
		Canary:           ptr(0),
	}
	// Crash loops should surface as a failed allocation, not hide behind
	// restarts while the deadline runs.
	tg.RestartPolicy = &api.RestartPolicy{
		Attempts: ptr(1), Interval: ptr(30 * time.Second), Delay: ptr(1 * time.Second), Mode: ptr("fail"),
	}
	tg.ReschedulePolicy = &api.ReschedulePolicy{
		Attempts: ptr(0), Interval: ptr(time.Duration(0)), Unlimited: ptr(false),
	}
	tg.Services = []*api.Service{{
		Name:      svc.Name,
		PortLabel: "http",
		Provider:  "nomad",
		Tags:      []string{"v" + svc.Version, "release-" + o.ReleaseVersion},
		Checks: []api.ServiceCheck{{
			Name: "http", Type: "http", Path: svc.HealthPath,
			Interval: t.CheckInterval, Timeout: t.CheckTimeout,
		}},
	}}
	// Without a restart on failing checks the health deadline is what fails
	// an allocation that runs but never becomes healthy.

	task := api.NewTask(svc.Name, "docker")
	task.Config = map[string]any{
		"image":        svc.ImageRef(),
		"network_mode": "host",
		"force_pull":   false,
	}
	env := map[string]string{
		"SERVICE_NAME":    svc.Name,
		"SERVICE_VERSION": svc.Version,
		"RELEASE_VERSION": o.ReleaseVersion,
		"BINARY_SHA256":   svc.BinarySHA256,
		"MIN_SCHEMA":      fmt.Sprint(svc.MinSchema),
		"MAX_SCHEMA":      fmt.Sprint(svc.MaxSchema),
		"NODE_NAME":       "${node.unique.name}",
		"PORT":            "${NOMAD_PORT_http}",
	}
	for k, v := range svc.Env {
		env[k] = v
	}
	task.Env = env
	task.Resources = &api.Resources{CPU: ptr(svc.CPU), MemoryMB: ptr(svc.MemoryMB)}
	task.KillTimeout = ptr(3 * time.Second)

	// Database credentials come from a Nomad Variable at nomad/jobs/<service>.
	// A change to the variable reaches new allocations only: deleting or
	// breaking it must not restart running tasks into a crash loop.
	task.Templates = append(task.Templates, &api.Template{
		EmbeddedTmpl: ptr(`{{ with nomadVar "nomad/jobs/` + svc.Name + `" }}DB_DSN={{ .db_dsn }}{{ end }}`),
		DestPath:     ptr("secrets/db.env"),
		Envvars:      ptr(true),
		ChangeMode:   ptr("noop"),
	})
	deps := append([]string(nil), svc.DependsOn...)
	sort.Strings(deps)
	for _, d := range deps {
		// Upstream addresses are rendered to a file the service re-reads,
		// so a dependency rolling does not restart its dependents.
		task.Templates = append(task.Templates, &api.Template{
			EmbeddedTmpl: ptr(`{{ range nomadService "` + d + `" }}{{ .Address }}:{{ .Port }}
{{ end }}`),
			DestPath:   ptr("local/upstreams/" + d),
			ChangeMode: ptr("noop"),
		})
		task.Env[UpstreamEnv(d)] = "/local/upstreams/" + d
	}
	tg.Tasks = []*api.Task{task}
	job.TaskGroups = []*api.TaskGroup{tg}
	return job
}
