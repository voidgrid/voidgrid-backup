package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/repocfg"
)

type Repository struct {
	ID            string
	Name          string
	Config        repocfg.Config
	Password      string
	CreatedAt     time.Time
	InitializedAt time.Time // zero until an agent has created/connected it
}

type Retention struct {
	Latest, Hourly, Daily, Weekly, Monthly, Annual int
}

// DefaultRetention matches Kopia's own defaults.
var DefaultRetention = Retention{Latest: 10, Hourly: 48, Daily: 7, Weekly: 4, Monthly: 24, Annual: 3}

// Job kinds.
const (
	JobPaths = "paths"
	JobStack = "stack"
	JobVM    = "vm"
)

// VMConfig is what a VM job backs up for one libvirt domain.
type VMConfig struct {
	Name    string   `json:"name"`
	Disks   []string `json:"disks"` // targets; empty = every backupable disk
	Quiesce bool     `json:"quiesce"`
}

type Job struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	Kind         string       `json:"kind"`
	AgentID      string       `json:"agent_id"`
	RepositoryID string       `json:"repository_id"`
	Paths        []string     `json:"paths"`
	Excludes     []string     `json:"excludes"`
	Stack        *StackConfig `json:"stack,omitempty"`
	VM           *VMConfig    `json:"vm,omitempty"`
	Schedule     string       `json:"schedule"`
	Keep         Retention    `json:"keep"`
	Enabled      bool         `json:"enabled"`
	CreatedAt    time.Time    `json:"created_at"`
}

// StackConfig is what a stack job backs up for one compose project.
type StackConfig struct {
	Project    string            `json:"project"`
	WorkingDir string            `json:"working_dir"`
	Include    []string          `json:"include"` // mount host paths to back up
	Exclude    []string          `json:"exclude"` // mount host paths to leave out
	Dumps      map[string]string `json:"dumps"`   // service -> dump kind
	Quiesce    string            `json:"quiesce"`
}

const (
	RunRunning = "running"
	RunSuccess = "success"
	RunPartial = "partial" // some paths failed
	RunFailed  = "failed"
)

