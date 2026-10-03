package logbuf

import (
	"bytes"
	"log"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// What the mains do: make Buffer.Handler(NewStderrHandler(...)) the default.
// Plain log.Print is redirected to it by slog, so it is captured too, and
// nothing may loop.
func TestInstalledAsDefault(t *testing.T) {
	prevLogger, prevWriter, prevFlags := slog.Default(), log.Writer(), log.Flags()
	defer func() {
		slog.SetDefault(prevLogger)
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
	}()

	var out bytes.Buffer
	b := New(10)
	slog.SetDefault(slog.New(b.Handler(NewStderrHandler(&out, slog.LevelInfo))))
	done := make(chan struct{})
	go func() {
		slog.Info("via slog", "k", "two words")
		slog.Debug("below the level")
		log.Print("via log")
		slog.With("a", 1).WithGroup("g").Warn("with attrs", "x", "y")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("logging through the installed handler does not return")
	}

	var msgs []string
	for _, e := range b.Entries(slog.LevelDebug, 0) {
		msgs = append(msgs, e.Message)
	}
	if strings.Join(msgs, "|") != "via slog|via log|with attrs" {
		t.Fatalf("captured %q", msgs)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("stderr lines: %q", lines)
	}
	// "2006/01/02 15:04:05 INFO via slog k=\"two words\""
	if !strings.HasSuffix(lines[0], ` INFO via slog k="two words"`) || len(lines[0]) < len(`2006/01/02 15:04:05 `) || lines[0][4] != '/' {
		t.Errorf("line format: %q", lines[0])
	}
	if !strings.HasSuffix(lines[2], ` WARN with attrs a=1 g.x=y`) {
		t.Errorf("attrs and groups: %q", lines[2])
	}
}

func TestStderrHandlerKeepsSecretsForStderr(t *testing.T) {
	var out bytes.Buffer
	b := New(5)
	slog.New(b.Handler(NewStderrHandler(&out, slog.LevelInfo))).Info("not enrolled", "code", "ABCD-1234")
	if !strings.Contains(out.String(), "code=ABCD-1234") {
		t.Errorf("stderr must show the enrollment code, which operators copy from it: %q", out.String())
	}
	if strings.Contains(b.Entries(slog.LevelDebug, 0)[0].Attrs, "ABCD-1234") {
		t.Error("the buffer kept the code")
	}
}
