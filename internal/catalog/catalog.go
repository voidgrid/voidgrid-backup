// Package catalog is the server's SQLite store: configuration and history.
// Snapshot data itself lives in the backup repository, not here.
package catalog

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

var ErrNotFound = errors.New("not found")

type Catalog struct{ db *sql.DB }

type Agent struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Address         string    `json:"address"`
	Hostname        string    `json:"hostname"`
	Version         string    `json:"version"`
	CertFingerprint string    `json:"cert_fingerprint"`
	EnrolledAt      time.Time `json:"enrolled_at"`
	LastSeen        time.Time `json:"last_seen"` // zero if never reached
	LastError       string    `json:"last_error"`
	Warnings        []string  `json:"warnings"` // e.g. missing capabilities, from the last check
}

// Open opens (creating if needed) the catalog at path and applies migrations.
func Open(path string) (*Catalog, error) {
	db, err := sql.Open("sqlite", "file:"+path+
		"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	// One connection: SQLite serializes writers anyway, and this rules out
	// SQLITE_BUSY between our own goroutines.
	db.SetMaxOpenConns(1)
	c := &Catalog{db: db}
	if err := c.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	// The catalog holds repository passwords. SQLite gives its -wal/-shm
	// files the database file's mode, so this covers them too.
	if err := os.Chmod(path, 0o600); err != nil {
		db.Close()
		return nil, err
	}
	return c, nil
}

func (c *Catalog) Close() error { return c.db.Close() }

// Ping checks that the database answers a query.
func (c *Catalog) Ping(ctx context.Context) error {
	var one int
	return c.db.QueryRowContext(ctx, `SELECT 1`).Scan(&one)
}

func (c *Catalog) migrate() error {
	if _, err := c.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL) STRICT`); err != nil {
		return err
	}
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		num, _, _ := strings.Cut(e.Name(), "_")
		v, err := strconv.Atoi(num)
		if err != nil {
			return fmt.Errorf("migration %s: name must start with a number", e.Name())
		}
		var applied int
		if err := c.db.QueryRow(`SELECT count(*) FROM schema_migrations WHERE version = ?`, v).Scan(&applied); err != nil {
			return err
		}
		if applied > 0 {
			continue
		}
		stmt, err := fs.ReadFile(migrations, "migrations/"+e.Name())
		if err != nil {
			return err
		}
		tx, err := c.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(stmt)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: %w", e.Name(), err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			v, formatTime(time.Now())); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (c *Catalog) AddAgent(ctx context.Context, a Agent) error {
	_, err := c.db.ExecContext(ctx, `INSERT INTO agents
		(id, name, address, hostname, version, cert_fingerprint, enrolled_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.Name, a.Address, a.Hostname, a.Version, a.CertFingerprint, formatTime(a.EnrolledAt))
	return err
}

const agentCols = `id, name, address, hostname, version, cert_fingerprint, enrolled_at, last_seen, last_error, warnings`

func (c *Catalog) Agent(ctx context.Context, id string) (Agent, error) {
	return scanAgent(c.db.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE id = ?`, id))
}

func (c *Catalog) AgentByName(ctx context.Context, name string) (Agent, error) {
	return scanAgent(c.db.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE name = ?`, name))
}

func (c *Catalog) ListAgents(ctx context.Context) ([]Agent, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT `+agentCols+` FROM agents ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// RecordSeen notes a successful check and clears any previous error.
func (c *Catalog) RecordSeen(ctx context.Context, id string, at time.Time, hostname, version string, warnings []string) error {
	return c.update(ctx, `UPDATE agents SET last_seen = ?, last_error = '', hostname = ?, version = ?, warnings = ? WHERE id = ?`,
		formatTime(at), hostname, version, strings.Join(warnings, "\n"), id)
}

// RecordFailure notes a failed check, keeping the last successful time.
func (c *Catalog) RecordFailure(ctx context.Context, id string, msg string) error {
	return c.update(ctx, `UPDATE agents SET last_error = ? WHERE id = ?`, msg, id)
}

func (c *Catalog) update(ctx context.Context, q string, args ...any) error {
	res, err := c.db.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

type scanner interface{ Scan(...any) error }

func scanAgent(s scanner) (Agent, error) {
	var a Agent
	var enrolled string
	var seen sql.NullString
	var warnings string
	err := s.Scan(&a.ID, &a.Name, &a.Address, &a.Hostname, &a.Version, &a.CertFingerprint, &enrolled, &seen, &a.LastError, &warnings)
	if warnings != "" {
		a.Warnings = strings.Split(warnings, "\n")
	}
	if errors.Is(err, sql.ErrNoRows) {
		return Agent{}, ErrNotFound
	}
	if err != nil {
		return Agent{}, err
	}
	if a.EnrolledAt, err = time.Parse(time.RFC3339Nano, enrolled); err != nil {
		return Agent{}, err
	}
	if seen.Valid {
		if a.LastSeen, err = time.Parse(time.RFC3339Nano, seen.String); err != nil {
			return Agent{}, err
		}
	}
	return a, nil
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// Settings is a small key-value store for server-wide configuration that
// isn't tied to a single agent, repository or job (e.g. notification
// settings). A missing key is not an error: GetSetting returns "".

// GetSetting returns the value for key, or "" if it was never set.
func (c *Catalog) GetSetting(ctx context.Context, key string) (string, error) {
	var v string
	err := c.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetSetting stores value for key, creating or replacing it.
func (c *Catalog) SetSetting(ctx context.Context, key, value string) error {
	_, err := c.db.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}
