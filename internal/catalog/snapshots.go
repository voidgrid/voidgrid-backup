package catalog

import (
	"context"
	"time"
)

// Snapshot is one snapshot of a job's source, as the UI shows it.
type Snapshot struct {
	ID         string
	Path       string
	Start, End time.Time
	Bytes      int64
	Files      int
	Errors     int
	Incomplete string
}

const upsertSnapshot = `INSERT INTO snapshots (job_id, snapshot_id, path, started_at, ended_at, bytes, files, errors, incomplete)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT (job_id, snapshot_id) DO UPDATE SET path = excluded.path, started_at = excluded.started_at,
	  ended_at = excluded.ended_at, bytes = excluded.bytes, files = excluded.files,
	  errors = excluded.errors, incomplete = excluded.incomplete`

// UpsertSnapshots records snapshots for a job, replacing any with the same ID,
// and forgets the ones in deleteIDs (what retention just removed).
func (c *Catalog) UpsertSnapshots(ctx context.Context, jobID string, snaps []Snapshot, deleteIDs []string) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, s := range snaps {
		if _, err := tx.ExecContext(ctx, upsertSnapshot, jobID, s.ID, s.Path, formatTime(s.Start), formatTime(s.End),
			s.Bytes, s.Files, s.Errors, s.Incomplete); err != nil {
			return err
		}
	}
	for _, id := range deleteIDs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM snapshots WHERE job_id = ? AND snapshot_id = ?`, jobID, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ReplaceSnapshots makes snaps the whole record for a job, as after listing
// the repository.
func (c *Catalog) ReplaceSnapshots(ctx context.Context, jobID string, snaps []Snapshot) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM snapshots WHERE job_id = ?`, jobID); err != nil {
		return err
	}
	for _, s := range snaps {
		if _, err := tx.ExecContext(ctx, upsertSnapshot, jobID, s.ID, s.Path, formatTime(s.Start), formatTime(s.End),
			s.Bytes, s.Files, s.Errors, s.Incomplete); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListSnapshots returns a job's recorded snapshots, newest first.
func (c *Catalog) ListSnapshots(ctx context.Context, jobID string) ([]Snapshot, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT snapshot_id, path, started_at, ended_at, bytes, files, errors, incomplete
		FROM snapshots WHERE job_id = ? ORDER BY started_at DESC, snapshot_id`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Snapshot
	for rows.Next() {
		var s Snapshot
		var start, end string
		if err := rows.Scan(&s.ID, &s.Path, &start, &end, &s.Bytes, &s.Files, &s.Errors, &s.Incomplete); err != nil {
			return nil, err
		}
		if s.Start, err = time.Parse(time.RFC3339Nano, start); err != nil {
			return nil, err
		}
		if s.End, err = time.Parse(time.RFC3339Nano, end); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
