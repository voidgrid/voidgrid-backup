package agent

import (
	"context"
	"encoding/json"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/voidgrid/voidgrid-backup/internal/engine"
	"github.com/voidgrid/voidgrid-backup/internal/guard"
	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
	"github.com/voidgrid/voidgrid-backup/internal/repocfg"
	"github.com/voidgrid/voidgrid-backup/internal/safeint"
)

func toRepo(r *agentpb.Repository) (engine.Repo, error) {
	if r == nil || r.GetId() == "" {
		return engine.Repo{}, status.Error(codes.InvalidArgument, "repository is required")
	}
	var cfg repocfg.Config
	if err := json.Unmarshal(r.GetConfigJson(), &cfg); err != nil {
		return engine.Repo{}, status.Errorf(codes.InvalidArgument, "repository config: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		return engine.Repo{}, status.Error(codes.InvalidArgument, err.Error())
	}
	return engine.Repo{ID: r.GetId(), Config: cfg, Password: r.GetPassword()}, nil
}

func (a *Agent) InitRepository(ctx context.Context, req *agentpb.InitRepositoryRequest) (*agentpb.InitRepositoryResponse, error) {
	r, err := toRepo(req.GetRepository())
	if err != nil {
		return nil, err
	}
	created, kh, err := a.engine.Init(ctx, r)
	if err != nil {
		if created {
			// The repository is in storage now. Aborted tells the server so: a
			// password it generated must still reach the operator.
			return nil, status.Errorf(codes.Aborted, "the repository was created, but then: %v", err)
		}
		return nil, err
	}
	return &agentpb.InitRepositoryResponse{Created: created, KnownHosts: kh}, nil
}

func (a *Agent) Backup(ctx context.Context, req *agentpb.BackupRequest) (*agentpb.BackupResponse, error) {
	r, err := toRepo(req.GetRepository())
	if err != nil {
		return nil, err
	}
	if len(req.GetPaths()) == 0 && req.GetStack() == nil && req.GetVm() == nil {
		return nil, status.Error(codes.InvalidArgument, "no paths to back up")
	}
	k := req.GetRetention()
	keep := engine.Retention{
		Latest:  int(k.GetKeepLatest()),
		Hourly:  int(k.GetKeepHourly()),
		Daily:   int(k.GetKeepDaily()),
		Weekly:  int(k.GetKeepWeekly()),
		Monthly: int(k.GetKeepMonthly()),
		Annual:  int(k.GetKeepAnnual()),
	}
	waiting := time.Now()
	unlock, lerr := a.opMu.lock(ctx)
	if lerr != nil {
		return nil, lerr
	}
	defer unlock()
	queued := time.Since(waiting)
	resp, err := a.runBackup(engine.WithNetworkRetry(ctx), r, req, keep)
	if resp != nil {
		resp.QueuedMs = queued.Milliseconds()
	}
	return resp, err
}

// runBackup does the backup once Backup holds the operation lock.
func (a *Agent) runBackup(ctx context.Context, r engine.Repo, req *agentpb.BackupRequest, keep engine.Retention) (*agentpb.BackupResponse, error) {
	logBackupStarted(r.ID, req)
	if spec := req.GetStack(); spec != nil {
		return a.backupStack(ctx, r, spec, keep)
	}
	if spec := req.GetVm(); spec != nil {
		return a.backupVM(ctx, r, spec, keep)
	}
	results, err := a.engine.Backup(ctx, r, req.GetPaths(), req.GetExcludes(), keep)
	if err != nil {
		return nil, err
	}
	resp := &agentpb.BackupResponse{}
	for _, res := range results {
		resp.Results = append(resp.Results, toPathResult(res))
	}
	return resp, nil
}

func toPathResult(res engine.PathResult) *agentpb.PathResult {
	pr := &agentpb.PathResult{
		Path:        res.Path,
		SnapshotId:  res.SnapshotID,
		Bytes:       res.Bytes,
		Files:       safeint.Int32(res.Files),
		Errors:      safeint.Int32(res.Errors),
		Pruned:      safeint.Int32(res.Pruned),
		Warnings:    res.Warnings,
		StartUnix:   res.Start,
		EndUnix:     res.End,
		Incomplete:  res.Incomplete,
		PrunedIds:   res.PrunedIDs,
		CachedFiles: safeint.Int32(res.CachedFiles),
		AddedBytes:  res.AddedBytes,
		Timings: &agentpb.PhaseTimings{
			OpenMs:        res.Timings.Open.Milliseconds(),
			PrepareMs:     res.Timings.Prepare.Milliseconds(),
			UploadMs:      res.Timings.Upload.Milliseconds(),
			RetentionMs:   res.Timings.Retention.Milliseconds(),
			CommitMs:      res.Timings.Commit().Milliseconds(),
			SaveMs:        res.Timings.Save.Milliseconds(),
			FlushMs:       res.Timings.Flush.Milliseconds(),
			CloseMs:       res.Timings.Close.Milliseconds(),
			MaintenanceMs: res.Timings.Maintenance.Milliseconds(),
		},
	}
	if res.Err != nil {
		pr.Error = res.Err.Error()
	}
	return pr
}

func (a *Agent) ListSnapshots(ctx context.Context, req *agentpb.ListSnapshotsRequest) (*agentpb.ListSnapshotsResponse, error) {
	r, err := toRepo(req.GetRepository())
	if err != nil {
		return nil, err
	}
	snaps, err := a.engine.ListSnapshots(ctx, r, req.GetPaths())
	if err != nil {
		return nil, err
	}
	resp := &agentpb.ListSnapshotsResponse{}
	for _, s := range snaps {
		resp.Snapshots = append(resp.Snapshots, &agentpb.Snapshot{
			Id:         s.ID,
			Path:       s.Path,
			StartUnix:  s.Start,
			EndUnix:    s.End,
			Bytes:      s.Bytes,
			Files:      safeint.Int32(s.Files),
			Errors:     safeint.Int32(s.Errors),
			Incomplete: s.Incomplete,
		})
	}
	return resp, nil
}

func (a *Agent) ListDirectory(ctx context.Context, req *agentpb.ListDirectoryRequest) (*agentpb.ListDirectoryResponse, error) {
	r, err := toRepo(req.GetRepository())
	if err != nil {
		return nil, err
	}
	entries, err := a.engine.ListDirectory(ctx, r, req.GetSnapshotId(), req.GetPath())
	if err != nil {
		return nil, err
	}
	resp := &agentpb.ListDirectoryResponse{}
	for _, e := range entries {
		resp.Entries = append(resp.Entries, &agentpb.DirEntry{
			Name: e.Name, Dir: e.Dir, Size: e.Size, ModUnix: e.ModUnix, Mode: e.Mode,
		})
	}
	return resp, nil
}

func (a *Agent) Restore(ctx context.Context, req *agentpb.RestoreRequest) (*agentpb.RestoreResponse, error) {
	r, err := toRepo(req.GetRepository())
	if err != nil {
		return nil, err
	}
	unlock, lerr := a.opMu.lock(ctx)
	if lerr != nil {
		return nil, lerr
	}
	defer unlock()
	logStarted("restore started", r.ID, "snapshot", req.GetSnapshotId(), "path", req.GetPath(), "target", req.GetTarget())
	st, err := a.engine.Restore(ctx, r, req.GetSnapshotId(), req.GetPath(), req.GetTarget(), req.GetOverwrite())
	if err != nil {
		return nil, err
	}
	return toRestoreResponse(st), nil
}

func toRestoreResponse(st engine.RestoreStats) *agentpb.RestoreResponse {
	return &agentpb.RestoreResponse{
		Bytes: st.Bytes, Files: safeint.Int32(st.Files), Dirs: safeint.Int32(st.Dirs), Skipped: safeint.Int32(st.Skipped),
		Warnings:         st.Warnings,
		OwnershipSkipped: st.OwnershipSkipped,
		AsUid:            safeint.Int32(st.AsUID),
		AsGid:            safeint.Int32(st.AsGID),
	}
}

func agentReadOnly(p string) bool {
	ro, _ := guard.ReadOnly(p)
	return ro
}
