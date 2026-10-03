package agent

import (
	"context"

	"github.com/voidgrid/voidgrid-backup/internal/fsusage"
	"github.com/voidgrid/voidgrid-backup/internal/guard"
	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
)

// maxUsagePaths keeps one request from queuing an unbounded number of walks.
const maxUsagePaths = 32

// PathUsage reports the filesystem totals, and optionally the tree size, of
// host paths as this agent sees them. A path it can't measure comes back with
// Error set rather than failing the whole call.
func (a *Agent) PathUsage(ctx context.Context, req *agentpb.PathUsageRequest) (*agentpb.PathUsageResponse, error) {
	paths := req.GetPaths()
	if len(paths) > maxUsagePaths {
		paths = paths[:maxUsagePaths]
	}
	resp := &agentpb.PathUsageResponse{}
	for _, p := range paths {
		out := &agentpb.PathUsage{Path: p}
		resp.Usages = append(resp.Usages, out)
		clean, err := guard.SourcePath(p)
		if err != nil {
			out.Error = err.Error()
			continue
		}
		out.Path = clean
		u, err := fsusage.Measure(ctx, clean, req.GetWalk(), fsusage.DefaultLimits)
		if err != nil {
			out.Error = err.Error()
			continue
		}
		out.FsTotal, out.FsUsed, out.FsAvail = u.FSTotal, u.FSUsed, u.FSAvail
		out.DirBytes, out.Files = u.Bytes, u.Files
		out.Walked, out.Truncated = u.Walked, u.Truncated
	}
	return resp, nil
}

// RepoStats totals the blobs a repository holds in its storage.
func (a *Agent) RepoStats(ctx context.Context, req *agentpb.RepoStatsRequest) (*agentpb.RepoStatsResponse, error) {
	r, err := toRepo(req.GetRepository())
	if err != nil {
		return nil, err
	}
	a.opMu.Lock()
	defer a.opMu.Unlock()
	st, err := a.engine.RepoStats(ctx, r)
	if err != nil {
		return nil, err
	}
	return &agentpb.RepoStatsResponse{StoredBytes: st.StoredBytes, Blobs: st.Blobs, FsTotal: st.FSTotal, FsAvail: st.FSAvail}, nil
}
