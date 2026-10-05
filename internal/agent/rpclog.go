package agent

import (
	"log/slog"
	"path"
	"strings"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
)

// logRPC records what an RPC did, so the agent's log tells the same story as
// the run the server shows. Every failed RPC is logged. Backups, restores and
// checks are also logged when they succeed, with their warnings and per-path
// errors (a backup RPC succeeds even when a path, stack or VM inside it
// failed). Browsing and polling RPCs stay quiet unless they fail.
func logRPC(method string, took time.Duration, req, resp any, err error) {
	method = path.Base(method)
	t := took.Round(time.Millisecond).String()
	repo := requestRepo(req)
	if err != nil {
		slog.Warn("rpc failed", "method", method, "repo", repo, "took", t, "err", err)
		return
	}
	switch r := resp.(type) {
	case *agentpb.BackupResponse:
		for _, res := range r.GetResults() {
			logBackupResult(res, repo, t)
		}
	case *agentpb.RestoreResponse:
		slog.Info("restore finished", "method", method, "repo", repo, "files", r.GetFiles(), "dirs", r.GetDirs(),
			"bytes", r.GetBytes(), "skipped", r.GetSkipped(), "took", t)
		for _, w := range r.GetWarnings() {
			slog.Warn("restore warning", "method", method, "repo", repo, "warning", w)
		}
	case *agentpb.CheckResponse:
		slog.Info("check finished", "repo", repo, "snapshots", r.GetSnapshots(), "objects", r.GetObjectsChecked(),
			"files_read", r.GetFilesRead(), "bytes_read", r.GetBytesRead(), "errors", len(r.GetErrors()), "took", t)
		for _, e := range r.GetErrors() {
			slog.Warn("check error", "repo", repo, "err", e)
		}
		if e := r.GetTestRestoreError(); e != "" {
			slog.Warn("check test restore failed", "repo", repo, "restored", r.GetTestRestore(), "err", e)
		}
	}
}

func logBackupResult(res *agentpb.PathResult, repo, took string) {
	if e := res.GetError(); e != "" {
		slog.Warn("backup failed", "repo", repo, "path", res.GetPath(), "took", took, "err", e)
	} else {
		slog.Info("backup finished", "repo", repo, "path", res.GetPath(), "snapshot", res.GetSnapshotId(),
			"files", res.GetFiles(), "bytes", res.GetBytes(), "unreadable", res.GetErrors(),
			"pruned", res.GetPruned(), "incomplete", res.GetIncomplete(), "took", took)
	}
	for _, w := range res.GetWarnings() {
		slog.Warn("backup warning", "repo", repo, "path", res.GetPath(), "warning", w)
	}
}

// requestRepo is the repository ID a request targets, or "" for requests
// that carry none (Ping, Logs, ...).
func requestRepo(req any) string {
	if r, ok := req.(interface{ GetRepository() *agentpb.Repository }); ok {
		return r.GetRepository().GetId()
	}
	return ""
}

// logStarted marks the moment a long operation begins, called once the host's
// operation lock is held, so a run in progress is visible in the log and not
// just its end.
func logStarted(msg, repoID string, attrs ...any) {
	slog.Info(msg, append([]any{"repo", repoID}, attrs...)...)
}

// logBackupStarted says what a backup request is about to read.
func logBackupStarted(repoID string, req *agentpb.BackupRequest) {
	switch {
	case req.GetStack() != nil:
		logStarted("backup started", repoID, "stack", req.GetStack().GetProject())
	case req.GetVm() != nil:
		logStarted("backup started", repoID, "vm", req.GetVm().GetName(),
			"disks", strings.Join(req.GetVm().GetDisks(), ","), "freeze", req.GetVm().GetQuiesce())
	default:
		logStarted("backup started", repoID, "paths", strings.Join(req.GetPaths(), ","))
	}
}
