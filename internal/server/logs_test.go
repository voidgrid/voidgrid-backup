package server

import (
	"bytes"
	"context"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/voidgrid/voidgrid-backup/internal/logbuf"
)

func testLogger(b *logbuf.Buffer) *slog.Logger {
	return slog.New(b.Handler(slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelDebug})))
}

func TestLogsPage(t *testing.T) {
	ctx := context.Background()
	a, addr := startAgent(t, t.TempDir())
	agentBuf := logbuf.New(50)
	a.LogBuf = agentBuf
	testLogger(agentBuf).Warn("disk almost full", "mount", "/data")
	testLogger(agentBuf).Info("not enrolled: enter this code", "code", "SECRET-CODE-123")

	c := newController(t)
	serverBuf := logbuf.New(50)
	c.LogBuf = serverBuf
	sl := testLogger(serverBuf)
	sl.Info("backup finished", "job", "activity", "status", "success")
	sl.Error("repository unreachable", "err", "timeout")

	ag, err := c.Enroll(ctx, "box", addr, a.EnrollmentCode())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	client := authedClient(t, c, srv)

	page := get(t, client, srv.URL+"/logs", "backup finished")
	for _, want := range []string{"repository unreachable", "job=activity", "Agent box", `href="/logs"`} {
		if !strings.Contains(page, want) {
			t.Errorf("server log page missing %q", want)
		}
	}
	// Newest first.
	if strings.Index(page, "repository unreachable") > strings.Index(page, "backup finished") {
		t.Error("lines are not newest first")
	}

	// The level filter.
	errsOnly := get(t, client, srv.URL+"/logs?level=error", "repository unreachable")
	if strings.Contains(errsOnly, "backup finished") {
		t.Error("level=error still shows info lines")
	}

	// An agent's log comes over its own link, with secrets already hidden.
	apage := get(t, client, srv.URL+"/logs?source="+url.QueryEscape(ag.ID)+"&level=debug", "disk almost full")
	if !strings.Contains(apage, "mount=/data") {
		t.Errorf("agent log page: %s", apage)
	}
	if strings.Contains(apage, "SECRET-CODE-123") {
		t.Error("an enrollment code reached the page")
	}
	if strings.Contains(apage, "backup finished") {
		t.Error("the agent page shows the server's lines")
	}
}

func TestLogsErrors(t *testing.T) {
	ctx := context.Background()
	a, addr := startAgent(t, t.TempDir()) // captures nothing
	c := newController(t)
	ag, err := c.Enroll(ctx, "box", addr, a.EnrollmentCode())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Logs(ctx, logSourceServer, slog.LevelInfo, 10); err == nil {
		t.Error("server log with capture off should say so")
	}
	if _, err := c.Logs(ctx, ag.ID, slog.LevelInfo, 10); err == nil {
		t.Error("an agent that isn't capturing should say so")
	}
	if _, err := c.Logs(ctx, "no-such-agent", slog.LevelInfo, 10); err == nil {
		t.Error("unknown agent accepted")
	}

	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	client := authedClient(t, c, srv)
	get(t, client, srv.URL+"/logs", "Could not read the log")
	get(t, client, srv.URL+"/logs?source=nope&limit=abc", "Could not read the log") // junk input is tolerated
}

func TestLogsLimit(t *testing.T) {
	c := newController(t)
	c.LogBuf = logbuf.New(100)
	l := testLogger(c.LogBuf)
	for i := 0; i < 40; i++ {
		l.Info("line")
	}
	got, err := c.Logs(context.Background(), "", slog.LevelInfo, 7)
	if err != nil || len(got) != 7 {
		t.Fatalf("limit: %d %v", len(got), err)
	}
	if all, _ := c.Logs(context.Background(), "", slog.LevelInfo, 0); len(all) != 40 {
		t.Fatalf("default limit returned %d", len(all))
	}
}
