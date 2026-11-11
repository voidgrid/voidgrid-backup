package agent

import (
	"context"

	"github.com/voidgrid/voidgrid-backup/internal/engine"
	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
	"github.com/voidgrid/voidgrid-backup/internal/safeint"
)

func (a *Agent) Check(ctx context.Context, req *agentpb.CheckRequest) (*agentpb.CheckResponse, error) {
	r, err := toRepo(req.GetRepository())
	if err != nil {
		return nil, err
	}
	unlock, lerr := a.opMu.lock(ctx)
	if lerr != nil {
		return nil, lerr
	}
	defer unlock()
	logStarted("check started", r.ID, "verify_percent", req.GetVerifyPercent())
	res, err := a.engine.Check(engine.WithNetworkRetry(ctx), r, engine.CheckOptions{
		VerifyPercent:       req.GetVerifyPercent(),
		TestRestoreMaxBytes: req.GetTestRestoreMaxBytes(),
	})
	if err != nil {
		return nil, err
	}
	return &agentpb.CheckResponse{
		Snapshots:        safeint.Int32(res.Snapshots),
		ObjectsChecked:   res.ObjectsChecked,
		FilesRead:        res.FilesRead,
		BytesRead:        res.BytesRead,
		Errors:           res.Errors,
		TestRestore:      res.TestRestore,
		TestRestoreError: res.TestRestoreError,
	}, nil
}
