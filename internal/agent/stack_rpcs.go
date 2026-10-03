package agent

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/voidgrid/voidgrid-backup/internal/docker"
	"github.com/voidgrid/voidgrid-backup/internal/engine"
	"github.com/voidgrid/voidgrid-backup/internal/guard"
	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
)

func (a *Agent) dockerClient() (*docker.Client, error) {
	if a.docker == nil {
		return nil, status.Error(codes.FailedPrecondition, "this agent has no Docker socket configured (-docker)")
	}
	return a.docker, nil
}

func (a *Agent) ListStacks(ctx context.Context, _ *agentpb.ListStacksRequest) (*agentpb.ListStacksResponse, error) {
	dk, err := a.dockerClient()
	if err != nil {
		return nil, err
	}
	stacks, err := dk.Stacks(ctx)
	if err != nil {
		return nil, err
	}
	resp := &agentpb.ListStacksResponse{}
	for _, st := range stacks {
		ps := &agentpb.Stack{Project: st.Project, WorkingDir: st.WorkingDir, ConfigFiles: st.ConfigFiles}
		for _, svc := range st.Services {
			pv := &agentpb.Service{
				Name: svc.Name, Container: svc.Container, Image: svc.Image, State: svc.State,
				DumpKind: svc.DumpKind, SqliteFiles: svc.SQLiteFiles,
			}
			for _, m := range svc.Mounts {
				ro, _ := guard.ReadOnly(m.Source)
				pv.Mounts = append(pv.Mounts, &agentpb.Mount{Type: m.Type, Source: m.Source, Destination: m.Destination, Rw: m.RW, AgentReadOnly: ro})
			}
			ps.Services = append(ps.Services, pv)
		}
		resp.Stacks = append(resp.Stacks, ps)
	}
	return resp, nil
}

func (a *Agent) backupStack(ctx context.Context, r engine.Repo, spec *agentpb.StackSpec, keep engine.Retention) (*agentpb.BackupResponse, error) {
	dk, err := a.dockerClient()
	if err != nil {
		return nil, err
	}
	res, err := a.engine.BackupStack(ctx, r, dk, engine.StackSpec{
		Project:    spec.GetProject(),
		WorkingDir: spec.GetWorkingDir(),
		Include:    spec.GetInclude(),
		Exclude:    spec.GetExclude(),
		Dumps:      spec.GetDumps(),
		Quiesce:    spec.GetQuiesce(),
	}, keep)
	if err != nil {
		return nil, err
	}
	return &agentpb.BackupResponse{Results: []*agentpb.PathResult{toPathResult(res)}}, nil
}

func (a *Agent) RestoreStack(ctx context.Context, req *agentpb.RestoreStackRequest) (*agentpb.RestoreResponse, error) {
	r, err := toRepo(req.GetRepository())
	if err != nil {
		return nil, err
	}
	dk, err := a.dockerClient()
	if err != nil {
		return nil, err
	}
	a.opMu.Lock()
	defer a.opMu.Unlock()
	st, err := a.engine.RestoreStack(ctx, r, dk, req.GetSnapshotId(), req.GetTargetRoot(), req.GetStopStack())
	if err != nil {
		return nil, err
	}
	return toRestoreResponse(st), nil
}

func (a *Agent) ImportDump(ctx context.Context, req *agentpb.ImportDumpRequest) (*agentpb.ImportDumpResponse, error) {
	r, err := toRepo(req.GetRepository())
	if err != nil {
		return nil, err
	}
	dk, err := a.dockerClient()
	if err != nil {
		return nil, err
	}
	a.opMu.Lock()
	defer a.opMu.Unlock()
	out, err := a.engine.ImportDump(ctx, r, dk, req.GetSnapshotId(), req.GetService())
	if err != nil {
		return nil, err
	}
	return &agentpb.ImportDumpResponse{Output: out}, nil
}
