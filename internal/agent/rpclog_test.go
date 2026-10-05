package agent

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
)

// captureLog returns what slog wrote while the test ran.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

func TestLogRPCFailureIsLogged(t *testing.T) {
	buf := captureLog(t)
	logRPC("/agent.Agent/Backup", time.Second,
		&agentpb.BackupRequest{Repository: &agentpb.Repository{Id: "repo-7"}}, nil, errors.New("libvirt connect: no socket"))
	out := buf.String()
	for _, want := range []string{"level=WARN", "rpc failed", "method=Backup", "repo=repo-7", "libvirt connect: no socket"} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q lacks %q", out, want)
		}
	}
}

func TestLogRPCBackupResults(t *testing.T) {
	buf := captureLog(t)
	logRPC("/agent.Agent/Backup", time.Second, &agentpb.BackupRequest{Repository: &agentpb.Repository{Id: "repo-7"}}, &agentpb.BackupResponse{Results: []*agentpb.PathResult{
		{Path: "vm:debian13", Error: "create overlay snapshot: denied"},
		{Path: "/dockers/a", SnapshotId: "k123", Files: 3, Bytes: 10, Warnings: []string{"ACTION NEEDED: merge"}},
	}}, nil)
	out := buf.String()
	for _, want := range []string{
		"backup failed", "repo=repo-7", "path=vm:debian13", "create overlay snapshot: denied",
		"backup finished", "snapshot=k123", "backup warning", "ACTION NEEDED: merge",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q lacks %q", out, want)
		}
	}
	if strings.Count(out, "backup finished") != 1 {
		t.Errorf("a failed path must not log as finished:\n%s", out)
	}
}

func TestLogRPCRestoreAndCheck(t *testing.T) {
	buf := captureLog(t)
	logRPC("/agent.Agent/RestoreVM", time.Second, nil, &agentpb.RestoreResponse{Files: 2, Warnings: []string{"ownership not restored"}}, nil)
	logRPC("/agent.Agent/Check", time.Second, &agentpb.CheckRequest{Repository: &agentpb.Repository{Id: "repo-7"}}, &agentpb.CheckResponse{Snapshots: 4, Errors: []string{"missing blob"}, TestRestoreError: "bad"}, nil)
	out := buf.String()
	for _, want := range []string{
		"restore finished", "ownership not restored", "check finished", "repo=repo-7", "missing blob", "check test restore failed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q lacks %q", out, want)
		}
	}
}

func TestLogRPCQuietOnPolling(t *testing.T) {
	buf := captureLog(t)
	logRPC("/agent.Agent/Ping", time.Second, nil, &agentpb.PingResponse{}, nil)
	logRPC("/agent.Agent/ListSnapshots", time.Second, nil, &agentpb.ListSnapshotsResponse{}, nil)
	if buf.Len() != 0 {
		t.Errorf("successful polling and browsing RPCs must not log, got %q", buf.String())
	}
}
