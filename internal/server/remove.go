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
	"github.com/voidgrid/voidgrid-backup/internal/engine"
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
	if c.IsRunning(repoMeasureKey(repoID)) || c.IsRunning(repoWipeKey(repoID)) || c.IsRunning(repoMaintRunKey(repoID)) {
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
	if err := c.Catalog.DeleteSettings(ctx, repoStatsKey(repoID), repoWipeErrKey(repoID), repoMaintKey(repoID),
		repoServerCreatedKey(repoID), serverReachKey(repoID), repoMaintByKey(repoID), repoMaintSchedKey(repoID)); err != nil {
		slog.Warn("clear the repository's settings", "repo", repoID, "err", err)
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
	agent := serverAsAgent
	if agentID != ServerHost {
		if agent, err = c.Catalog.Agent(ctx, agentID); err != nil {
			return fmt.Errorf("agent: %w", err)
		}
	} else if err := c.ServerCanOpen(repo); err != nil {
		return fmt.Errorf("the server cannot open this repository: %w", err)
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
		bg := context.WithoutCancel(ctx) // the wipe outlives the request that asked for it
		slog.Warn("repository wipe requested", "repo", repo.ID, "name", repo.Name, "agent", agent.Name)
		resp, err := runOn(c, agent, repo, wipeTimeout, func(ctx context.Context, er engine.Repo) (*agentpb.WipeRepositoryResponse, error) {
			blobs, bytes, err := c.Engine.Wipe(ctx, er)
			return &agentpb.WipeRepositoryResponse{Blobs: blobs, Bytes: bytes}, err
		}, func(ctx context.Context, cl agentpb.AgentClient) (*agentpb.WipeRepositoryResponse, error) {
			return cl.WipeRepository(ctx, &agentpb.WipeRepositoryRequest{Repository: toProtoRepo(repo, agent.ID)})
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

// DeleteJob removes a job and its run history. With snapshots, it first
// forgets every snapshot in the job's recorded list, in one request to the
// agent, and keeps the job if that fails. It returns how many snapshots were
// forgotten (a snapshot the repository no longer has counts as forgotten).
// Their space is released by later full maintenance, and a snapshot the list
// hasn't picked up yet (the list is refreshed after every backup) is left in
// the repository.
func (c *Controller) DeleteJob(ctx context.Context, jobID string, snapshots bool) (int, error) {
	return c.DeleteJobOn(ctx, jobID, snapshots, "")
}

// DeleteJobOn is DeleteJob with the snapshots forgotten through on: "" for the
// job's agent, or ServerHost for the server's own engine, for when that agent
// cannot reach the repository and the server can. It never falls back by
// itself; on only matters when the snapshots are being deleted, since deleting
// the job alone needs no agent.
func (c *Controller) DeleteJobOn(ctx context.Context, jobID string, snapshots bool, on string) (int, error) {
	if !c.inflight.start(jobID) {
		return 0, ErrRunning
	}
	defer c.inflight.done(jobID)
	n := 0
	if snapshots {
		job, repo, agent, err := c.jobContext(ctx, jobID)
		if err != nil {
			return 0, err
		}
		switch on {
		case "":
		case ServerHost:
			if err := c.ServerCanOpen(repo); err != nil {
				return 0, fmt.Errorf("the server cannot open this repository: %w", err)
			}
			agent = serverAsAgent
		default:
			return 0, fmt.Errorf("unknown place to delete from: %q", on)
		}
		snaps, err := c.Catalog.ListSnapshots(ctx, jobID)
		if err != nil {
			return 0, err
		}
		ids := make([]string, 0, len(snaps))
		for _, s := range snaps {
			ids = append(ids, s.ID)
		}
		if len(ids) > 0 {
			if _, err := runOn(c, agent, repo, browseTimeout, func(ctx context.Context, er engine.Repo) (*agentpb.DeleteSnapshotResponse, error) {
				// The snapshots are the job's agent's: the server acts for that host only.
				return &agentpb.DeleteSnapshotResponse{}, c.Engine.DeleteSnapshotsOf(ctx, er, job.AgentID, ids)
			}, func(ctx context.Context, cl agentpb.AgentClient) (*agentpb.DeleteSnapshotResponse, error) {
				return cl.DeleteSnapshot(ctx, &agentpb.DeleteSnapshotRequest{Repository: toProtoRepo(repo, agent.ID), SnapshotIds: ids})
			}); err != nil {
				return 0, fmt.Errorf("delete snapshots on %s: %w", agent.Name, err)
			}
			n = len(ids)
			slog.Warn("job snapshots deleted", "job", job.Name, "repo", repo.ID, "snapshots", n, "on", agent.Name)
		}
	}
	if err := c.Catalog.DeleteJob(ctx, jobID); err != nil {
		return n, err
	}
	if err := c.Catalog.DeleteSettings(ctx, scheduleChangedKey(jobID), snapshotsSyncedKey(jobID), snapshotsViaKey(jobID)); err != nil {
		slog.Warn("clear the job's settings", "job", jobID, "err", err)
	}
	c.snapLocks.Delete(jobID)
	return n, nil
}

// DeleteSnapshot forgets one snapshot of a job through the job's agent. Its
// data is released only by later full maintenance. The snapshot must be in the
// job's recorded list.
func (c *Controller) DeleteSnapshot(ctx context.Context, jobID, snapshotID string) error {
	return c.DeleteSnapshotOn(ctx, jobID, snapshotID, "")
}

// DeleteSnapshotOn is DeleteSnapshot through on: "" for the job's agent, or
// ServerHost for the server's own engine, for when that agent cannot reach the
// repository and the server can. It never falls back by itself: a delete runs
// where it was asked to.
func (c *Controller) DeleteSnapshotOn(ctx context.Context, jobID, snapshotID, on string) error {
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
	switch on {
	case "":
	case ServerHost:
		if err := c.ServerCanOpen(repo); err != nil {
			return fmt.Errorf("the server cannot open this repository: %w", err)
		}
		agent = serverAsAgent
	default:
		return fmt.Errorf("unknown place to delete from: %q", on)
	}
	if _, err := runOn(c, agent, repo, browseTimeout, func(ctx context.Context, er engine.Repo) (*agentpb.DeleteSnapshotResponse, error) {
		// The snapshot is the job's agent's: the server acts for that host only.
		return &agentpb.DeleteSnapshotResponse{}, c.Engine.DeleteSnapshotOf(ctx, er, job.AgentID, snapshotID)
	}, func(ctx context.Context, cl agentpb.AgentClient) (*agentpb.DeleteSnapshotResponse, error) {
		return cl.DeleteSnapshot(ctx, &agentpb.DeleteSnapshotRequest{Repository: toProtoRepo(repo, agent.ID), SnapshotId: snapshotID})
	}); err != nil {
		return fmt.Errorf("delete on %s: %w", agent.Name, err)
	}
	slog.Warn("snapshot deleted", "job", job.Name, "repo", repo.ID, "snapshot", snapshotID, "on", agent.Name)
	return c.Catalog.UpsertSnapshots(context.WithoutCancel(ctx), jobID, nil, []string{snapshotID})
}
