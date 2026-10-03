package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
)

// repoStatsTimeout is generous: listing every blob of a large S3 or SFTP
// repository takes a while.
const repoStatsTimeout = 15 * time.Minute

// RepoStats is the last measurement of the space a repository takes in its
// storage, kept in the catalog so pages don't re-list the storage.
type RepoStats struct {
	StoredBytes int64     `json:"stored_bytes"`
	Blobs       int64     `json:"blobs"`
	FSTotal     int64     `json:"fs_total,omitempty"` // filesystem repositories only
	FSAvail     int64     `json:"fs_avail,omitempty"`
	At          time.Time `json:"at"`
}

func repoStatsKey(repoID string) string { return "repo_stats:" + repoID }

// RepoStats returns the saved measurement for a repository, or ok=false if it
// has never been measured.
func (c *Controller) RepoStats(ctx context.Context, repoID string) (RepoStats, bool, error) {
	v, err := c.Catalog.GetSetting(ctx, repoStatsKey(repoID))
	if err != nil || v == "" {
		return RepoStats{}, false, err
	}
	var st RepoStats
	if err := json.Unmarshal([]byte(v), &st); err != nil {
		return RepoStats{}, false, nil // unreadable: treat as never measured
	}
	return st, true, nil
}

// RefreshRepoStats asks an agent to total the repository's blobs and saves the
// result. The agent is the one running a job on this repository, or failing
// that any enrolled agent.
func (c *Controller) RefreshRepoStats(ctx context.Context, repoID string) (RepoStats, error) {
	repo, err := c.Catalog.Repository(ctx, repoID)
	if err != nil {
		return RepoStats{}, fmt.Errorf("repository: %w", err)
	}
	if repo.InitializedAt.IsZero() {
		return RepoStats{}, ErrNotInitialized
	}
	agent, err := c.agentForRepo(ctx, repoID)
	if err != nil {
		return RepoStats{}, err
	}
	release, err := c.gate.acquire(ctx, repoID)
	if err != nil {
		return RepoStats{}, err
	}
	defer release()
	resp, err := withAgent(c, agent, repoStatsTimeout, func(ctx context.Context, cl agentpb.AgentClient) (*agentpb.RepoStatsResponse, error) {
		return cl.RepoStats(ctx, &agentpb.RepoStatsRequest{Repository: toProtoRepo(repo)})
	})
	if err != nil {
		return RepoStats{}, err
	}
	st := RepoStats{StoredBytes: resp.GetStoredBytes(), Blobs: resp.GetBlobs(),
		FSTotal: resp.GetFsTotal(), FSAvail: resp.GetFsAvail(), At: time.Now()}
	b, _ := json.Marshal(st)
	if err := c.Catalog.SetSetting(context.WithoutCancel(ctx), repoStatsKey(repoID), string(b)); err != nil {
		return st, err
	}
	return st, nil
}

func (c *Controller) agentForRepo(ctx context.Context, repoID string) (catalog.Agent, error) {
	jobs, err := c.Catalog.ListJobs(ctx)
	if err != nil {
		return catalog.Agent{}, err
	}
	for _, j := range jobs {
		if j.RepositoryID == repoID {
			return c.Catalog.Agent(ctx, j.AgentID)
		}
	}
	agents, err := c.Catalog.ListAgents(ctx)
	if err != nil {
		return catalog.Agent{}, err
	}
	if len(agents) == 0 {
		return catalog.Agent{}, errors.New("no agent is enrolled to measure the repository")
	}
	return agents[0], nil
}
