package server

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

type capturedLog []string

func (c *capturedLog) warn(msg string, args ...any) {
	*c = append(*c, strings.TrimSpace(msg+" "+fmt.Sprintln(args...)))
}

func TestRefusalLogBoundsAndCounts(t *testing.T) {
	var got capturedLog
	l := newRefusalLog(time.Minute, got.warn)
	t0 := time.Unix(1_000_000, 0)

	// A burst from one address: one line now, the rest counted.
	for i := 0; i < 40; i++ {
		l.record("203.0.113.5", t0.Add(time.Duration(i)*time.Second/2))
	}
	// Another address is logged separately.
	l.record("198.51.100.7", t0.Add(time.Second))
	if len(got) != 2 {
		t.Fatalf("burst should log one line per address, got %d: %q", len(got), got)
	}

	// The window ends: one summary with the count, the quiet address none.
	l.flush(t0.Add(2 * time.Minute))
	if len(got) != 3 || !strings.Contains(got[2], "203.0.113.5") || !strings.Contains(got[2], "39") {
		t.Fatalf("summary after the window: %q", got)
	}
	if len(l.sources) != 0 {
		t.Fatalf("flushed sources kept: %v", l.sources)
	}

	// A new attempt after a flush starts over with an immediate line.
	l.record("203.0.113.5", t0.Add(3*time.Minute))
	if len(got) != 4 || !strings.Contains(got[3], "wrong token") {
		t.Fatalf("after the window: %q", got)
	}
}

func TestRefusalLogSummaryWithoutFlush(t *testing.T) {
	var got capturedLog
	l := newRefusalLog(time.Minute, got.warn)
	t0 := time.Unix(1_000_000, 0)
	l.record("203.0.113.5", t0)
	l.record("203.0.113.5", t0.Add(time.Second))
	l.record("203.0.113.5", t0.Add(2*time.Second))
	// Next attempt after the window, before any flush: the count is written
	// first, then the new attempt.
	l.record("203.0.113.5", t0.Add(2*time.Minute))
	if len(got) != 3 || !strings.Contains(got[1], "count 2") || !strings.Contains(got[2], "wrong token") {
		t.Fatalf("got %q", got)
	}
}

func TestRefusalLogCapsSources(t *testing.T) {
	var got capturedLog
	l := newRefusalLog(time.Minute, got.warn)
	t0 := time.Unix(1_000_000, 0)
	for i := 0; i < maxRefusalSources+500; i++ {
		l.record(fmt.Sprintf("2001:db8::%x", i), t0)
	}
	if len(l.sources) > maxRefusalSources+1 {
		t.Fatalf("tracked %d sources", len(l.sources))
	}
	if len(got) != maxRefusalSources+1 {
		t.Fatalf("lines for many sources: %d", len(got))
	}
	l.flush(t0.Add(2 * time.Minute))
	if last := got[len(got)-1]; !strings.Contains(last, otherSources) || !strings.Contains(last, "499") {
		t.Fatalf("overflow summary: %q", last)
	}
}
