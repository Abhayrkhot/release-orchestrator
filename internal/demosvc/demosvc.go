// Package demosvc is the small service the cluster deploys. Three roles share
// it: catalog-api (owns the items table), order-worker and edge-gateway (both
// depend on catalog-api). It records lifecycle events in MySQL so tests can
// check restart ordering, reports exactly which binary it is, and has fault
// modes so a release can be made to fail in specific ways.
package demosvc

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

type Role string

const (
	RoleAPI     Role = "catalog-api"
	RoleWorker  Role = "order-worker"
	RoleGateway Role = "edge-gateway"
)

// Fault modes, set through FAULT_MODE in the release manifest:
//
//	crash_on_start  exit 3 shortly after start
//	unhealthy       run, but fail every health check
//	slow_start      listen only after FAULT_DELAY (default 60s)
//	crash_after     pass health checks, then exit after FAULT_DELAY (default 4s)
//	oom             allocate memory until the container's limit kills it
//	bad_schema      require a schema version that does not exist
//
// FAULT_NODE limits the fault to one node, for partial failures.
type config struct {
	role         Role
	buildVersion string
	version      string
	release      string
	port         string
	dsn          string
	node         string
	alloc        string
	minSchema    int
	maxSchema    int
	upstreamFile string
	fault        string
	faultDelay   time.Duration
}

type server struct {
	cfg       config
	db        *sql.DB
	binSHA    string
	schema    int
	degraded  atomic.Bool
	readyOnce sync.Once
	client    *http.Client
}

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

func Main(role Role, buildVersion string) {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.SetPrefix(string(role) + " ")
	cfg := config{
		role:         role,
		buildVersion: buildVersion,
		version:      os.Getenv("SERVICE_VERSION"),
		release:      os.Getenv("RELEASE_VERSION"),
		port:         os.Getenv("PORT"),
		dsn:          os.Getenv("DB_DSN"),
		node:         os.Getenv("NODE_NAME"),
		alloc:        os.Getenv("NOMAD_ALLOC_ID"),
		minSchema:    envInt("MIN_SCHEMA", 1),
		maxSchema:    envInt("MAX_SCHEMA", 1<<30),
		upstreamFile: os.Getenv("CATALOG_API_UPSTREAMS"),
		fault:        os.Getenv("FAULT_MODE"),
	}
	if cfg.version == "" {
		cfg.version = buildVersion
	}
	if cfg.port == "" {
		cfg.port = "8080"
	}
	if fn := os.Getenv("FAULT_NODE"); fn != "" && fn != cfg.node {
		cfg.fault = ""
	}
	if d, err := time.ParseDuration(os.Getenv("FAULT_DELAY")); err == nil {
		cfg.faultDelay = d
	}
	if err := run(cfg); err != nil {
		log.Printf("fatal: %v", err)
		os.Exit(1)
	}
}

func binarySHA() string {
	p, err := os.Executable()
	if err != nil {
		return ""
	}
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

func run(cfg config) error {
	s := &server{cfg: cfg, binSHA: binarySHA(), client: &http.Client{Timeout: 700 * time.Millisecond}}
	log.Printf("starting version=%s build=%s release=%s node=%s fault=%q sha256=%.12s", cfg.version, cfg.buildVersion, cfg.release, cfg.node, cfg.fault, s.binSHA)

	switch cfg.fault {
	case "crash_on_start":
		time.Sleep(300 * time.Millisecond)
		log.Printf("fault crash_on_start: exiting")
		os.Exit(3)
	case "slow_start":
		d := cfg.faultDelay
		if d == 0 {
			d = 60 * time.Second
		}
		log.Printf("fault slow_start: sleeping %s before listening", d)
		time.Sleep(d)
	case "oom":
		go hog()
	case "bad_schema":
		cfg.minSchema = 1 << 20
		s.cfg.minSchema = cfg.minSchema
	}

	if cfg.dsn == "" {
		return errors.New("DB_DSN is not set")
	}
	db, err := sql.Open("mysql", cfg.dsn)
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(4)
	db.SetConnMaxLifetime(time.Minute)
	s.db = db
	if err := s.waitDB(20 * time.Second); err != nil {
		return err
	}
	schema, err := s.schemaVersion()
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if schema < cfg.minSchema || schema > cfg.maxSchema {
		return fmt.Errorf("schema %d outside supported range %d-%d", schema, cfg.minSchema, cfg.maxSchema)
	}
	s.schema = schema
	s.record("start")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /version", s.versionHandler)
	mux.HandleFunc("POST /admin/fault", s.adminFault)
	switch cfg.role {
	case RoleAPI:
		mux.HandleFunc("GET /items", s.items)
	case RoleGateway:
		mux.HandleFunc("GET /items", s.proxyItems)
	}

	ln, err := net.Listen("tcp", ":"+cfg.port)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 2 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("serve: %v", err)
		}
	}()
	log.Printf("listening on :%s (schema %d)", cfg.port, schema)

	if cfg.role == RoleWorker {
		go s.workLoop()
	}
	if cfg.fault == "crash_after" {
		d := cfg.faultDelay
		if d == 0 {
			d = 4 * time.Second
		}
		go func() {
			time.Sleep(d)
			log.Printf("fault crash_after: exiting after %s", d)
			os.Exit(5)
		}()
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
	log.Printf("shutting down")
	s.record("stop")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return srv.Shutdown(ctx)
}

