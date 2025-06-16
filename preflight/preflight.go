// Package preflight runs the Bash preflight checks in this directory. The
// scripts are embedded in relctl so the binary carries the exact checks it was
// built with; RELCTL_PREFLIGHT_DIR points at a working copy instead.
package preflight

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

//go:embed run.sh lib.sh checks/*.sh sql/*.sql
var scripts embed.FS

type Check struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Status   string `json:"status"` // pass | fail | skip
	Detail   string `json:"detail"`
	Millis   int64  `json:"ms"`
	Category string `json:"category"`
}

type Report struct {
	Passed  bool          `json:"passed"`
	Checks  []Check       `json:"checks"`
	Elapsed time.Duration `json:"elapsed"`
}

func (r *Report) Failed() []Check {
	var out []Check
	for _, c := range r.Checks {
		if c.Status == "fail" {
			out = append(out, c)
		}
	}
	return out
}

func (r *Report) FailedIDs() []string {
	var ids []string
	for _, c := range r.Failed() {
		ids = append(ids, c.ID)
	}
	return ids
}

type Runner struct {
	Dir    string    // working copy of this directory; empty uses the embedded copy
	Output io.Writer // human-readable progress
}

// Extract writes the embedded scripts to dir.
func Extract(dir string) error {
	return fs.WalkDir(scripts, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		dst := filepath.Join(dir, p)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		b, err := scripts.ReadFile(p)
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(p, ".sh") {
			mode = 0o755
		}
		return os.WriteFile(dst, b, mode)
	})
}

func SQL(name string) (string, error) {
	b, err := scripts.ReadFile("sql/" + name)
	return string(b), err
}

func (r *Runner) Run(ctx context.Context, env map[string]string) (*Report, error) {
	start := time.Now()
	dir := r.Dir
	if dir == "" {
		tmp, err := os.MkdirTemp("", "relctl-preflight-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(tmp)
		if err := Extract(tmp); err != nil {
			return nil, err
		}
		dir = tmp
	}
	reportFile, err := os.CreateTemp("", "preflight-report-*.jsonl")
	if err != nil {
		return nil, err
	}
	reportFile.Close()
	defer os.Remove(reportFile.Name())

	cmd := exec.CommandContext(ctx, "bash", filepath.Join(dir, "run.sh"))
	cmd.Env = os.Environ()
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		cmd.Env = append(cmd.Env, k+"="+env[k])
	}
	cmd.Env = append(cmd.Env, "PREFLIGHT_REPORT="+reportFile.Name(), "PREFLIGHT_DIR="+dir)
	var stderr bytes.Buffer
	out := r.Output
	if out == nil {
		out = io.Discard
	}
	cmd.Stdout = out
	cmd.Stderr = io.MultiWriter(out, &stderr)
	runErr := cmd.Run()

	rep := &Report{Elapsed: time.Since(start)}
	f, err := os.Open(reportFile.Name())
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var c Check
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			return nil, fmt.Errorf("preflight report line %q: %w", sc.Text(), err)
		}
		rep.Checks = append(rep.Checks, c)
	}
	var exit *exec.ExitError
	switch {
	case runErr == nil:
		rep.Passed = len(rep.Failed()) == 0
	case errors.As(runErr, &exit) && exit.ExitCode() == 1:
		rep.Passed = false
	default:
		return rep, fmt.Errorf("preflight did not run: %v: %s", runErr, strings.TrimSpace(stderr.String()))
	}
	if !rep.Passed && len(rep.Failed()) == 0 {
		return rep, fmt.Errorf("preflight exited 1 without reporting a failed check")
	}
	return rep, nil
}
