// Package store keeps release history, the deploy lock and schema snapshots
// in MySQL (database "relctl"), so a restarted controller knows what is
// deployed and whether a release was interrupted.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const schema = `
CREATE TABLE IF NOT EXISTS releases (
  id              BIGINT AUTO_INCREMENT PRIMARY KEY,
  kind            VARCHAR(16) NOT NULL,
  version         VARCHAR(32) NOT NULL,
  manifest_sha256 CHAR(64) NOT NULL,
  manifest        MEDIUMBLOB NOT NULL,
  signature       BLOB NOT NULL,
  key_id          VARCHAR(32) NOT NULL,
  status          VARCHAR(16) NOT NULL,
  stage           VARCHAR(64) NOT NULL DEFAULT '',
  error           TEXT NULL,
  previous_id     BIGINT NULL,
  started_at      DATETIME(3) NOT NULL,
  finished_at     DATETIME(3) NULL,
  KEY idx_status (status)
);
CREATE TABLE IF NOT EXISTS release_events (
  id         BIGINT AUTO_INCREMENT PRIMARY KEY,
  release_id BIGINT NOT NULL,
  at         DATETIME(3) NOT NULL,
  service    VARCHAR(64) NOT NULL DEFAULT '',
  kind       VARCHAR(32) NOT NULL,
  detail     TEXT NULL,
  KEY idx_release (release_id)
);
CREATE TABLE IF NOT EXISTS release_lock (
  id           TINYINT PRIMARY KEY,
  owner        VARCHAR(128) NULL,
  release_id   BIGINT NULL,
  acquired_at  DATETIME(3) NULL,
  heartbeat_at DATETIME(3) NULL
);
INSERT IGNORE INTO release_lock (id) VALUES (1);
CREATE TABLE IF NOT EXISTS schema_snapshots (
  id             BIGINT AUTO_INCREMENT PRIMARY KEY,
  schema_version INT NOT NULL,
  fingerprint    CHAR(64) NOT NULL,
  release_id     BIGINT NULL,
  taken_at       DATETIME(3) NOT NULL
);`

const (
	StatusInProgress = "in_progress"
	StatusSucceeded  = "succeeded"
	StatusRolledBack = "rolled_back" // failed, and the previous release was restored
	StatusFailed     = "failed"      // failed, and restoring the previous release also failed
	StatusAborted    = "aborted"     // stopped before any service changed
	StatusReconciled = "reconciled"  // interrupted, then cleaned up by `relctl reconcile`

	KindDeploy    = "deploy"
	KindRollback  = "rollback"
	KindReconcile = "reconcile"
)

type Store struct{ DB *sql.DB }

type Release struct {
	ID             int64
	Kind           string
	Version        string
	ManifestSHA256 string
	Manifest       []byte
	Signature      []byte
	KeyID          string
	Status         string
	Stage          string
	Error          string
	PreviousID     sql.NullInt64
	StartedAt      time.Time
	FinishedAt     sql.NullTime
}

type Event struct {
	At      time.Time `json:"at"`
	Service string    `json:"service,omitempty"`
	Kind    string    `json:"kind"`
	Detail  string    `json:"detail,omitempty"`
}

func (s *Store) Init(ctx context.Context) error {
	// The driver runs one statement per Exec unless multiStatements is set,
	// so split on the blank-line-free ";\n" boundaries above.
	for _, stmt := range splitSQL(schema) {
		if _, err := s.DB.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("init relctl schema: %w", err)
		}
	}
	return nil
}

func splitSQL(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ';' {
			if st := trim(s[start:i]); st != "" {
				out = append(out, st)
			}
			start = i + 1
		}
	}
	if st := trim(s[start:]); st != "" {
		out = append(out, st)
	}
	return out
}

func trim(s string) string {
	i, j := 0, len(s)
	for i < j && (s[i] == ' ' || s[i] == '\n' || s[i] == '\t') {
		i++
	}
	for j > i && (s[j-1] == ' ' || s[j-1] == '\n' || s[j-1] == '\t') {
		j--
	}
	return s[i:j]
}

const releaseCols = `id, kind, version, manifest_sha256, manifest, signature, key_id, status, stage, IFNULL(error, ''), previous_id, started_at, finished_at`

func scanRelease(row interface{ Scan(...any) error }) (*Release, error) {
	var r Release
	err := row.Scan(&r.ID, &r.Kind, &r.Version, &r.ManifestSHA256, &r.Manifest, &r.Signature, &r.KeyID,
		&r.Status, &r.Stage, &r.Error, &r.PreviousID, &r.StartedAt, &r.FinishedAt)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// Current is the release the cluster is supposed to be running: the latest
// one that finished successfully (a deploy or an operator rollback).
func (s *Store) Current(ctx context.Context) (*Release, error) {
	r, err := scanRelease(s.DB.QueryRowContext(ctx,
		`SELECT `+releaseCols+` FROM releases WHERE status = ? ORDER BY id DESC LIMIT 1`, StatusSucceeded))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

func (s *Store) Get(ctx context.Context, id int64) (*Release, error) {
	return scanRelease(s.DB.QueryRowContext(ctx, `SELECT `+releaseCols+` FROM releases WHERE id = ?`, id))
}

// SucceededBefore returns the newest succeeded release older than id whose
// version differs from skipVersion.
func (s *Store) SucceededBefore(ctx context.Context, id int64, skipVersion string) (*Release, error) {
	r, err := scanRelease(s.DB.QueryRowContext(ctx,
		`SELECT `+releaseCols+` FROM releases WHERE status = ? AND id < ? AND version <> ? ORDER BY id DESC LIMIT 1`,
		StatusSucceeded, id, skipVersion))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

func (s *Store) LatestSucceededVersion(ctx context.Context, version string) (*Release, error) {
	r, err := scanRelease(s.DB.QueryRowContext(ctx,
		`SELECT `+releaseCols+` FROM releases WHERE status = ? AND version = ? ORDER BY id DESC LIMIT 1`,
		StatusSucceeded, version))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

func (s *Store) InProgress(ctx context.Context) ([]*Release, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+releaseCols+` FROM releases WHERE status = ? ORDER BY id`, StatusInProgress)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Release
	for rows.Next() {
		r, err := scanRelease(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) History(ctx context.Context, limit int) ([]*Release, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+releaseCols+` FROM releases ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Release
	for rows.Next() {
		r, err := scanRelease(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) Begin(ctx context.Context, kind, version, sha string, manifest, sig []byte, keyID string, previous *Release) (int64, error) {
	var prev sql.NullInt64
	if previous != nil {
		prev = sql.NullInt64{Int64: previous.ID, Valid: true}
	}
	res, err := s.DB.ExecContext(ctx, `INSERT INTO releases (kind, version, manifest_sha256, manifest, signature, key_id, status, stage, previous_id, started_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'starting', ?, NOW(3))`, kind, version, sha, manifest, sig, keyID, StatusInProgress, prev)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) SetStage(ctx context.Context, id int64, stage string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE releases SET stage = ? WHERE id = ?`, stage, id)
	return err
}

func (s *Store) Finish(ctx context.Context, id int64, status, errMsg string) error {
	var e sql.NullString
	if errMsg != "" {
		e = sql.NullString{String: errMsg, Valid: true}
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE releases SET status = ?, error = ?, finished_at = NOW(3) WHERE id = ?`, status, e, id)
	return err
}

