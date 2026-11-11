package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	// Via is who measured it when that was not an agent ("the server"): an
	// agent could not open the repository and the server could.
	Via string `json:"via,omitempty"`
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
	release, err := c.gate.acquire(ctx, repoID)
	if err != nil {
		return RepoStats{}, err
	}
	defer release()
	st, err := c.measureOnAgent(ctx, repo)
	if err != nil {
		// Reads fall back to the server when it can open the repository itself.
		if c.ServerCanOpen(repo) != nil {
			return RepoStats{}, err
		}
		var serr error
		if st, serr = c.measureOnServer(ctx, repo); serr != nil {
			return RepoStats{}, fmt.Errorf("%w (and the server could not measure it either: %v)", err, serr)
		}
		slog.Warn("repository measured through the server", "repo", repo.ID, "agent_error", err)
	}
	b, _ := json.Marshal(st)
	if err := c.Catalog.SetSetting(context.WithoutCancel(ctx), repoStatsKey(repoID), string(b)); err != nil {
		return st, err
	}
	return st, nil
}

// measureOnAgent totals the repository's blobs through the agent that runs a
// job on it, or failing that any enrolled agent.
func (c *Controller) measureOnAgent(ctx context.Context, repo catalog.Repository) (RepoStats, error) {
	agent, err := c.agentForRepo(ctx, repo.ID)
	if err != nil {
		return RepoStats{}, err
	}
	resp, err := withStorageSlot(ctx, c, repo, func() (*agentpb.RepoStatsResponse, error) {
		return withAgent(c, agent, repoStatsTimeout, func(ctx context.Context, cl agentpb.AgentClient) (*agentpb.RepoStatsResponse, error) {
			return cl.RepoStats(ctx, &agentpb.RepoStatsRequest{Repository: toProtoRepo(repo, agent.ID)})
		})
	})
	if err != nil {
		return RepoStats{}, err
	}
	return RepoStats{StoredBytes: resp.GetStoredBytes(), Blobs: resp.GetBlobs(),
		FSTotal: resp.GetFsTotal(), FSAvail: resp.GetFsAvail(), At: time.Now()}, nil
}

// measureOnServer totals the repository's blobs through the server's own engine.
func (c *Controller) measureOnServer(ctx context.Context, repo catalog.Repository) (RepoStats, error) {
	er, err := serverRepo(repo)
	if err != nil {
		return RepoStats{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, repoStatsTimeout)
	defer cancel()
	res, err := c.Engine.RepoStats(ctx, er)
	if err != nil {
		return RepoStats{}, err
	}
	return RepoStats{StoredBytes: res.StoredBytes, Blobs: res.Blobs, FSTotal: res.FSTotal, FSAvail: res.FSAvail,
		At: time.Now(), Via: "the server"}, nil
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
	// No job yet: one of the agents assigned to it, a reachable one first. An
	// agent that is not assigned is never sent the repository's password.
	assigned, err := c.Catalog.RepositoryAgents(ctx, repoID)
	if err != nil {
		return catalog.Agent{}, err
	}
	var down *catalog.Agent
	for _, id := range assigned {
		a, err := c.Catalog.Agent(ctx, id)
		if err != nil {
			continue
		}
		if a.LastError == "" {
			return a, nil
		}
		if down == nil {
			down = &a
		}
	}
	if down != nil {
		return *down, nil
	}
	if len(assigned) > 0 || c.serverCreated(ctx, repoID) {
		return catalog.Agent{}, errors.New("no agent is assigned to this repository")
	}
	// A repository from before assignments existed has none and is open to
	// every agent (see ReposFor).
	agents, err := c.Catalog.ListAgents(ctx)
	if err != nil {
		return catalog.Agent{}, err
	}
	if len(agents) == 0 {
		return catalog.Agent{}, errors.New("no agent is enrolled to measure the repository")
	}
	return agents[0], nil
}
