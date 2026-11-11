package agent

import (
	"context"
	"encoding/json"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/voidgrid/voidgrid-backup/internal/engine"
	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
	"github.com/voidgrid/voidgrid-backup/internal/repocfg"
)

func (a *Agent) DeleteSnapshot(ctx context.Context, req *agentpb.DeleteSnapshotRequest) (*agentpb.DeleteSnapshotResponse, error) {
	r, err := toRepo(req.GetRepository())
	if err != nil {
		return nil, err
	}
	unlock, lerr := a.opMu.lock(ctx)
	if lerr != nil {
		return nil, lerr
	}
	defer unlock()
	ids := req.GetSnapshotIds()
	if id := req.GetSnapshotId(); id != "" {
		ids = append([]string{id}, ids...)
	}
	logStarted("snapshot delete started", r.ID, "snapshots", len(ids))
	if err := a.engine.DeleteSnapshots(ctx, r, ids); err != nil {
		return nil, err
	}
	return &agentpb.DeleteSnapshotResponse{}, nil
}

func (a *Agent) WipeRepository(ctx context.Context, req *agentpb.WipeRepositoryRequest) (*agentpb.WipeRepositoryResponse, error) {
	r, err := toRepo(req.GetRepository())
	if err != nil {
		return nil, err
	}
	unlock, lerr := a.opMu.lock(ctx)
	if lerr != nil {
		return nil, lerr
	}
	defer unlock()
	logStarted("repository wipe started", r.ID, "location", r.Config.Location())
	blobs, bytes, err := a.engine.Wipe(ctx, r)
	if err != nil {
		return nil, err
	}
	return &agentpb.WipeRepositoryResponse{Blobs: blobs, Bytes: bytes}, nil
}

func (a *Agent) Maintain(ctx context.Context, req *agentpb.MaintainRequest) (*agentpb.MaintainResponse, error) {
	r, err := toRepo(req.GetRepository())
	if err != nil {
		return nil, err
	}
	waiting := time.Now()
	unlock, lerr := a.opMu.lock(ctx)
	if lerr != nil {
		return nil, lerr
	}
	defer unlock()
	queued := time.Since(waiting)
	logStarted("maintenance started", r.ID)
	ran, owner, err := a.engine.Maintain(engine.WithNetworkRetry(ctx), r)
	if err != nil {
		return nil, err
	}
	return &agentpb.MaintainResponse{Ran: ran, Owner: owner, QueuedMs: queued.Milliseconds()}, nil
}

// TestRepository opens the repository at the location in the request without
// creating anything, and reports its identity and maintenance owner.
func (a *Agent) TestRepository(ctx context.Context, req *agentpb.TestRepositoryRequest) (*agentpb.TestRepositoryResponse, error) {
	r, err := toRepo(req.GetRepository())
	if err != nil {
		return nil, err
	}
	info, err := a.engine.Inspect(ctx, r)
	if err != nil {
		return nil, err
	}
	return &agentpb.TestRepositoryResponse{Warnings: info.Warnings, RepositoryId: info.ID, Owner: info.Owner}, nil
}

// MoveRepository renames the repository's directory to the location in the
// request. It waits for the agent's other operations, so nothing is reading
// the repository while it moves.
func (a *Agent) MoveRepository(ctx context.Context, req *agentpb.MoveRepositoryRequest) (*agentpb.MoveRepositoryResponse, error) {
	r, err := toRepo(req.GetRepository())
	if err != nil {
		return nil, err
	}
	var to repocfg.Config
	if err := json.Unmarshal(req.GetNewConfigJson(), &to); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "new repository config: %v", err)
	}
	unlock, lerr := a.opMu.lock(ctx)
	if lerr != nil {
		return nil, lerr
	}
	defer unlock()
	logStarted("repository move started", r.ID, "from", r.Config.Location(), "to", to.Location())
	if err := a.engine.MoveRepository(ctx, r.Config, to); err != nil {
		return nil, err
	}
	logStarted("repository moved", r.ID, "to", to.Location())
	return &agentpb.MoveRepositoryResponse{}, nil
}

// TakeMaintenance makes this agent the repository's maintenance owner.
func (a *Agent) TakeMaintenance(ctx context.Context, req *agentpb.TakeMaintenanceRequest) (*agentpb.TakeMaintenanceResponse, error) {
	r, err := toRepo(req.GetRepository())
	if err != nil {
		return nil, err
	}
	unlock, lerr := a.opMu.lock(ctx)
	if lerr != nil {
		return nil, lerr
	}
	defer unlock()
	logStarted("maintenance takeover started", r.ID)
	if err := a.engine.SetMaintenanceOwner(ctx, r, a.engine.UsernameAtHost()); err != nil {
		return nil, err
	}
	return &agentpb.TakeMaintenanceResponse{}, nil
}
