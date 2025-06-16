// Command relctl builds, signs, checks, deploys and rolls back releases.
package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/hashicorp/nomad/api"

	"github.com/Abhayrkhot/release-orchestrator/internal/controller"
	"github.com/Abhayrkhot/release-orchestrator/internal/manifest"
	"github.com/Abhayrkhot/release-orchestrator/internal/migrate"
	"github.com/Abhayrkhot/release-orchestrator/internal/nomadx"
	"github.com/Abhayrkhot/release-orchestrator/internal/ociimage"
	"github.com/Abhayrkhot/release-orchestrator/internal/store"
	"github.com/Abhayrkhot/release-orchestrator/preflight"
)

var version = "dev"

const usage = `relctl: release controller for Nomad services

Build and sign:
  relctl keygen    --out keys/release [--trust DIR]
  relctl trust     --pub FILE --dir DIR   add a public key to a trust directory
  relctl image build --binary FILE --name NAME --version V --arch ARCH [--push URL] [--out DIR]
  relctl image push  --layout DIR --repo NAME --tag TAG --registry URL
  relctl image rm    --registry URL --repo NAME --digest DIGEST
  relctl manifest create --spec FILE --version V --images DIR --migrations DIR --out DIR [--service-version V] [--set svc=V] [--schema-target N]
  relctl manifest sign   --bundle DIR --key FILE
  relctl manifest verify --bundle DIR [--trust DIR]
  relctl manifest lint FILE | relctl manifest order FILE
  relctl render --manifest FILE --service NAME

Operate (needs NOMAD_ADDR and RELCTL_MYSQL_*):
  relctl bootstrap                    create state tables and per-service Nomad Variables
  relctl preflight --bundle DIR       run the preflight checks only
  relctl deploy    --bundle DIR [--json] [--allow-downgrade] [--skip-preflight]
  relctl rollback  [--to VERSION] [--json]
  relctl reconcile [--json]           clean up after an interrupted release
  relctl status    [--json]
  relctl history   [--limit N]
  relctl events    --release ID
  relctl verify-runtime               compare running binaries with the current manifest
  relctl migrate status | relctl migrate repair --version N [--drop]
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "keygen":
		err = cmdKeygen(args)
	case "trust":
		err = cmdTrust(args)
	case "image":
		err = cmdImage(ctx, args)
	case "manifest":
		err = cmdManifest(args)
	case "render":
		err = cmdRender(args)
	case "bootstrap":
		err = cmdBootstrap(ctx, args)
	case "preflight":
		err = cmdPreflight(ctx, args)
	case "deploy":
		err = cmdDeploy(ctx, args)
	case "rollback":
		err = cmdRollback(ctx, args)
	case "reconcile":
		err = cmdReconcile(ctx, args)
	case "status":
		err = cmdStatus(ctx, args)
	case "history":
		err = cmdHistory(ctx, args)
	case "events":
		err = cmdEvents(ctx, args)
	case "verify-runtime":
		err = cmdVerifyRuntime(ctx, args)
	case "migrate":
		err = cmdMigrate(ctx, args)
	case "version":
		fmt.Println(version)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "relctl:", err)
		var ec exitCoder
		if errors.As(err, &ec) {
			os.Exit(ec.code)
		}
		os.Exit(1)
	}
}

// Exit codes the tests rely on: 1 error, 3 blocked by preflight or
// verification, 4 rolled back, 5 rollback failed, 6 locked.
type exitCoder struct {
	code int
	err  error
}

func (e exitCoder) Error() string { return e.err.Error() }
func (e exitCoder) Unwrap() error { return e.err }

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func newFlags(name string) *flag.FlagSet {
	return flag.NewFlagSet(name, flag.ExitOnError)
}

// ---- build and sign ----

func cmdKeygen(args []string) error {
	fs := newFlags("keygen")
	out := fs.String("out", "keys/release", "path prefix for <out>.key and <out>.pub")
	trust := fs.String("trust", "", "also add the public key to this trust directory")
	fs.Parse(args)
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(*out + ".key"); err == nil {
		return fmt.Errorf("%s.key exists; refusing to overwrite", *out)
	}
	id, err := manifest.GenerateKey(*out+".key", *out+".pub")
	if err != nil {
		return err
	}
	fmt.Printf("key %s: %s.key (private), %s.pub\n", id, *out, *out)
	if *trust != "" {
		if _, err := manifest.TrustPublicKey(*out+".pub", *trust); err != nil {
			return err
		}
		fmt.Printf("trusted in %s/%s.pem\n", *trust, id)
	}
	return nil
}

func cmdTrust(args []string) error {
	fs := newFlags("trust")
	pub := fs.String("pub", "", "PEM public key (PKIX)")
	dir := fs.String("dir", env("RELCTL_TRUST_DIR", "keys/trusted"), "trust directory")
	fs.Parse(args)
	id, err := manifest.TrustPublicKey(*pub, *dir)
	if err != nil {
		return err
	}
	fmt.Printf("trusted key %s in %s\n", id, *dir)
	return nil
}

func sourceDateEpoch() time.Time {
	if s := os.Getenv("SOURCE_DATE_EPOCH"); s != "" {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return time.Unix(n, 0).UTC()
		}
	}
	return time.Unix(0, 0).UTC()
}

func cmdImage(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("image: build | push | rm")
	}
	switch args[0] {
	case "build":
		fs := newFlags("image build")
		bin := fs.String("binary", "", "static linux binary")
		name := fs.String("name", "", "image and entrypoint name")
		ver := fs.String("version", "", "service version")
		arch := fs.String("arch", "arm64", "amd64 or arm64")
		push := fs.String("push", "", "registry base URL to push to, e.g. http://localhost:5050")
		out := fs.String("out", "dist/images", "where to write the descriptor and OCI layout")
		fs.Parse(args[1:])
		if *bin == "" || *name == "" || *ver == "" {
			return errors.New("--binary, --name and --version are required")
		}
		img, err := ociimage.Build(ociimage.BuildOptions{
			Binary: *bin, Name: *name, Arch: *arch, Created: sourceDateEpoch(), User: "65534:65534",
			Labels: map[string]string{"org.opencontainers.image.version": *ver, "org.opencontainers.image.title": *name},
		})
		if err != nil {
			return err
		}
		d := manifest.ImageDescriptor{
			Name: *name, Version: *ver, Arch: *arch, Repository: *name, Digest: img.Manifest.Digest,
			BinarySHA256: img.BinarySHA256, ConfigDigest: img.Config.Digest, LayerDigest: img.Layer.Digest,
		}
		if err := img.WriteLayout(filepath.Join(*out, "oci", fmt.Sprintf("%s-%s-%s", *name, *ver, *arch))); err != nil {
			return err
		}
		b, _ := json.MarshalIndent(d, "", "  ")
		if err := os.WriteFile(manifest.DescriptorPath(*out, *name, *ver, *arch), append(b, '\n'), 0o644); err != nil {
			return err
		}
		if *push != "" {
			if err := (&ociimage.Registry{Base: *push}).Push(ctx, *name, *ver, img); err != nil {
				return err
			}
		}
		fmt.Printf("%s:%s %s %s\n", *name, *ver, *arch, img.Manifest.Digest)
		return nil
	case "push":
		fs := newFlags("image push")
		layout := fs.String("layout", "", "OCI layout directory written by image build")
		repo := fs.String("repo", "", "repository")
		tag := fs.String("tag", "", "tag")
		reg := fs.String("registry", env("RELCTL_REGISTRY_URL", "http://registry:5000"), "registry base URL")
		fs.Parse(args[1:])
		img, err := ociimage.ReadLayout(*layout)
		if err != nil {
			return err
		}
		if err := (&ociimage.Registry{Base: *reg}).Push(ctx, *repo, *tag, img); err != nil {
			return err
		}
		fmt.Printf("pushed %s:%s %s\n", *repo, *tag, img.Manifest.Digest)
		return nil
	case "rm":
		fs := newFlags("image rm")
		repo := fs.String("repo", "", "repository")
		digest := fs.String("digest", "", "manifest digest")
		reg := fs.String("registry", env("RELCTL_REGISTRY_URL", "http://registry:5000"), "registry base URL")
		fs.Parse(args[1:])
		return (&ociimage.Registry{Base: *reg}).DeleteManifest(ctx, *repo, *digest)
	}
	return fmt.Errorf("image: unknown subcommand %q", args[0])
}

type kvFlag map[string]string

func (k kvFlag) String() string { return fmt.Sprint(map[string]string(k)) }
func (k kvFlag) Set(s string) error {
	a, b, ok := strings.Cut(s, "=")
	if !ok {
		return fmt.Errorf("want key=value, got %q", s)
	}
	k[a] = b
	return nil
}

func cmdManifest(args []string) error {
	if len(args) == 0 {
		return errors.New("manifest: create | sign | verify | lint | order")
	}
	switch args[0] {
	case "create":
		fs := newFlags("manifest create")
		spec := fs.String("spec", "releases/spec.json", "release spec")
		ver := fs.String("version", "", "release version")
		svcVer := fs.String("service-version", "", "image version for every service")
		set := kvFlag{}
		fs.Var(set, "set", "service=version override (repeatable)")
		images := fs.String("images", "dist/images", "image descriptors")
		arch := fs.String("arch", "arm64", "image architecture")
		migs := fs.String("migrations", "migrations", "migrations directory")
		target := fs.Int("schema-target", 0, "include migrations up to this version (default all)")
		out := fs.String("out", "", "bundle directory")
		commit := fs.String("commit", env("GIT_COMMIT", ""), "source commit")
		clean := fs.Bool("clean", false, "remove an existing bundle's files in --out first")
		envs := kvFlag{}
		fs.Var(envs, "env", "service:KEY=VALUE added to that service's environment (repeatable)")
		fs.Parse(args[1:])
		if *ver == "" || *out == "" {
			return errors.New("--version and --out are required")
		}
		if *clean {
			if err := manifest.CleanBundle(*out); err != nil {
				return err
			}
		}
		sp, err := manifest.LoadSpec(*spec)
		if err != nil {
			return err
		}
		for k, v := range envs {
			svc, key, ok := strings.Cut(k, ":")
			if !ok {
				return fmt.Errorf("--env %s=%s: want service:KEY=VALUE", k, v)
			}
			found := false
			for i := range sp.Services {
				if sp.Services[i].Name == svc {
					if sp.Services[i].Env == nil {
						sp.Services[i].Env = map[string]string{}
					}
					sp.Services[i].Env[key] = v
					found = true
				}
			}
			if !found {
				return fmt.Errorf("--env: no service %q in spec", svc)
			}
		}
		sv := map[string]string{"": *svcVer}
		if *svcVer == "" {
			sv[""] = *ver
		}
		for k, v := range set {
			sv[k] = v
		}
		m, err := manifest.CreateBundle(manifest.CreateOptions{
			Version: *ver, Spec: sp, ServiceVersions: sv, ImagesDir: *images, Arch: *arch,
			MigrationsDir: *migs, SchemaTarget: *target, Commit: *commit, CreatedAt: sourceDateEpoch(), OutDir: *out,
		})
		if err != nil {
			return err
		}
		fmt.Printf("bundle %s: release %s, schema %d, %d services (unsigned)\n", *out, m.Version, m.Schema.Target, len(m.Services))
		return nil
	case "sign":
		fs := newFlags("manifest sign")
		bundle := fs.String("bundle", "", "bundle directory")
		key := fs.String("key", env("RELCTL_SIGNING_KEY", "keys/release.key"), "ed25519 private key")
		fs.Parse(args[1:])
		id, err := manifest.SignBundle(*bundle, *key)
		if err != nil {
			return err
		}
		fmt.Printf("signed %s with key %s\n", filepath.Join(*bundle, manifest.ManifestFile), id)
		return nil
	case "verify":
		fs := newFlags("manifest verify")
		bundle := fs.String("bundle", "", "bundle directory")
		trust := fs.String("trust", env("RELCTL_TRUST_DIR", "keys/trusted"), "trusted public keys")
		fs.Parse(args[1:])
		b, err := manifest.OpenBundle(*bundle, *trust)
		if err != nil {
			return exitCoder{3, err}
		}
		fmt.Printf("ok: release %s signed by %s, %d migrations and %d services verified\n",
			b.Manifest.Version, b.KeyID, len(b.Manifest.Schema.Migrations), len(b.Manifest.Services))
		return nil
	case "lint", "order":
		if len(args) < 2 {
			return fmt.Errorf("manifest %s FILE", args[0])
		}
		m, _, err := manifest.Load(args[1])
		if err != nil {
			return err
		}
		if args[0] == "lint" {
			return m.Lint()
		}
		order, err := m.Order()
		if err != nil {
			return err
		}
		for _, s := range order {
			fmt.Println(s.Name)
		}
		return nil
	}
	return fmt.Errorf("manifest: unknown subcommand %q", args[0])
}

func cmdRender(args []string) error {
	fs := newFlags("render")
	mf := fs.String("manifest", "", "manifest file")
	svc := fs.String("service", "", "service name")
	fs.Parse(args)
	m, _, err := manifest.Load(*mf)
	if err != nil {
		return err
	}
	s, ok := m.Service(*svc)
	if !ok {
		return fmt.Errorf("service %q not in manifest", *svc)
	}
	return json.NewEncoder(os.Stdout).Encode(nomadx.Render(s, nomadx.RenderOptions{ReleaseVersion: m.Version, Timing: timing()}))
}

// ---- operate ----

type deps struct {
	state *store.Store
	app   *sql.DB
	nomad *nomadx.Client
	ctl   *controller.Controller
}

func dsn(db string, extra map[string]string) string {
	c := mysql.NewConfig()
	c.User = env("RELCTL_MYSQL_USER", "relctl")
	c.Passwd = os.Getenv("RELCTL_MYSQL_PASSWORD")
	c.Net = "tcp"
	c.Addr = env("RELCTL_MYSQL_HOST", "mysql") + ":" + env("RELCTL_MYSQL_PORT", "3306")
	c.DBName = db
	c.ParseTime = true
	c.Timeout = 5 * time.Second
	c.ReadTimeout = 30 * time.Second
	c.WriteTimeout = 30 * time.Second
	c.ClientFoundRows = true
	c.Params = extra
	return c.FormatDSN()
}

func durationEnv(k string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(k)); err == nil {
		return d
	}
	return def
}

func timing() nomadx.Timing {
	t := nomadx.DefaultTiming
	t.MinHealthy = durationEnv("RELCTL_MIN_HEALTHY", t.MinHealthy)
	t.HealthyDeadline = durationEnv("RELCTL_HEALTHY_DEADLINE", t.HealthyDeadline)
	t.ProgressDeadline = durationEnv("RELCTL_PROGRESS_DEADLINE", t.ProgressDeadline)
	return t
}

func connect(ctx context.Context, quiet bool) (*deps, error) {
	stateDB, err := sql.Open("mysql", dsn(env("RELCTL_STATE_DB", "relctl"), nil))
	if err != nil {
		return nil, err
	}
	appDB, err := sql.Open("mysql", dsn(env("RELCTL_APP_DB", "app"), map[string]string{"multiStatements": "true"}))
	if err != nil {
		return nil, err
	}
	appDB.SetMaxOpenConns(4)
	stateDB.SetMaxOpenConns(4)
	nc, err := nomadx.New(env("NOMAD_ADDR", "http://nomad-server:4646"))
	if err != nil {
		return nil, err
	}
	st := &store.Store{DB: stateDB}
	// The tables normally exist already. If the database is down, carry on:
	// deploy still runs the preflight checks so the cause gets reported.
	if err := st.Init(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "relctl: warning:", err)
	}
	host, _ := os.Hostname()
	rnd := make([]byte, 4)
	rand.Read(rnd)
	logw := os.Stderr
	if quiet {
		logw = nil
	}
	pfDir := os.Getenv("RELCTL_PREFLIGHT_DIR")
	self, _ := os.Executable()
	ctl := &controller.Controller{
		State: st, AppDB: appDB, Nomad: nc,
		TrustDir:  env("RELCTL_TRUST_DIR", "/etc/relctl/trusted"),
		Preflight: &preflight.Runner{Dir: pfDir, Output: os.Stderr},
		PreflightEnv: map[string]string{
			"NOMAD_ADDR":     env("NOMAD_ADDR", "http://nomad-server:4646"),
			"REGISTRY_URL":   env("RELCTL_REGISTRY_URL", "http://registry:5000"),
			"MYSQL_HOST":     env("RELCTL_MYSQL_HOST", "mysql"),
			"MYSQL_PORT":     env("RELCTL_MYSQL_PORT", "3306"),
			"MYSQL_USER":     env("RELCTL_MYSQL_USER", "relctl"),
			"MYSQL_PASSWORD": os.Getenv("RELCTL_MYSQL_PASSWORD"),
			"APP_DB":         env("RELCTL_APP_DB", "app"),
			"STATE_DB":       env("RELCTL_STATE_DB", "relctl"),
			"REQUIRED_NODES": env("RELCTL_REQUIRED_NODES", "3"),
			"RELCTL":         self,
		},
		Owner:          fmt.Sprintf("%s/%d/%s", host, os.Getpid(), hex.EncodeToString(rnd)),
		LockTTL:        durationEnv("RELCTL_LOCK_TTL", 15*time.Second),
		ServiceTimeout: durationEnv("RELCTL_SERVICE_TIMEOUT", 90*time.Second),
		Timing:         timing(),
	}
	if logw != nil {
		ctl.Log = logw
	}
	return &deps{state: st, app: appDB, nomad: nc, ctl: ctl}, nil
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func resultErr(res *controller.Result, err error) error {
	if err == nil {
		return nil
	}
	switch {
	case res != nil && res.Status == "blocked":
		return exitCoder{3, err}
	case res != nil && res.Status == store.StatusRolledBack:
		return exitCoder{4, err}
	case res != nil && res.Status == store.StatusFailed:
		return exitCoder{5, err}
	case errors.Is(err, store.ErrLocked):
		return exitCoder{6, err}
	}
	return err
}

func summarize(res *controller.Result) {
	fmt.Fprintf(os.Stderr, "\n%s %s: %s", res.Kind, res.Version, strings.ToUpper(res.Status))
	if res.Reason != "" {
		fmt.Fprintf(os.Stderr, " (%s)", res.Reason)
	}
	fmt.Fprintln(os.Stderr)
	switch {
	case res.Status == store.StatusFailed:
		// The previous release was not restored; there is no time to report.
	case res.RecoverySeconds > 0:
		fmt.Fprintf(os.Stderr, "rollback: previous release healthy %.1fs after failure detected (%.1fs after the bad rollout began)\n",
			res.RollbackSeconds, res.RecoverySeconds)
	case res.RollbackSeconds > 0:
		fmt.Fprintf(os.Stderr, "restored in %.1fs\n", res.RollbackSeconds)
	}
}

func cmdBootstrap(ctx context.Context, args []string) error {
	fs := newFlags("bootstrap")
	services := fs.String("services", "catalog-api,order-worker,edge-gateway", "services that need a DSN variable")
	appUser := fs.String("app-user", env("APP_MYSQL_USER", "app"), "MySQL user the services connect as")
	appPass := fs.String("app-password", os.Getenv("APP_MYSQL_PASSWORD"), "its password")
	fs.Parse(args)
	d, err := connect(ctx, true)
	if err != nil {
		return err
	}
	if err := (&migrate.Migrator{DB: d.app}).Ensure(ctx); err != nil {
		return err
	}
	c := mysql.NewConfig()
	c.User, c.Passwd, c.Net = *appUser, *appPass, "tcp"
	c.Addr = env("RELCTL_MYSQL_HOST", "mysql") + ":" + env("RELCTL_MYSQL_PORT", "3306")
	c.DBName = env("RELCTL_APP_DB", "app")
	c.ParseTime = true
	c.Timeout = 3 * time.Second
	for _, s := range strings.Split(*services, ",") {
		v := &api.Variable{Path: "nomad/jobs/" + s, Items: api.VariableItems{"db_dsn": c.FormatDSN()}}
		if _, _, err := d.nomad.API.Variables().Create(v, nil); err != nil {
			return fmt.Errorf("variable %s: %w", v.Path, err)
		}
		fmt.Printf("variable nomad/jobs/%s written\n", s)
	}
	fmt.Println("state tables ready")
	return nil
}

func cmdPreflight(ctx context.Context, args []string) error {
	fs := newFlags("preflight")
	bundle := fs.String("bundle", "", "bundle directory")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	fs.Parse(args)
	d, err := connect(ctx, true)
	if err != nil {
		return err
	}
	cur, err := d.state.Current(ctx)
	res, err := d.ctl.PreflightOnly(ctx, *bundle, cur, err != nil)
	if err != nil {
		return err
	}
	if *asJSON {
		printJSON(res)
	}
	if !res.Passed {
		return exitCoder{3, fmt.Errorf("preflight failed: %s", strings.Join(res.FailedIDs(), ", "))}
	}
	return nil
}

func cmdDeploy(ctx context.Context, args []string) error {
	fs := newFlags("deploy")
	bundle := fs.String("bundle", "", "bundle directory")
	asJSON := fs.Bool("json", false, "print the result as JSON on stdout")
	down := fs.Bool("allow-downgrade", false, "allow a release older than the running one")
	skip := fs.Bool("skip-preflight", false, "skip the Bash preflight checks (signature is still verified)")
	fs.Parse(args)
	if *bundle == "" {
		return errors.New("--bundle is required")
	}
	d, err := connect(ctx, false)
	if err != nil {
		return err
	}
	res, err := d.ctl.Deploy(ctx, *bundle, controller.DeployOptions{SkipPreflight: *skip, AllowDowngrade: *down})
	if *asJSON {
		printJSON(res)
	}
	summarize(res)
	return resultErr(res, err)
}

func cmdRollback(ctx context.Context, args []string) error {
	fs := newFlags("rollback")
	to := fs.String("to", "", "release version to return to (default: the one before the current)")
	asJSON := fs.Bool("json", false, "print the result as JSON on stdout")
	fs.Parse(args)
	d, err := connect(ctx, false)
	if err != nil {
		return err
	}
	res, err := d.ctl.Rollback(ctx, *to)
	if *asJSON {
		printJSON(res)
	}
	summarize(res)
	return resultErr(res, err)
}

func cmdReconcile(ctx context.Context, args []string) error {
	fs := newFlags("reconcile")
	asJSON := fs.Bool("json", false, "print the result as JSON on stdout")
	fs.Parse(args)
	d, err := connect(ctx, false)
	if err != nil {
		return err
	}
	res, err := d.ctl.Reconcile(ctx)
	if *asJSON {
		printJSON(res)
	}
	summarize(res)
	return resultErr(res, err)
}

type serviceStatus struct {
	Name      string            `json:"name"`
	Want      string            `json:"want_version"`
	JobMeta   map[string]string `json:"job_meta,omitempty"`
	Endpoints []nomadx.Endpoint `json:"endpoints"`
	Healthy   bool              `json:"healthy"`
	Detail    string            `json:"detail,omitempty"`
}

type statusOut struct {
	Release  string          `json:"release"`
	Schema   int             `json:"schema"`
	Dirty    bool            `json:"dirty"`
	Lock     string          `json:"lock,omitempty"`
	Services []serviceStatus `json:"services"`
}

func cmdStatus(ctx context.Context, args []string) error {
	fs := newFlags("status")
	asJSON := fs.Bool("json", false, "JSON output")
	fs.Parse(args)
	d, err := connect(ctx, true)
	if err != nil {
		return err
	}
	out := statusOut{}
	cur, err := d.state.Current(ctx)
	if err != nil {
		return err
	}
	out.Schema, out.Dirty, _ = (&migrate.Migrator{DB: d.app}).Current(ctx)
	if li, err := d.state.Lock(ctx, d.ctl.LockTTL); err == nil && li.Owner != "" {
		out.Lock = li.Owner
		if li.Stale {
			out.Lock += " (stale)"
		}
	}
	if cur != nil {
		out.Release = cur.Version
		m, err := manifest.Parse(cur.Manifest)
		if err != nil {
			return err
		}
		order, _ := m.Order()
		for _, s := range order {
			ss := serviceStatus{Name: s.Name, Want: s.Version}
			if j, err := d.nomad.Job(ctx, s.Name); err == nil && j != nil {
				ss.JobMeta = j.Meta
			}
			ss.Endpoints, _ = d.nomad.Endpoints(ctx, s.Name)
			ss.Healthy, ss.Detail, _ = d.nomad.ChecksPassing(ctx, s.Name)
			out.Services = append(out.Services, ss)
		}
	}
	if *asJSON {
		printJSON(out)
		return nil
	}
	fmt.Printf("release %s, schema %d", out.Release, out.Schema)
	if out.Dirty {
		fmt.Print(" (DIRTY)")
	}
	if out.Lock != "" {
		fmt.Printf(", lock held by %s", out.Lock)
	}
	fmt.Println()
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SERVICE\tWANT\tRUNNING\tHEALTHY\tNODES")
	for _, s := range out.Services {
		var nodes []string
		for _, e := range s.Endpoints {
			nodes = append(nodes, e.Node)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%v\t%s\n", s.Name, s.Want, s.JobMeta[nomadx.MetaService], s.Healthy, strings.Join(nodes, ","))
	}
	return tw.Flush()
}

func cmdHistory(ctx context.Context, args []string) error {
	fs := newFlags("history")
	limit := fs.Int("limit", 20, "rows")
	fs.Parse(args)
	d, err := connect(ctx, true)
	if err != nil {
		return err
	}
	rs, err := d.state.History(ctx, *limit)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tKIND\tVERSION\tSTATUS\tSTARTED\tDURATION\tERROR")
	for _, r := range rs {
		dur := ""
		if r.FinishedAt.Valid {
			dur = r.FinishedAt.Time.Sub(r.StartedAt).Round(100 * time.Millisecond).String()
		}
		e := r.Error
		if len(e) > 80 {
			e = e[:77] + "..."
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n", r.ID, r.Kind, r.Version, r.Status, r.StartedAt.Format("01-02 15:04:05"), dur, e)
	}
	return tw.Flush()
}

func cmdEvents(ctx context.Context, args []string) error {
	fs := newFlags("events")
	id := fs.Int64("release", 0, "release id")
	asJSON := fs.Bool("json", false, "JSON output")
	fs.Parse(args)
	d, err := connect(ctx, true)
	if err != nil {
		return err
	}
	evs, err := d.state.Events(ctx, *id)
	if err != nil {
		return err
	}
	if *asJSON {
		printJSON(evs)
		return nil
	}
	for _, e := range evs {
		fmt.Printf("%s  %-15s %-14s %s\n", e.At.Format("15:04:05.000"), e.Kind, e.Service, e.Detail)
	}
	return nil
}

func cmdVerifyRuntime(ctx context.Context, args []string) error {
	fs := newFlags("verify-runtime")
	asJSON := fs.Bool("json", false, "JSON output")
	fs.Parse(args)
	d, err := connect(ctx, true)
	if err != nil {
		return err
	}
	cur, err := d.state.Current(ctx)
	if err != nil || cur == nil {
		return fmt.Errorf("no current release: %v", err)
	}
	m, err := manifest.Parse(cur.Manifest)
	if err != nil {
		return err
	}
	vs, err := d.ctl.VerifyRuntime(ctx, m, nil)
	if *asJSON {
		printJSON(vs)
	} else {
		for _, v := range vs {
			fmt.Printf("%-13s %-7s v%-7s %s\n", v.Service, v.Node, v.Version, v.BinarySHA256)
		}
	}
	if err != nil {
		return exitCoder{3, err}
	}
	fmt.Fprintf(os.Stderr, "%d allocations match release %s\n", len(vs), cur.Version)
	return nil
}

func cmdMigrate(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("migrate: status | repair")
	}
	d, err := connect(ctx, true)
	if err != nil {
		return err
	}
	mg := &migrate.Migrator{DB: d.app}
	switch args[0] {
	case "status":
		as, err := mg.Applied(ctx)
		if err != nil {
			return err
		}
		for _, a := range as {
			fmt.Printf("%04d %-28s %s dirty=%v\n", a.Version, a.Name, a.Checksum[:12], a.Dirty)
		}
		return nil
	case "repair":
		fs := newFlags("migrate repair")
		v := fs.Int("version", 0, "dirty migration version")
		drop := fs.Bool("drop", false, "remove the row so the migration runs again")
		fs.Parse(args[1:])
		return mg.Repair(ctx, *v, *drop)
	}
	return fmt.Errorf("migrate: unknown subcommand %q", args[0])
}
