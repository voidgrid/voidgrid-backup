package agent

import (
	"context"
	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
)

const (
	defaultLogLimit = 500
	maxLogLimit     = 2000
)

// Logs returns the agent's recent log lines, oldest first.
func (a *Agent) Logs(_ context.Context, req *agentpb.LogsRequest) (*agentpb.LogsResponse, error) {
	if a.LogBuf == nil {
		return nil, status.Error(codes.FailedPrecondition, "this agent is not capturing its log")
	}
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = defaultLogLimit
	}
	if limit > maxLogLimit {
		limit = maxLogLimit
	}
	resp := &agentpb.LogsResponse{}
	for _, e := range a.LogBuf.Entries(slog.Level(req.GetMinLevel()), limit) {
		resp.Entries = append(resp.Entries, &agentpb.LogEntry{
			Seq: e.Seq, UnixNano: e.Time.UnixNano(), Level: int32(e.Level), Message: e.Message, Attrs: e.Attrs,
		})
	}
	return resp, nil
}
