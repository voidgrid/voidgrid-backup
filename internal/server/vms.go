package server

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
	"github.com/voidgrid/voidgrid-backup/internal/engine"
	"github.com/voidgrid/voidgrid-backup/internal/guard"
	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
)

var vmName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.+-]{0,127}$`)
var diskTarget = regexp.MustCompile(`^[a-z]{2,4}[a-z0-9]{0,4}$`)

func validVM(v catalog.VMConfig) (*catalog.VMConfig, error) {
	if !vmName.MatchString(v.Name) {
		return nil, fmt.Errorf("invalid VM name %q", v.Name)
	}
	for _, d := range v.Disks {
		if !diskTarget.MatchString(d) {
			return nil, fmt.Errorf("invalid disk target %q", d)
		}
	}
	return &v, nil
}

// VMs lists the libvirt domains on an agent's host.
func (c *Controller) VMs(ctx context.Context, agentID string) ([]*agentpb.VM, error) {
	agent, err := c.Catalog.Agent(ctx, agentID)
	if err != nil {
		return nil, fmt.Errorf("agent: %w", err)
	}
	resp, err := withAgent(c, agent, browseTimeout, func(ctx context.Context, cl agentpb.AgentClient) (*agentpb.ListVMsResponse, error) {
		return cl.ListVMs(ctx, &agentpb.ListVMsRequest{})
	})
	if err != nil {
		return nil, err
	}
	return resp.GetVms(), nil
}

// RestoreVM restores a VM job's snapshot, in place (targetDir "", VM shut
// off) or into an empty directory, and records it as a run. onID runs it on
// another agent, which needs a targetDir.
func (c *Controller) RestoreVM(ctx context.Context, jobID, onID, snapshotID, targetDir string, define bool) (catalog.Run, error) {
	if targetDir != "" {
		if _, err := guard.RestoreTarget(targetDir); err != nil {
			return catalog.Run{}, err
		}
	}
	return c.jobOp(ctx, jobID, onID, targetDir == "", "restore", catalog.JobVM, func(rctx context.Context, repo catalog.Repository, agent catalog.Agent, job catalog.Job, run *catalog.Run) error {
		where := "its original disk paths"
		if targetDir != "" {
			where = targetDir
		}
		run.Summary = fmt.Sprintf("restore VM %s from %s to %s%s", job.VM.Name, shortID(snapshotID), where, onHost(job, agent))
		resp, err := runOnFrom(rctx, c, agent, repo, runTimeout, func(ctx context.Context, er engine.Repo) (*agentpb.RestoreResponse, error) {
			// Into a directory the hypervisor is not used; in place never runs here.
			st, err := c.Engine.RestoreVM(ctx, er, nil, snapshotID, targetDir, define)
			return restoreResponse(st), err
		}, func(ctx context.Context, cl agentpb.AgentClient) (*agentpb.RestoreResponse, error) {
			return cl.RestoreVM(ctx, &agentpb.RestoreVMRequest{Repository: toProtoRepo(repo, agent.ID),
				SnapshotId: snapshotID, TargetDir: targetDir, Define: define})
		})
		if err != nil {
			return err
		}
		run.Bytes, run.Files = resp.GetBytes(), int(resp.GetFiles())
		run.Error = strings.Join(resp.GetWarnings(), "\n")
		run.Summary = withOwnershipNote(run.Summary+fmt.Sprintf(": %d files, %s", run.Files, HumanBytes(run.Bytes)), resp, agent.Name)
		return nil
	})
}
