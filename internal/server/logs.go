package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
)

const (
	logsTimeout     = 15 * time.Second
	defaultLogLines = 300
	maxLogLines     = 2000
)

// logSourceServer is the Logs page's source value for the server's own log.
const logSourceServer = "server"

// LogLine is one log line, ready to show.
type LogLine struct {
	Time    time.Time
	Level   slog.Level
	Message string
	Attrs   string
}

// Logs returns the newest limit log lines at or above min from source: the
// server itself, or an agent by ID. Oldest first.
func (c *Controller) Logs(ctx context.Context, source string, min slog.Level, limit int) ([]LogLine, error) {
	if limit <= 0 {
		limit = defaultLogLines
	}
	if limit > maxLogLines {
		limit = maxLogLines
	}
	if source == "" || source == logSourceServer {
		if c.LogBuf == nil {
			return nil, errors.New("the server is not capturing its log")
		}
		var out []LogLine
		for _, e := range c.LogBuf.Entries(min, limit) {
			out = append(out, LogLine{Time: e.Time, Level: e.Level, Message: e.Message, Attrs: e.Attrs})
		}
		return out, nil
	}
	agent, err := c.Catalog.Agent(ctx, source)
	if err != nil {
		return nil, fmt.Errorf("agent: %w", err)
	}
	resp, err := withAgent(c, agent, logsTimeout, func(ctx context.Context, cl agentpb.AgentClient) (*agentpb.LogsResponse, error) {
		return cl.Logs(ctx, &agentpb.LogsRequest{MinLevel: int32(min), Limit: int32(limit)})
	})
	if err != nil {
		return nil, err
	}
	out := make([]LogLine, 0, len(resp.GetEntries()))
	for _, e := range resp.GetEntries() {
		out = append(out, LogLine{Time: time.Unix(0, e.GetUnixNano()), Level: slog.Level(e.GetLevel()), Message: e.GetMessage(), Attrs: e.GetAttrs()})
	}
	return out, nil
}

type logSource struct {
	ID, Name string
	Selected bool
}

type logLevelOpt struct {
	Value, Label string
	Selected     bool
}

type logsPage struct {
	base
	Sources []logSource
	Levels  []logLevelOpt
	Limit   int
	Auto    bool
	Lines   []LogLine // newest first
}

var logLevels = []struct {
	value, label string
	level        slog.Level
}{
	{"debug", "all", slog.LevelDebug},
	{"info", "info and above", slog.LevelInfo},
	{"warn", "warnings and errors", slog.LevelWarn},
	{"error", "errors only", slog.LevelError},
}

func (u *ui) logs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	source, levelName := q.Get("source"), q.Get("level")
	if source == "" {
		source = logSourceServer
	}
	if levelName == "" {
		levelName = "info"
	}
	limit := defaultLogLines
	if n, err := strconv.Atoi(q.Get("limit")); err == nil {
		limit = n
	}
	if limit < 1 || limit > maxLogLines {
		limit = defaultLogLines
	}
	p := logsPage{base: u.newBase("Logs", "logs", r), Limit: limit, Auto: q.Get("auto") != ""}
	min := slog.LevelInfo
	for _, l := range logLevels {
		p.Levels = append(p.Levels, logLevelOpt{Value: l.value, Label: l.label, Selected: l.value == levelName})
		if l.value == levelName {
			min = l.level
		}
	}
	p.Sources = append(p.Sources, logSource{ID: logSourceServer, Name: "Server", Selected: source == logSourceServer})
	agents, _ := u.c.Catalog.ListAgents(ctx)
	for _, a := range agents {
		p.Sources = append(p.Sources, logSource{ID: a.ID, Name: "Agent " + a.Name, Selected: source == a.ID})
	}
	lines, err := u.c.Logs(ctx, source, min, limit)
	if err != nil {
		p.Error = "Could not read the log: " + err.Error()
	}
	for i := len(lines) - 1; i >= 0; i-- { // newest first
		p.Lines = append(p.Lines, lines[i])
	}
	u.render(w, http.StatusOK, "logs", p)
}
