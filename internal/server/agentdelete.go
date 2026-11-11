package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
	"github.com/voidgrid/voidgrid-backup/internal/engine"
	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
)

// AgentInUseError says why an agent cannot be deleted yet: it still has jobs,
// or it is the Kopia maintenance owner of repositories that nothing else
// maintains.
type AgentInUseError struct {
	Agent     catalog.Agent
	Jobs      []string
	Maintains []catalog.Repository
}

func (e AgentInUseError) Error() string {
	var parts []string
	if len(e.Jobs) > 0 {
		parts = append(parts, fmt.Sprintf("it still has %d job(s) (%s); delete them first", len(e.Jobs), strings.Join(e.Jobs, ", ")))
	}
	if len(e.Maintains) > 0 {
		var names []string
		for _, r := range e.Maintains {
			names = append(names, r.Name)
		}
		parts = append(parts, "it maintains "+strings.Join(names, ", ")+"; hand maintenance over first")
	}
	return e.Agent.Name + " cannot be deleted: " + strings.Join(parts, ", and ")
}

// DeleteAgent removes an agent. It refuses (AgentInUseError) while the agent
// has jobs, or is the maintenance owner of a repository the server cannot
// maintain otherwise: deleting it would leave the repository's maintenance
// stalled on an agent that no longer exists. Its snapshots stay in their
// repositories, and warnings lists repositories whose owner could not be
// checked because nothing else could open them.
func (c *Controller) DeleteAgent(ctx context.Context, id string) (warnings []string, err error) {
	agent, err := c.Catalog.Agent(ctx, id)
	if err != nil {
		return nil, err
	}
	jobs, err := c.Catalog.ListJobs(ctx)
	if err != nil {
		return nil, err
	}
	block := AgentInUseError{Agent: agent}
	for _, j := range jobs {
		if j.AgentID == id {
			block.Jobs = append(block.Jobs, j.Name)
		}
	}
	repos, err := c.Catalog.ListRepositories(ctx)
	if err != nil {
		return nil, err
	}
	for _, repo := range repos {
		assigned, err := c.Catalog.RepositoryAgents(ctx, repo.ID)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(assigned, id) || repo.InitializedAt.IsZero() || c.MaintainedBy(ctx, repo.ID) != MaintByAgent {
			continue
		}
		owner, ok := c.maintenanceOwner(ctx, repo, assigned, id)
		switch {
		case !ok:
			warnings = append(warnings, "could not check who maintains "+repo.Name+": no other agent or the server could open it")
		case owner == engine.OwnerOf(id):
			block.Maintains = append(block.Maintains, repo)
		}
	}
	if len(block.Jobs) > 0 || len(block.Maintains) > 0 {
		return nil, block
	}
	if err := c.Catalog.DeleteAgent(ctx, id); err != nil {
		return nil, err
	}
	if err := c.Catalog.SetSetting(ctx, agentNextCertKey(id), ""); err != nil {
		warnings = append(warnings, "could not clear the agent's pending certificate renewal: "+err.Error())
	}
	return warnings, nil
}

// maintenanceOwner reads the repository's recorded Kopia maintenance owner
// through an agent other than skip that is assigned to it (reachable ones
// first), else through the server if it can open the repository. ok is false
// when nothing could.
func (c *Controller) maintenanceOwner(ctx context.Context, repo catalog.Repository, assigned []string, skip string) (owner string, ok bool) {
	var agents []catalog.Agent
	for _, id := range assigned {
		if id == skip {
			continue
		}
		if a, err := c.Catalog.Agent(ctx, id); err == nil {
			agents = append(agents, a)
		}
	}
	slices.SortStableFunc(agents, func(a, b catalog.Agent) int {
		return boolRank(a.LastError != "") - boolRank(b.LastError != "")
	})
	for _, a := range agents {
		resp, err := withAgent(c, a, initTimeout, func(ctx context.Context, cl agentpb.AgentClient) (*agentpb.TestRepositoryResponse, error) {
			return cl.TestRepository(ctx, &agentpb.TestRepositoryRequest{Repository: toProtoRepo(repo, a.ID)})
		})
		if err == nil && resp.GetRepositoryId() != "" {
			return resp.GetOwner(), true
		}
	}
	if c.ServerCanOpen(repo) == nil {
		if er, err := serverRepo(repo); err == nil {
			if owner, err := c.Engine.MaintenanceOwner(ctx, er); err == nil {
				return owner, true
			}
		}
	}
	return "", false
}

func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

// isAgentInUse unwraps an AgentInUseError.
func isAgentInUse(err error) (AgentInUseError, bool) {
	var e AgentInUseError
	return e, errors.As(err, &e)
}
