package agent

import (
	"context"

	"github.com/voidgrid/voidgrid-backup/internal/engine"
	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
)

func (a *Agent) Check(ctx context.Context, req *agentpb.CheckRequest) (*agentpb.CheckResponse, error) {
	r, err := toRepo(req.GetRepository())
	if err != nil {
		return nil, err
	}
	a.opMu.Lock()
	defer a.opMu.Unlock()
	res, err := a.engine.Check(ctx, r, engine.CheckOptions{
		VerifyPercent:       req.GetVerifyPercent(),
		TestRestoreMaxBytes: req.GetTestRestoreMaxBytes(),
	})
	if err != nil {
		return nil, err
	}
	return &agentpb.CheckResponse{
		Snapshots:        int32(res.Snapshots),
		ObjectsChecked:   res.ObjectsChecked,
		FilesRead:        res.FilesRead,
		BytesRead:        res.BytesRead,
		Errors:           res.Errors,
		TestRestore:      res.TestRestore,
		TestRestoreError: res.TestRestoreError,
	}, nil
}
