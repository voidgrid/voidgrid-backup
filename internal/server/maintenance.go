package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
)

const (
	// maintInterval is the time between full maintenance cycles of a repository
	// that has jobs. maintJitter is added after every run, and maintFirstSpread
	// spreads the first runs after the server starts, so repositories never all
	// begin together.
	maintInterval    = 24 * time.Hour
	maintJitter      = time.Hour
	maintFirstSpread = 2 * time.Hour
	// maintRetry is the wait before trying again after a failed run.
	maintRetry   = time.Hour
	maintTimeout = 6 * time.Hour
)

// maintRand returns a random duration in [0, max). Tests replace it.
var maintRand = func(maxd time.Duration) time.Duration {
	return rand.N(maxd) //nolint:gosec // G404: scheduling jitter, not security
}

func repoMaintKey(repoID string) string    { return "repo_maint:" + repoID }
func repoMaintRunKey(repoID string) string { return "maint:" + repoID }

// RepoMaintenance is what the server knows about a repository's maintenance.
type RepoMaintenance struct {
	LastRun time.Time `json:"last_run"` // zero until it has run
	Status  string    `json:"status"`   // "ok" or "failed"
	Error   string    `json:"error,omitempty"`
	AgentID string    `json:"agent_id,omitempty"` // the agent that ran, or last tried
	NextDue time.Time `json:"next_due"`
}

// RepoMaintenance returns the saved state, or ok=false if the repository has
// never been seen by the maintenance schedule.
func (c *Controller) RepoMaintenance(ctx context.Context, repoID string) (RepoMaintenance, bool) {
	v, err := c.Catalog.GetSetting(ctx, repoMaintKey(repoID))
	if err != nil || v == "" {
		return RepoMaintenance{}, false
	}
	var m RepoMaintenance
	if err := json.Unmarshal([]byte(v), &m); err != nil {
		return RepoMaintenance{}, false
	}
	return m, true
}

func (c *Controller) saveMaintenance(ctx context.Context, repoID string, m RepoMaintenance) {
	b, _ := json.Marshal(m) // plain fields always marshal
	if err := c.Catalog.SetSetting(ctx, repoMaintKey(repoID), string(b)); err != nil {
		slog.Error("save maintenance state", "repo", repoID, "err", err)
	}
}

// MaintainRepository runs a full maintenance cycle now, on the repository's
// maintenance owner, and records the result and when the next one is due.
// Kopia lets only the owner do it: the first agent asked (one with a job on
// the repository) answers with the owner when it is not, and the owner, whose
// agent ID is the host part of that answer, is asked next.
func (c *Controller) MaintainRepository(ctx context.Context, repoID string) (RepoMaintenance, error) {
	repo, err := c.Catalog.Repository(ctx, repoID)
	if err != nil {
		return RepoMaintenance{}, err
	}
	if repo.InitializedAt.IsZero() {
		return RepoMaintenance{}, ErrNotInitialized
	}
	if c.IsRunning(repoWipeKey(repoID)) || !c.inflight.start(repoMaintRunKey(repoID)) {
		return RepoMaintenance{}, ErrRunning
	}
	defer c.inflight.done(repoMaintRunKey(repoID))

	now := time.Now()
	m := RepoMaintenance{LastRun: now, Status: "ok"}
	agentID, err := c.runMaintenance(ctx, repo)
	m.AgentID = agentID
	if err != nil {
		m.Status, m.Error = "failed", err.Error()
		m.NextDue = now.Add(maintRetry + maintRand(maintJitter))
		slog.Error("maintenance failed", "repo", repo.ID, "name", repo.Name, "err", err)
	} else {
		m.NextDue = now.Add(maintInterval + maintRand(maintJitter))
		slog.Info("maintenance finished", "repo", repo.ID, "name", repo.Name, "agent", agentID, "next", m.NextDue.Format(time.RFC3339))
	}
	c.saveMaintenance(context.WithoutCancel(ctx), repoID, m)
	return m, err
}

func (c *Controller) runMaintenance(ctx context.Context, repo catalog.Repository) (string, error) {
	agent, err := c.agentForRepo(ctx, repo.ID)
	if err != nil {
		return "", err
	}
	tried := map[string]bool{}
	for range 3 {
		tried[agent.ID] = true
		resp, err := withAgent(c, agent, maintTimeout, func(ctx context.Context, cl agentpb.AgentClient) (*agentpb.MaintainResponse, error) {
			return cl.Maintain(ctx, &agentpb.MaintainRequest{Repository: toProtoRepo(repo)})
		})
		if err != nil {
			return agent.ID, fmt.Errorf("on %s: %w", agent.Name, err)
		}
		if resp.GetRan() {
			return agent.ID, nil
		}
		owner := resp.GetOwner()
		next, err := c.Catalog.Agent(ctx, owner[strings.LastIndex(owner, "@")+1:])
		if err != nil || tried[next.ID] {
			return agent.ID, fmt.Errorf("the maintenance owner is %q, which is not a registered agent (a replaced or removed host?), so no agent can run maintenance", owner)
		}
		agent = next
	}
	return agent.ID, errors.New("could not find the maintenance owner")
}

// startMaintenance starts the full cycle of every repository that has jobs
// and is due. A repository seen for the first time gets a random start time
// within maintFirstSpread. A repository with a running job waits for the next
// tick; repositories without jobs are never scheduled (the button still works).
func (c *Controller) startMaintenance(ctx context.Context, now time.Time) {
	jobs, err := c.Catalog.ListJobs(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("scheduler: list jobs", "err", err)
		}
		return
	}
	busy := map[string]bool{} // repository ID -> has a job, true if one is running
	for _, j := range jobs {
		busy[j.RepositoryID] = busy[j.RepositoryID] || c.IsRunning(j.ID)
	}
	repos, err := c.Catalog.ListRepositories(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("scheduler: list repositories", "err", err)
		}
		return
	}
	for _, r := range repos {
		running, hasJobs := busy[r.ID]
		if !hasJobs || r.InitializedAt.IsZero() {
			continue
		}
		m, ok := c.RepoMaintenance(ctx, r.ID)
		if !ok {
			c.saveMaintenance(ctx, r.ID, RepoMaintenance{NextDue: now.Add(maintRand(maintFirstSpread))})
			continue
		}
		if now.Before(m.NextDue) || running || c.IsRunning(repoMaintRunKey(r.ID)) {
			continue
		}
		go func(id string) {
			if _, err := c.MaintainRepository(context.WithoutCancel(ctx), id); err != nil && !errors.Is(err, ErrRunning) {
				slog.Error("scheduled maintenance", "repo", id, "err", err)
			}
		}(r.ID)
	}
}