type Run struct {
	ID         int64     `json:"id"`
	JobID      string    `json:"job_id"`
	Kind       string    `json:"kind"`
	Trigger    string    `json:"trigger"`
	Status     string    `json:"status"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Bytes      int64     `json:"bytes"`
	Files      int       `json:"files"`
	Summary    string    `json:"summary"`
	Error      string    `json:"error"`
}

// Repositories

func (c *Catalog) AddRepository(ctx context.Context, r Repository) error {
	cfg, err := json.Marshal(r.Config)
	if err != nil {
		return err
	}
	_, err = c.db.ExecContext(ctx, `INSERT INTO repositories (id, name, config, password, created_at) VALUES (?, ?, ?, ?, ?)`,
		r.ID, r.Name, string(cfg), r.Password, formatTime(r.CreatedAt))
	return err
}

// MarkRepositoryInitialized records the (possibly updated) config, e.g. an
// SFTP host key pinned on first use.
func (c *Catalog) MarkRepositoryInitialized(ctx context.Context, id string, cfg repocfg.Config, at time.Time) error {
	b, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return c.update(ctx, `UPDATE repositories SET config = ?, initialized_at = ? WHERE id = ?`, string(b), formatTime(at), id)
}

// DeleteRepository removes a repository record that no job uses. The data
// in storage is untouched.
func (c *Catalog) DeleteRepository(ctx context.Context, id string) error {
	return c.update(ctx, `DELETE FROM repositories WHERE id = ?`, id)
}

const repoCols = `id, name, config, password, created_at, initialized_at`

func (c *Catalog) Repository(ctx context.Context, id string) (Repository, error) {
	return scanRepository(c.db.QueryRowContext(ctx, `SELECT `+repoCols+` FROM repositories WHERE id = ?`, id))
}

func (c *Catalog) RepositoryByName(ctx context.Context, name string) (Repository, error) {
	return scanRepository(c.db.QueryRowContext(ctx, `SELECT `+repoCols+` FROM repositories WHERE name = ?`, name))
}

func (c *Catalog) ListRepositories(ctx context.Context) ([]Repository, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT `+repoCols+` FROM repositories ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Repository
	for rows.Next() {
		r, err := scanRepository(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanRepository(s scanner) (Repository, error) {
	var r Repository
	var cfg, created string
	var initialized sql.NullString
	err := s.Scan(&r.ID, &r.Name, &cfg, &r.Password, &created, &initialized)
	if errors.Is(err, sql.ErrNoRows) {
		return Repository{}, ErrNotFound
	}
	if err != nil {
		return Repository{}, err
	}
	if err := json.Unmarshal([]byte(cfg), &r.Config); err != nil {
		return Repository{}, err
	}
	if r.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return Repository{}, err
	}
	if initialized.Valid {
		if r.InitializedAt, err = time.Parse(time.RFC3339Nano, initialized.String); err != nil {
			return Repository{}, err
		}
	}
	return r, nil
}

// Jobs

// jobJSON is how a job's list and config fields are stored: JSON text, with
// "" for a stack or VM config the job doesn't have.
func jobJSON(j Job) (paths, excludes, stack, vm string, err error) {
	if paths, excludes, err = marshalLists(j.Paths, j.Excludes); err != nil {
		return
	}
	if j.Stack != nil {
		var b []byte
		if b, err = json.Marshal(j.Stack); err != nil {
			return
		}
		stack = string(b)
	}
	if j.VM != nil {
		var b []byte
		if b, err = json.Marshal(j.VM); err != nil {
			return
		}
		vm = string(b)
	}
	return
}

func (c *Catalog) AddJob(ctx context.Context, j Job) error {
	paths, excludes, stack, vm, err := jobJSON(j)
	if err != nil {
		return err
	}
	kind := j.Kind
	if kind == "" {
		kind = JobPaths
	}
	_, err = c.db.ExecContext(ctx, `INSERT INTO jobs
		(id, name, kind, agent_id, repository_id, paths, excludes, stack, vm, schedule,
		 keep_latest, keep_hourly, keep_daily, keep_weekly, keep_monthly, keep_annual, enabled, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		j.ID, j.Name, kind, j.AgentID, j.RepositoryID, paths, excludes, stack, vm, j.Schedule,
		j.Keep.Latest, j.Keep.Hourly, j.Keep.Daily, j.Keep.Weekly, j.Keep.Monthly, j.Keep.Annual,
		boolInt(j.Enabled), formatTime(j.CreatedAt))
	return err
}

// UpdateJob rewrites what a job backs up and how: name, paths, excludes, stack
// or VM config, schedule, retention and enabled. Its identity (id, kind, agent,
// repository, creation time) is never changed here.
func (c *Catalog) UpdateJob(ctx context.Context, j Job) error {
	paths, excludes, stack, vm, err := jobJSON(j)
	if err != nil {
		return err
	}
	return c.update(ctx, `UPDATE jobs SET name = ?, paths = ?, excludes = ?, stack = ?, vm = ?, schedule = ?,
		keep_latest = ?, keep_hourly = ?, keep_daily = ?, keep_weekly = ?, keep_monthly = ?, keep_annual = ?, enabled = ?
		WHERE id = ?`,
		j.Name, paths, excludes, stack, vm, j.Schedule,
		j.Keep.Latest, j.Keep.Hourly, j.Keep.Daily, j.Keep.Weekly, j.Keep.Monthly, j.Keep.Annual,
		boolInt(j.Enabled), j.ID)
}

func (c *Catalog) SetJobEnabled(ctx context.Context, id string, enabled bool) error {
	return c.update(ctx, `UPDATE jobs SET enabled = ? WHERE id = ?`, boolInt(enabled), id)
}

// DeleteJob removes a job and its run history. Snapshots in the repository
// are untouched.
func (c *Catalog) DeleteJob(ctx context.Context, id string) error {
	return c.update(ctx, `DELETE FROM jobs WHERE id = ?`, id)
}

const jobCols = `id, name, kind, agent_id, repository_id, paths, excludes, stack, vm, schedule,
	keep_latest, keep_hourly, keep_daily, keep_weekly, keep_monthly, keep_annual, enabled, created_at`

func (c *Catalog) Job(ctx context.Context, id string) (Job, error) {
	return scanJob(c.db.QueryRowContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE id = ?`, id))
}

func (c *Catalog) JobByName(ctx context.Context, name string) (Job, error) {
	return scanJob(c.db.QueryRowContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE name = ?`, name))
}