// hog allocates until the container's memory limit kills it. It stops at
// 512 MiB so a missing limit shows up as a healthy service, not a node
// running out of memory.
func hog() {
	var keep [][]byte
	for len(keep) < 32 {
		b := make([]byte, 16<<20)
		for i := range b {
			b[i] = byte(i)
		}
		keep = append(keep, b)
		time.Sleep(50 * time.Millisecond)
	}
	log.Printf("fault oom: holding %d MiB and still alive; memory limit not enforced", len(keep)*16)
	select {}
}

func (s *server) waitDB(limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := s.db.PingContext(ctx)
		cancel()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("database not reachable after %s: %w", limit, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (s *server) schemaVersion() (int, error) {
	var v sql.NullInt64
	err := s.db.QueryRow(`SELECT MAX(version) FROM schema_migrations WHERE NOT dirty`).Scan(&v)
	return int(v.Int64), err
}

func (s *server) record(event string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `INSERT INTO service_events (service, version, release_version, alloc_id, node, event) VALUES (?, ?, ?, ?, ?, ?)`,
		string(s.cfg.role), s.cfg.version, s.cfg.release, s.cfg.alloc, s.cfg.node, event)
	if err != nil {
		log.Printf("record %s: %v", event, err)
	}
}

func (s *server) upstreams() []string {
	if s.cfg.upstreamFile == "" {
		return nil
	}
	b, err := os.ReadFile(s.cfg.upstreamFile)
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// upstreamHealthy is true if any catalog-api instance answers its health check.
func (s *server) upstreamHealthy(ctx context.Context) (string, error) {
	ups := s.upstreams()
	if len(ups) == 0 {
		return "", errors.New("no catalog-api instances registered")
	}
	rand.Shuffle(len(ups), func(i, j int) { ups[i], ups[j] = ups[j], ups[i] })
	var last error
	for _, u := range ups {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+u+"/healthz", nil)
		resp, err := s.client.Do(req)
		if err != nil {
			last = err
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return u, nil
		}
		last = fmt.Errorf("%s: %s", u, resp.Status)
	}
	return "", fmt.Errorf("catalog-api unavailable: %w", last)
}

func (s *server) healthz(w http.ResponseWriter, r *http.Request) {
	if s.cfg.fault == "unhealthy" || s.degraded.Load() {
		http.Error(w, "degraded", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 500*time.Millisecond)
	defer cancel()
	if err := s.db.PingContext(ctx); err != nil {
		http.Error(w, "db: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	if s.cfg.role != RoleAPI {
		if _, err := s.upstreamHealthy(ctx); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
	}
	// Recorded before answering, so the row exists before any dependent can
	// see this instance as healthy: ordering evidence then cannot race.
	s.readyOnce.Do(func() { s.record("ready") })
	io.WriteString(w, "ok\n")
}

func (s *server) versionHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"service": string(s.cfg.role), "version": s.cfg.version, "build_version": s.cfg.buildVersion,
		"release": s.cfg.release, "binary_sha256": s.binSHA, "alloc": s.cfg.alloc,
		"node": s.cfg.node, "schema": s.schema,
	})
}

// adminFault lets a test degrade a running instance without redeploying it.
func (s *server) adminFault(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Query().Get("mode") {
	case "unhealthy":
		s.degraded.Store(true)
	case "healthy", "":
		s.degraded.Store(false)
	default:
		http.Error(w, "mode must be unhealthy or healthy", http.StatusBadRequest)
		return
	}
	io.WriteString(w, "ok\n")
}

func (s *server) items(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(), `SELECT sku, name, price_cents FROM items ORDER BY sku LIMIT 100`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	type item struct {
		SKU   string `json:"sku"`
		Name  string `json:"name"`
		Price int    `json:"price_cents"`
	}
	out := []item{}
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.SKU, &it.Name, &it.Price); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		out = append(out, it)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"version": s.cfg.version, "items": out})
}

func (s *server) proxyItems(w http.ResponseWriter, r *http.Request) {
	u, err := s.upstreamHealthy(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	resp, err := s.client.Get("http://" + u + "/items")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Upstream", u)
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// workLoop reads the catalog through catalog-api and records an order now
// and then, so the worker exercises both its upstream and the database.
func (s *server) workLoop() {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for range t.C {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		u, err := s.upstreamHealthy(ctx)
		if err == nil {
			var resp *http.Response
			resp, err = s.client.Get("http://" + u + "/items")
			if err == nil {
				resp.Body.Close()
				_, err = s.db.ExecContext(ctx, `INSERT INTO orders (sku, quantity, worker_version) VALUES ('SKU-1', 1, ?)`, s.cfg.version)
			}
		}
		cancel()
		if err != nil {
			log.Printf("work: %v", err)
		}
	}
}
