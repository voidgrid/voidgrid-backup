package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
)

// wipeTimeout bounds one repository wipe: a big S3 bucket is deleted one
// object at a time.
const wipeTimeout = 12 * time.Hour

func repoWipeKey(repoID string) string    { return "wipe:" + repoID }
func repoWipeErrKey(repoID string) string { return "repo_wipe_error:" + repoID }

// RepoInUseError is returned when a repository still has jobs.
type RepoInUseError struct{ Jobs []string }

func (e RepoInUseError) Error() string {
	return fmt.Sprintf("%d job(s) still use this repository (%s); delete them first", len(e.Jobs), strings.Join(e.Jobs, ", "))
}

// repoIdle refuses while any job uses the repository or something is already
// working on it.
func (c *Controller) repoIdle(ctx context.Context, repoID string) error {
	jobs, err := c.Catalog.ListJobs(ctx)
	if err != nil {
		return err
	}
	var using []string
	for _, j := range jobs {
		if j.RepositoryID == repoID {
			using = append(using, j.Name)
		}
	}
	if len(using) > 0 {
		return RepoInUseError{Jobs: using}
	}
	if c.IsRunning(repoMeasureKey(repoID)) || c.IsRunning(repoWipeKey(repoID)) {
		return ErrRunning
	}
	return nil
}

// forgetRepository deletes the repository's record and what the server saved
// about it. The data in storage is untouched.
func (c *Controller) forgetRepository(ctx context.Context, repoID string) error {
	if err := c.Catalog.DeleteRepository(ctx, repoID); err != nil {
		return err
	}
	for _, key := range []string{repoStatsKey(repoID), repoWipeErrKey(repoID)} {
		if err := c.Catalog.SetSetting(ctx, key, ""); err != nil {
			slog.Warn("clear repository setting", "key", key, "err", err)
		}
	}
	return nil
}

// RemoveRepository removes a repository from the server. Its data stays in
// storage, so it can be added again with its password.
func (c *Controller) RemoveRepository(ctx context.Context, repoID string) error {
	repo, err := c.Catalog.Repository(ctx, repoID)
	if err != nil {
		return err
	}
	if err := c.repoIdle(ctx, repoID); err != nil {
		return err
	}
	if err := c.forgetRepository(ctx, repoID); err != nil {
		return err
	}
	slog.Warn("repository removed from the server; its data was not touched", "repo", repo.ID, "name", repo.Name)
	return nil
}

// StartWipe deletes every blob of the repository's storage through agentID,
// then removes the repository record, in the background. It refuses at once
// when the repository still has jobs or is busy. A failed wipe keeps the
// record and its error (see WipeError) so it can be run again.
func (c *Controller) StartWipe(ctx context.Context, repoID, agentID string) error {
	repo, err := c.Catalog.Repository(ctx, repoID)
	if err != nil {
		return err
	}
	agent, err := c.Catalog.Agent(ctx, agentID)
	if err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	if err := c.repoIdle(ctx, repoID); err != nil {
		return err
	}
	if !c.inflight.start(repoWipeKey(repoID)) {
		return ErrRunning
	}
	if err := c.Catalog.SetSetting(ctx, repoWipeErrKey(repoID), ""); err != nil {
		slog.Warn("clear wipe error", "repo", repoID, "err", err)
	}
	go func() {
		defer c.inflight.done(repoWipeKey(repoID))
		bg := context.Background()
		slog.Warn("repository wipe requested", "repo", repo.ID, "name", repo.Name, "agent", agent.Name)
		resp, err := withAgent(c, agent, wipeTimeout, func(ctx context.Context, cl agentpb.AgentClient) (*agentpb.WipeRepositoryResponse, error) {
			return cl.WipeRepository(ctx, &agentpb.WipeRepositoryRequest{Repository: toProtoRepo(repo)})
		})
		if err != nil {
			slog.Error("repository wipe failed", "repo", repo.ID, "agent", agent.Name, "err", err)
			if serr := c.Catalog.SetSetting(bg, repoWipeErrKey(repoID), "wipe via "+agent.Name+" failed: "+err.Error()); serr != nil {
				slog.Warn("save wipe error", "repo", repoID, "err", serr)
			}
			return
		}
		if err := c.forgetRepository(bg, repoID); err != nil {
			slog.Error("repository wiped but its record could not be removed", "repo", repo.ID, "err", err)
			return
		}
		slog.Warn("repository wiped and removed", "repo", repo.ID, "name", repo.Name, "blobs", resp.GetBlobs(), "bytes", resp.GetBytes())
	}()
	return nil
}

// WipeError is the last failure of a wipe of this repository, "" if none.
func (c *Controller) WipeError(ctx context.Context, repoID string) string {
	v, _ := c.Catalog.GetSetting(ctx, repoWipeErrKey(repoID))
	return v
}

// DeleteSnapshot forgets one snapshot of a job. Its data is released only by
// later full maintenance. The snapshot must be in the job's recorded list.
func (c *Controller) DeleteSnapshot(ctx context.Context, jobID, snapshotID string) error {
	if !c.inflight.start(jobID) {
		return ErrRunning
	}
	defer c.inflight.done(jobID)
	job, repo, agent, err := c.jobContext(ctx, jobID)
	if err != nil {
		return err
	}
	snaps, err := c.Catalog.ListSnapshots(ctx, jobID)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(snaps, func(s catalog.Snapshot) bool { return s.ID == snapshotID }) {
		return errors.New("that snapshot is not in the job's list; refresh the list from the repository first")
	}
	if _, err := withAgent(c, agent, browseTimeout, func(ctx context.Context, cl agentpb.AgentClient) (*agentpb.DeleteSnapshotResponse, error) {
		return cl.DeleteSnapshot(ctx, &agentpb.DeleteSnapshotRequest{Repository: toProtoRepo(repo), SnapshotId: snapshotID})
	}); err != nil {
		return fmt.Errorf("delete on %s: %w", agent.Name, err)
	}
	slog.Warn("snapshot deleted", "job", job.Name, "repo", repo.ID, "snapshot", snapshotID)
	return c.Catalog.UpsertSnapshots(context.WithoutCancel(ctx), jobID, nil, []string{snapshotID})
}
