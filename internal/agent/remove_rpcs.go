package agent

import (
	"context"

	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
)

func (a *Agent) DeleteSnapshot(ctx context.Context, req *agentpb.DeleteSnapshotRequest) (*agentpb.DeleteSnapshotResponse, error) {
	r, err := toRepo(req.GetRepository())
	if err != nil {
		return nil, err
	}
	a.opMu.Lock()
	defer a.opMu.Unlock()
	logStarted("snapshot delete started", r.ID, "snapshot", req.GetSnapshotId())
	if err := a.engine.DeleteSnapshot(ctx, r, req.GetSnapshotId()); err != nil {
		return nil, err
	}
	return &agentpb.DeleteSnapshotResponse{}, nil
}

func (a *Agent) WipeRepository(ctx context.Context, req *agentpb.WipeRepositoryRequest) (*agentpb.WipeRepositoryResponse, error) {
	r, err := toRepo(req.GetRepository())
	if err != nil {
		return nil, err
	}
	a.opMu.Lock()
	defer a.opMu.Unlock()
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
	a.opMu.Lock()
	defer a.opMu.Unlock()
	logStarted("maintenance started", r.ID)
	ran, owner, err := a.engine.Maintain(ctx, r)
	if err != nil {
		return nil, err
	}
	return &agentpb.MaintainResponse{Ran: ran, Owner: owner}, nil
}
