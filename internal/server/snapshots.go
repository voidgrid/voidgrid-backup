package server

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
)

// Snapshot is a snapshot as the UI shows it.
type Snapshot = catalog.Snapshot

// snapLock serializes, per job, recording what a backup produced and
// reconciling the list from the repository. Without it a reconcile that listed
// the repository before a backup finished could replace the record after the
// backup had added its snapshot, and drop it.
func (c *Controller) snapLock(jobID string) *sync.Mutex {
	m, _ := c.snapLocks.LoadOrStore(jobID, new(sync.Mutex))
	return m.(*sync.Mutex)
}

func snapshotsSyncedKey(jobID string) string { return "snapshots_synced:" + jobID }

// Snapshots returns the job's snapshots from the server's own record. It
// never contacts the repository, so it is fast and works when the agent or
// the storage is down; RefreshSnapshots brings the record in line with the
// repository.
func (c *Controller) Snapshots(ctx context.Context, jobID string) ([]Snapshot, error) {
	return c.Catalog.ListSnapshots(ctx, jobID)
}

// SnapshotsSynced is when the job's snapshot record was last made to match
// the repository; ok is false if that has never happened.
func (c *Controller) SnapshotsSynced(ctx context.Context, jobID string) (time.Time, bool) {
	v, err := c.Catalog.GetSetting(ctx, snapshotsSyncedKey(jobID))
	if err != nil || v == "" {
		return time.Time{}, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(n, 0), true
}

// RefreshSnapshots lists the job's snapshots from the repository and makes the
// server's record match, which also picks up changes made outside this
// server. It is limited per repository, like other interactive repository calls.
func (c *Controller) RefreshSnapshots(ctx context.Context, jobID string) error {
	job, err := c.Catalog.Job(ctx, jobID)
	if err != nil {
		return err
	}
	release, err := c.gate.acquire(ctx, job.RepositoryID)
	if err != nil {
		return err
	}
	defer release()
	mu := c.snapLock(jobID)
	mu.Lock()
	defer mu.Unlock()
	snaps, err := c.listRemoteSnapshots(ctx, jobID)
	if err != nil {
		return err
	}
	if err := c.Catalog.ReplaceSnapshots(ctx, jobID, snaps); err != nil {
		return err
	}
	return c.Catalog.SetSetting(ctx, snapshotsSyncedKey(jobID), strconv.FormatInt(time.Now().Unix(), 10))
}

// recordSnapshots adds what a backup just produced to the job's snapshot
// record and removes what its retention deleted. If the record has never been
// made to match the repository (jobs that existed before the record did), or
// the agent is too old to report what's needed, it reconciles in the background.
func (c *Controller) recordSnapshots(ctx context.Context, job catalog.Job, results []*agentpb.PathResult) {
	var add []Snapshot
	var drop []string
	reconcile := false
	for _, r := range results {
		drop = append(drop, r.GetPrunedIds()...)
		if r.GetSnapshotId() == "" {
			continue
		}
		if r.GetStartUnix() == 0 {
			reconcile = true // an agent that predates the fields
			continue
		}
		add = append(add, Snapshot{
			ID: r.GetSnapshotId(), Path: r.GetPath(),
			Start: time.Unix(r.GetStartUnix(), 0), End: time.Unix(r.GetEndUnix(), 0),
			Bytes: r.GetBytes(), Files: int(r.GetFiles()), Errors: int(r.GetErrors()), Incomplete: r.GetIncomplete(),
		})
	}
	mu := c.snapLock(job.ID)
	mu.Lock()
	if err := c.Catalog.UpsertSnapshots(ctx, job.ID, add, drop); err != nil {
		slog.Error("record snapshots", "job", job.Name, "err", err)
		reconcile = true
	}
	mu.Unlock()
	if _, ok := c.SnapshotsSynced(ctx, job.ID); !ok {
		reconcile = true
	}
	if reconcile {
		go func() {
			if err := c.RefreshSnapshots(context.Background(), job.ID); err != nil {
				slog.Error("reconcile snapshots", "job", job.Name, "err", err)
			}
		}()
	}
}
