// Package migrate applies the schema migrations a signed release carries.
// Each migration is recorded with its checksum and a dirty flag that stays
// set if the migration fails half way, so a later release refuses to run on
// top of a schema in an unknown state.
package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Abhayrkhot/release-orchestrator/internal/manifest"
)

const createTable = `CREATE TABLE IF NOT EXISTS schema_migrations (
  version    INT PRIMARY KEY,
  name       VARCHAR(128) NOT NULL,
  checksum   CHAR(64) NOT NULL,
  dirty      BOOLEAN NOT NULL DEFAULT TRUE,
  applied_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
)`

// FingerprintSQL fingerprints the app schema (columns and indexes), excluding
// the migrations table. The preflight checks run the same query from
// preflight/sql/schema_fingerprint.sql; TestFingerprintSQLMatchesPreflight
// keeps the two identical.
const FingerprintSQL = `SELECT SHA2(CONCAT(
  IFNULL((SELECT GROUP_CONCAT(CONCAT_WS(':', TABLE_NAME, COLUMN_NAME, COLUMN_TYPE, IS_NULLABLE, IFNULL(COLUMN_DEFAULT, '<null>'), EXTRA)
          ORDER BY TABLE_NAME, ORDINAL_POSITION SEPARATOR '|')
          FROM information_schema.COLUMNS
          WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME <> 'schema_migrations'), ''),
  '#',
  IFNULL((SELECT GROUP_CONCAT(CONCAT_WS(':', TABLE_NAME, INDEX_NAME, SEQ_IN_INDEX, COLUMN_NAME, NON_UNIQUE)
          ORDER BY TABLE_NAME, INDEX_NAME, SEQ_IN_INDEX SEPARATOR '|')
          FROM information_schema.STATISTICS
          WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME <> 'schema_migrations'), '')
), 256)`

type Applied struct {
	Version  int
	Name     string
	Checksum string
	Dirty    bool
}

type Migrator struct{ DB *sql.DB }

func (m *Migrator) Ensure(ctx context.Context) error {
	_, err := m.DB.ExecContext(ctx, createTable)
	return err
}

func (m *Migrator) Applied(ctx context.Context) ([]Applied, error) {
	if err := m.Ensure(ctx); err != nil {
		return nil, err
	}
	rows, err := m.DB.QueryContext(ctx, `SELECT version, name, checksum, dirty FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Applied
	for rows.Next() {
		var a Applied
		if err := rows.Scan(&a.Version, &a.Name, &a.Checksum, &a.Dirty); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Current is the highest clean version, and whether any row is dirty.
func (m *Migrator) Current(ctx context.Context) (version int, dirty bool, err error) {
	as, err := m.Applied(ctx)
	if err != nil {
		return 0, false, err
	}
	for _, a := range as {
		if a.Dirty {
			return a.Version, true, nil
		}
		version = a.Version
	}
	return version, false, nil
}

var ErrDirty = errors.New("schema is dirty: a previous migration failed part way")

type Result struct {
	From, To int
	Applied  []int
}

// Up applies the bundle's migrations above the current version. It refuses to
// run if the schema is dirty or if an applied migration's checksum differs
// from the one in the manifest.
func (m *Migrator) Up(ctx context.Context, b *manifest.Bundle) (*Result, error) {
	applied, err := m.Applied(ctx)
	if err != nil {
		return nil, err
	}
	byVer := map[int]Applied{}
	cur := 0
	for _, a := range applied {
		if a.Dirty {
			return nil, fmt.Errorf("%w (version %d %s)", ErrDirty, a.Version, a.Name)
		}
		byVer[a.Version] = a
		cur = a.Version
	}
	res := &Result{From: cur, To: cur}
	if cur > b.Manifest.Schema.Target {
		return nil, fmt.Errorf("database is at schema %d, release targets %d", cur, b.Manifest.Schema.Target)
	}
	for _, mg := range b.Manifest.Schema.Migrations {
		if a, ok := byVer[mg.Version]; ok {
			if a.Checksum != mg.SHA256 {
				return nil, fmt.Errorf("migration %d checksum drift: applied %s, release has %s", mg.Version, a.Checksum, mg.SHA256)
			}
			continue
		}
		body, err := os.ReadFile(filepath.Join(b.Dir, mg.File))
		if err != nil {
			return nil, err
		}
		if manifest.SHA256Bytes(body) != mg.SHA256 {
			return nil, fmt.Errorf("migration %d file checksum mismatch", mg.Version)
		}
		// Mark dirty first: MySQL DDL commits implicitly, so a failure in the
		// middle of a multi-statement migration cannot be rolled back.
		if _, err := m.DB.ExecContext(ctx, `INSERT INTO schema_migrations (version, name, checksum, dirty) VALUES (?, ?, ?, TRUE)`,
			mg.Version, mg.Name, mg.SHA256); err != nil {
			return res, err
		}
		if _, err := m.DB.ExecContext(ctx, string(body)); err != nil {
			return res, fmt.Errorf("migration %d %s failed, schema left dirty: %w", mg.Version, mg.Name, err)
		}
		if _, err := m.DB.ExecContext(ctx, `UPDATE schema_migrations SET dirty = FALSE WHERE version = ?`, mg.Version); err != nil {
			return res, err
		}
		res.Applied = append(res.Applied, mg.Version)
		res.To = mg.Version
	}
	return res, nil
}

// Repair clears a dirty row after an operator fixed the schema by hand. With
// drop=true the row is removed so the migration runs again.
func (m *Migrator) Repair(ctx context.Context, version int, drop bool) error {
	q := `UPDATE schema_migrations SET dirty = FALSE WHERE version = ? AND dirty`
	if drop {
		q = `DELETE FROM schema_migrations WHERE version = ? AND dirty`
	}
	r, err := m.DB.ExecContext(ctx, q, version)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return fmt.Errorf("no dirty migration at version %d", version)
	}
	return nil
}

func (m *Migrator) Fingerprint(ctx context.Context) (string, error) {
	// The session variable must be set on the same connection that runs the
	// query, so pin one connection from the pool.
	conn, err := m.DB.Conn(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SET SESSION group_concat_max_len = 1048576`); err != nil {
		return "", err
	}
	var fp string
	err = conn.QueryRowContext(ctx, FingerprintSQL).Scan(&fp)
	return fp, err
}