func (c *Catalog) ListJobs(ctx context.Context) ([]Job, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT `+jobCols+` FROM jobs ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func scanJob(s scanner) (Job, error) {
	var j Job
	var paths, excludes, stack, vm, created string
	var enabled int
	err := s.Scan(&j.ID, &j.Name, &j.Kind, &j.AgentID, &j.RepositoryID, &paths, &excludes, &stack, &vm, &j.Schedule,
		&j.Keep.Latest, &j.Keep.Hourly, &j.Keep.Daily, &j.Keep.Weekly, &j.Keep.Monthly, &j.Keep.Annual,
		&enabled, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, err
	}
	if stack != "" {
		j.Stack = &StackConfig{}
		if err := json.Unmarshal([]byte(stack), j.Stack); err != nil {
			return Job{}, err
		}
	}
	if vm != "" {
		j.VM = &VMConfig{}
		if err := json.Unmarshal([]byte(vm), j.VM); err != nil {
			return Job{}, err
		}
	}
	if err := json.Unmarshal([]byte(paths), &j.Paths); err != nil {
		return Job{}, err
	}
	if err := json.Unmarshal([]byte(excludes), &j.Excludes); err != nil {
		return Job{}, err
	}
	j.Enabled = enabled != 0
	if j.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return Job{}, err
	}
	return j, nil
}

// Runs

func (c *Catalog) StartRun(ctx context.Context, jobID, kind, trigger string, at time.Time) (int64, error) {
	res, err := c.db.ExecContext(ctx, `INSERT INTO runs (job_id, kind, trigger, status, started_at) VALUES (?, ?, ?, ?, ?)`,
		jobID, kind, trigger, RunRunning, formatTime(at))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (c *Catalog) FinishRun(ctx context.Context, r Run) error {
	return c.update(ctx, `UPDATE runs SET status = ?, finished_at = ?, bytes = ?, files = ?, summary = ?, error = ? WHERE id = ?`,
		r.Status, formatTime(r.FinishedAt), r.Bytes, r.Files, r.Summary, r.Error, r.ID)
}

// FailInterruptedRuns marks runs left "running" by a previous server process.
func (c *Catalog) FailInterruptedRuns(ctx context.Context, at time.Time) (int64, error) {
	res, err := c.db.ExecContext(ctx, `UPDATE runs SET status = ?, finished_at = ?, error = 'server restarted during the run' WHERE status = ?`,
		RunFailed, formatTime(at), RunRunning)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

const runCols = `id, job_id, kind, trigger, status, started_at, finished_at, bytes, files, summary, error`

func (c *Catalog) ListRuns(ctx context.Context, jobID string, limit int) ([]Run, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT `+runCols+` FROM runs WHERE job_id = ? ORDER BY started_at DESC, id DESC LIMIT ?`, jobID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LastRun returns the most recent run of kind for the job.
func (c *Catalog) LastRun(ctx context.Context, jobID, kind string) (Run, error) {
	return scanRun(c.db.QueryRowContext(ctx, `SELECT `+runCols+` FROM runs WHERE job_id = ? AND kind = ? ORDER BY started_at DESC, id DESC LIMIT 1`, jobID, kind))
}

func scanRun(s scanner) (Run, error) {
	var r Run
	var started string
	var finished sql.NullString
	err := s.Scan(&r.ID, &r.JobID, &r.Kind, &r.Trigger, &r.Status, &started, &finished, &r.Bytes, &r.Files, &r.Summary, &r.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	if err != nil {
		return Run{}, err
	}
	if r.StartedAt, err = time.Parse(time.RFC3339Nano, started); err != nil {
		return Run{}, err
	}
	if finished.Valid {
		if r.FinishedAt, err = time.Parse(time.RFC3339Nano, finished.String); err != nil {
			return Run{}, err
		}
	}
	return r, nil
}

func marshalLists(a, b []string) (string, string, error) {
	if a == nil {
		a = []string{}
	}
	if b == nil {
		b = []string{}
	}
	ja, err := json.Marshal(a)
	if err != nil {
		return "", "", err
	}
	jb, err := json.Marshal(b)
	return string(ja), string(jb), err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
