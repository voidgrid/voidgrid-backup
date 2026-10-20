package server

import (
	"context"
	"errors"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
	"github.com/voidgrid/voidgrid-backup/internal/engine"
	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
	"github.com/voidgrid/voidgrid-backup/internal/safeint"
)

// ServerHost is the "restore on" value that means the server itself, for when
// the job's agent is broken or gone and no other agent will do. It can't clash
// with an agent ID, which is random hex.
const ServerHost = "server"

// serverAsAgent stands in for an agent in the browse and restore paths.
var serverAsAgent = catalog.Agent{ID: ServerHost, Name: "the server"}

// ServerCanOpen says why the server cannot browse or restore from a
// repository, or nil if it can try: it needs its engine and, for a
// filesystem repository, the directory mapped into its own container.
func (c *Controller) ServerCanOpen(repo catalog.Repository) error {
	if c.Engine == nil {
		return errors.New("this server has no repository engine")
	}
	_, err := serverRepo(repo)
	return err
}

// runOn runs an operation on the agent a, or in the server's own engine when a
// is the server. Like withAgent it is detached from the request's cancellation.
func runOn[T any](c *Controller, a catalog.Agent, repo catalog.Repository, timeout time.Duration,
	local func(context.Context, engine.Repo) (T, error),
	remote func(context.Context, agentpb.AgentClient) (T, error)) (T, error) {
	return runOnFrom(context.Background(), c, a, repo, timeout, local, remote)
}

// runOnFrom is runOn for a run that can be cancelled: it ends when parent does.
func runOnFrom[T any](parent context.Context, c *Controller, a catalog.Agent, repo catalog.Repository, timeout time.Duration,
	local func(context.Context, engine.Repo) (T, error),
	remote func(context.Context, agentpb.AgentClient) (T, error)) (T, error) {
	var zero T
	release, err := c.storageSlot(parent, repo)
	if err != nil {
		return zero, err
	}
	defer release()
	if a.ID != ServerHost {
		return withAgentFrom(parent, c, a, timeout, remote)
	}
	if c.Engine == nil {
		return zero, errors.New("this server has no repository engine")
	}
	er, err := serverRepo(repo)
	if err != nil {
		return zero, err
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	return local(ctx, er)
}

func restoreResponse(st engine.RestoreStats) *agentpb.RestoreResponse {
	return &agentpb.RestoreResponse{
		Bytes: st.Bytes, Files: safeint.Int32(st.Files), Dirs: safeint.Int32(st.Dirs), Skipped: safeint.Int32(st.Skipped),
		Warnings:         st.Warnings,
		OwnershipSkipped: st.OwnershipSkipped,
		AsUid:            safeint.Int32(st.AsUID),
		AsGid:            safeint.Int32(st.AsGID),
	}
}

func dirEntries(entries []engine.DirEntry) *agentpb.ListDirectoryResponse {
	resp := &agentpb.ListDirectoryResponse{}
	for _, e := range entries {
		resp.Entries = append(resp.Entries, &agentpb.DirEntry{
			Name: e.Name, Dir: e.Dir, Size: e.Size, ModUnix: e.ModUnix, Mode: e.Mode,
		})
	}
	return resp
}
