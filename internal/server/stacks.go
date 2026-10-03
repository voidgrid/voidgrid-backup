package server

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
	"github.com/voidgrid/voidgrid-backup/internal/guard"
	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
)

var projectName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

var dumpKinds = map[string]bool{"postgres": true, "mariadb": true, "redis": true}

func validStack(s catalog.StackConfig) (*catalog.StackConfig, error) {
	if !projectName.MatchString(s.Project) {
		return nil, fmt.Errorf("invalid compose project %q", s.Project)
	}
	var err error
	if s.WorkingDir, err = guard.SourcePath(s.WorkingDir); err != nil {
		return nil, err
	}
	clean := func(in []string) ([]string, error) {
		var out []string
		for _, p := range nonEmpty(in) {
			c, err := guard.SourcePath(p)
			if err != nil {
				return nil, err
			}
			out = append(out, c)
		}
		return out, nil
	}
	if s.Include, err = clean(s.Include); err != nil {
		return nil, err
	}
	if s.Exclude, err = clean(s.Exclude); err != nil {
		return nil, err
	}
	dumps := map[string]string{}
	for svc, kind := range s.Dumps {
		if kind == "" || kind == "none" {
			continue
		}
		if !dumpKinds[kind] {
			return nil, fmt.Errorf("service %s: unknown dump kind %q", svc, kind)
		}
		dumps[svc] = kind
	}
	s.Dumps = dumps
	switch s.Quiesce {
	case "", "none":
		s.Quiesce = "none"
	case "pause", "stop":
	default:
		return nil, fmt.Errorf("unknown quiesce mode %q", s.Quiesce)
	}
	return &s, nil
}

// Stacks lists the compose projects running on an agent's host.
func (c *Controller) Stacks(ctx context.Context, agentID string) ([]*agentpb.Stack, error) {
	agent, err := c.Catalog.Agent(ctx, agentID)
	if err != nil {
		return nil, fmt.Errorf("agent: %w", err)
	}
	resp, err := withAgent(c, agent, browseTimeout, func(ctx context.Context, cl agentpb.AgentClient) (*agentpb.ListStacksResponse, error) {
		return cl.ListStacks(ctx, &agentpb.ListStacksRequest{})
	})
	if err != nil {
		return nil, err
	}
	return resp.GetStacks(), nil
}

// RestoreStack restores a stack job's snapshot, in place (targetRoot "")
// or into an empty directory, and records it as a run.
func (c *Controller) RestoreStack(ctx context.Context, jobID, snapshotID, targetRoot string, stopStack bool) (catalog.Run, error) {
	if targetRoot != "" {
		if _, err := guard.RestoreTarget(targetRoot); err != nil {
			return catalog.Run{}, err
		}
	}
	return c.stackOp(ctx, jobID, "restore", func(repo catalog.Repository, agent catalog.Agent, job catalog.Job, run *catalog.Run) error {
		where := "original locations"
		if targetRoot != "" {
			where = targetRoot
		}
		run.Summary = fmt.Sprintf("restore stack %s from %s to %s", job.Stack.Project, shortID(snapshotID), where)
		resp, err := withAgent(c, agent, runTimeout, func(ctx context.Context, cl agentpb.AgentClient) (*agentpb.RestoreResponse, error) {
			return cl.RestoreStack(ctx, &agentpb.RestoreStackRequest{Repository: toProtoRepo(repo),
				SnapshotId: snapshotID, TargetRoot: targetRoot, StopStack: stopStack})
		})
		if err != nil {
			return err
		}
		run.Bytes, run.Files = resp.GetBytes(), int(resp.GetFiles())
		run.Error = strings.Join(resp.GetWarnings(), "\n")
		run.Summary += fmt.Sprintf(": %d files, %s", run.Files, HumanBytes(run.Bytes))
		return nil
	})
}

// ImportDump loads a service's database dump from a stack snapshot into the
// running container, recorded as a run.
func (c *Controller) ImportDump(ctx context.Context, jobID, snapshotID, service string) (catalog.Run, error) {
	return c.stackOp(ctx, jobID, "import", func(repo catalog.Repository, agent catalog.Agent, job catalog.Job, run *catalog.Run) error {
		run.Summary = fmt.Sprintf("import %s dump from %s", service, shortID(snapshotID))
		resp, err := withAgent(c, agent, runTimeout, func(ctx context.Context, cl agentpb.AgentClient) (*agentpb.ImportDumpResponse, error) {
			return cl.ImportDump(ctx, &agentpb.ImportDumpRequest{Repository: toProtoRepo(repo), SnapshotId: snapshotID, Service: service})
		})
		if err != nil {
			return err
		}
		if out := strings.TrimSpace(resp.GetOutput()); out != "" {
			run.Summary += "\n" + out
		}
		return nil
	})
}

func (c *Controller) stackOp(ctx context.Context, jobID, kind string, op func(catalog.Repository, catalog.Agent, catalog.Job, *catalog.Run) error) (catalog.Run, error) {
	return c.jobOp(ctx, jobID, kind, catalog.JobStack, op)
}

// jobOp runs a manual operation on a job of jobKind and records it as a run.
func (c *Controller) jobOp(ctx context.Context, jobID, kind, jobKind string, op func(catalog.Repository, catalog.Agent, catalog.Job, *catalog.Run) error) (catalog.Run, error) {
	if !c.inflight.start(jobID) {
		return catalog.Run{}, ErrRunning
	}
	defer c.inflight.done(jobID)
	job, repo, agent, err := c.jobContext(ctx, jobID)
	if err != nil {
		return catalog.Run{}, err
	}
	if job.Kind != jobKind {
		return catalog.Run{}, errors.New("not a " + jobKind + " job")
	}
	run := catalog.Run{JobID: jobID, Kind: kind, Trigger: "manual", StartedAt: time.Now()}
	if run.ID, err = c.Catalog.StartRun(ctx, jobID, kind, run.Trigger, run.StartedAt); err != nil {
		return catalog.Run{}, err
	}
	err = op(repo, agent, job, &run)
	run.FinishedAt = time.Now()
	switch {
	case err != nil:
		run.Status, run.Error = catalog.RunFailed, err.Error()
	case run.Error != "": // the op reported warnings
		run.Status = catalog.RunPartial
	default:
		run.Status = catalog.RunSuccess
	}
	if ferr := c.Catalog.FinishRun(context.WithoutCancel(ctx), run); ferr != nil {
		return run, ferr
	}
	return run, nil
}