func (s *Store) AddEvent(ctx context.Context, id int64, service, kind, detail string) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO release_events (release_id, at, service, kind, detail) VALUES (?, NOW(3), ?, ?, ?)`,
		id, service, kind, detail)
	return err
}

func (s *Store) Events(ctx context.Context, id int64) ([]Event, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT at, service, kind, IFNULL(detail, '') FROM release_events WHERE release_id = ? ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.At, &e.Service, &e.Kind, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ErrLocked means another controller holds a live lease.
var ErrLocked = errors.New("another release holds the deploy lock")

type LockInfo struct {
	Owner     string
	ReleaseID sql.NullInt64
	Heartbeat sql.NullTime
	Stale     bool
}

// Acquire takes the deploy lease if it is free or its holder stopped
// heartbeating more than ttl ago.
func (s *Store) Acquire(ctx context.Context, owner string, ttl time.Duration) error {
	res, err := s.DB.ExecContext(ctx, `UPDATE release_lock SET owner = ?, release_id = NULL, acquired_at = NOW(3), heartbeat_at = NOW(3)
		WHERE id = 1 AND (owner IS NULL OR heartbeat_at < NOW(3) - INTERVAL ? MICROSECOND)`, owner, ttl.Microseconds())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		li, _ := s.Lock(ctx, ttl)
		if li != nil {
			return fmt.Errorf("%w (owner %s)", ErrLocked, li.Owner)
		}
		return ErrLocked
	}
	return nil
}

func (s *Store) AttachLock(ctx context.Context, owner string, releaseID int64) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE release_lock SET release_id = ? WHERE id = 1 AND owner = ?`, releaseID, owner)
	return err
}

// Heartbeat renews the lease; it returns an error if the lease was lost.
func (s *Store) Heartbeat(ctx context.Context, owner string) error {
	res, err := s.DB.ExecContext(ctx, `UPDATE release_lock SET heartbeat_at = NOW(3) WHERE id = 1 AND owner = ?`, owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		// MySQL reports 0 rows when the value did not change (same ms). Check.
		li, err := s.Lock(ctx, time.Hour)
		if err != nil {
			return err
		}
		if li.Owner != owner {
			return fmt.Errorf("deploy lock lost to %q", li.Owner)
		}
	}
	return nil
}

func (s *Store) Release(ctx context.Context, owner string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE release_lock SET owner = NULL, release_id = NULL, acquired_at = NULL, heartbeat_at = NULL WHERE id = 1 AND owner = ?`, owner)
	return err
}

func (s *Store) Lock(ctx context.Context, ttl time.Duration) (*LockInfo, error) {
	var li LockInfo
	var owner sql.NullString
	var stale sql.NullBool
	err := s.DB.QueryRowContext(ctx, `SELECT owner, release_id, heartbeat_at, heartbeat_at < NOW(3) - INTERVAL ? MICROSECOND FROM release_lock WHERE id = 1`,
		ttl.Microseconds()).Scan(&owner, &li.ReleaseID, &li.Heartbeat, &stale)
	if err != nil {
		return nil, err
	}
	li.Owner = owner.String
	li.Stale = stale.Bool
	return &li, nil
}

func (s *Store) SaveSnapshot(ctx context.Context, version int, fingerprint string, releaseID int64) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO schema_snapshots (schema_version, fingerprint, release_id, taken_at) VALUES (?, ?, ?, NOW(3))`,
		version, fingerprint, releaseID)
	return err
}
