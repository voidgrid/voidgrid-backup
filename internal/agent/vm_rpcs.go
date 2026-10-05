package agent

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/voidgrid/voidgrid-backup/internal/engine"
	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
	"github.com/voidgrid/voidgrid-backup/internal/virt"
)

// SetHypervisor enables VM backups through hv.
func (a *Agent) SetHypervisor(hv virt.Hypervisor) { a.hv = hv }

func (a *Agent) hypervisor() (virt.Hypervisor, error) {
	if a.hv == nil {
		return nil, status.Error(codes.FailedPrecondition, "this agent has no libvirt socket configured (-libvirt-socket)")
	}
	return a.hv, nil
}

func (a *Agent) ListVMs(ctx context.Context, _ *agentpb.ListVMsRequest) (*agentpb.ListVMsResponse, error) {
	hv, err := a.hypervisor()
	if err != nil {
		return nil, err
	}
	doms, err := hv.Domains(ctx)
	if err != nil {
		return nil, err
	}
	resp := &agentpb.ListVMsResponse{}
	for _, d := range doms {
		vm := &agentpb.VM{Name: d.Name, Uuid: d.UUID, State: d.State}
		for _, disk := range d.Disks {
			vm.Disks = append(vm.Disks, &agentpb.VMDisk{
				Target: disk.Target, Device: disk.Device, Type: disk.Type, Source: disk.Source,
				Format: disk.Format, ReadOnly: disk.ReadOnly, Backing: disk.Backing, Backupable: disk.Backupable(),
				AgentReadOnly: agentReadOnly(disk.Source),
			})
		}
		resp.Vms = append(resp.Vms, vm)
	}
	return resp, nil
}

func (a *Agent) backupVM(ctx context.Context, r engine.Repo, spec *agentpb.VMSpec, keep engine.Retention) (*agentpb.BackupResponse, error) {
	hv, err := a.hypervisor()
	if err != nil {
		return nil, err
	}
	res, err := a.engine.BackupVM(ctx, r, hv, engine.VMSpec{
		Name: spec.GetName(), Disks: spec.GetDisks(), Quiesce: spec.GetQuiesce(),
	}, keep)
	if err != nil {
		return nil, err
	}
	return &agentpb.BackupResponse{Results: []*agentpb.PathResult{toPathResult(res)}}, nil
}

func (a *Agent) RestoreVM(ctx context.Context, req *agentpb.RestoreVMRequest) (*agentpb.RestoreResponse, error) {
	r, err := toRepo(req.GetRepository())
	if err != nil {
		return nil, err
	}
	hv, err := a.hypervisor()
	if err != nil {
		return nil, err
	}
	a.opMu.Lock()
	defer a.opMu.Unlock()
	logStarted("restore started", r.ID, "vm", req.GetSnapshotId(), "target", req.GetTargetDir())
	st, err := a.engine.RestoreVM(ctx, r, hv, req.GetSnapshotId(), req.GetTargetDir(), req.GetDefine())
	if err != nil {
		return nil, err
	}
	return toRestoreResponse(st), nil
}
