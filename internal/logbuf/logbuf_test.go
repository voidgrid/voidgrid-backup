package logbuf

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

func logger(b *Buffer, out *bytes.Buffer) *slog.Logger {
	return slog.New(b.Handler(slog.NewTextHandler(out, &slog.HandlerOptions{Level: slog.LevelDebug})))
}

func TestCaptureAndForward(t *testing.T) {
	var out bytes.Buffer
	b := New(10)
	l := logger(b, &out)
	l.Info("backup finished", "job", "activity", "summary", "1 of 1 paths", "n", 3)
	if !strings.Contains(out.String(), "backup finished") {
		t.Fatalf("not forwarded to the wrapped handler: %q", out.String())
	}
	got := b.Entries(slog.LevelDebug, 0)
	if len(got) != 1 || got[0].Message != "backup finished" || got[0].Level != slog.LevelInfo || got[0].Time.IsZero() {
		t.Fatalf("entries: %+v", got)
	}
	if want := `job=activity summary="1 of 1 paths" n=3`; got[0].Attrs != want {
		t.Fatalf("attrs = %q, want %q", got[0].Attrs, want)
	}
}

func TestRingKeepsNewestInOrder(t *testing.T) {
	b := New(3)
	l := logger(b, &bytes.Buffer{})
	for i := 0; i < 5; i++ {
		l.Info(fmt.Sprint("m", i))
	}
	got := b.Entries(slog.LevelDebug, 0)
	if len(got) != 3 || got[0].Message != "m2" || got[2].Message != "m4" {
		t.Fatalf("ring: %+v", got)
	}
	if got[0].Seq != 2 || got[2].Seq != 4 {
		t.Fatalf("sequence numbers: %+v", got)
	}
	if last := b.Entries(slog.LevelDebug, 2); len(last) != 2 || last[0].Message != "m3" || last[1].Message != "m4" {
		t.Fatalf("limit takes the newest, oldest first: %+v", last)
	}
}

func TestLevelFilter(t *testing.T) {
	b := New(10)
	l := logger(b, &bytes.Buffer{})
	l.Debug("d")
	l.Info("i")
	l.Warn("w")
	l.Error("e")
	if got := b.Entries(slog.LevelWarn, 0); len(got) != 2 || got[0].Message != "w" || got[1].Message != "e" {
		t.Fatalf("warn and up: %+v", got)
	}
}

func TestWithAttrsAndGroups(t *testing.T) {
	b := New(10)
	l := logger(b, &bytes.Buffer{}).With("agent", "box").WithGroup("req").With("id", 7)
	l.Info("hello", "path", "/x", slog.Group("g", "k", "v"))
	got := b.Entries(slog.LevelDebug, 0)
	if len(got) != 1 {
		t.Fatalf("%+v", got)
	}
	for _, want := range []string{"agent=box", "req.id=7", "req.path=/x", "req.g.k=v"} {
		if !strings.Contains(got[0].Attrs, want) {
			t.Errorf("attrs %q missing %q", got[0].Attrs, want)
		}
	}
}

func TestSecretsAreNotKept(t *testing.T) {
	var out bytes.Buffer
	b := New(10)
	logger(b, &out).Info("not enrolled", "code", "ABCD-1234", "setup_token", "tokenvalue9", "client_secret", "secretvalue7", "host", "h")
	e := b.Entries(slog.LevelDebug, 0)[0]
	for _, leaked := range []string{"ABCD-1234", "tokenvalue9", "secretvalue7"} {
		if strings.Contains(e.Attrs, leaked) {
			t.Errorf("kept %q: %s", leaked, e.Attrs)
		}
	}
	if !strings.Contains(e.Attrs, "host=h") || strings.Count(e.Attrs, "[hidden]") != 3 {
		t.Errorf("attrs: %s", e.Attrs)
	}
	// The wrapped handler (docker logs) is not altered.
	if !strings.Contains(out.String(), "ABCD-1234") {
		t.Error("the wrapped handler's output was changed")
	}
}

func TestLongFieldsAreClipped(t *testing.T) {
	b := New(2)
	logger(b, &bytes.Buffer{}).Info(strings.Repeat("x", 5000), "k", strings.Repeat("y", 5000))
	e := b.Entries(slog.LevelDebug, 0)[0]
	if len(e.Message) > maxFieldLen+4 || len(e.Attrs) > maxFieldLen+4 {
		t.Fatalf("not clipped: %d, %d", len(e.Message), len(e.Attrs))
	}
}

func TestConcurrent(t *testing.T) {
	b := New(50)
	l := logger(b, &bytes.Buffer{})
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				l.Info("x", "i", i)
				b.Entries(slog.LevelDebug, 10)
			}
		}()
	}
	wg.Wait()
	if got := b.Entries(slog.LevelDebug, 0); len(got) != 50 || got[49].Seq != 1599 {
		t.Fatalf("after 1600 entries: %d entries, last seq %d", len(got), got[len(got)-1].Seq)
	}
}
