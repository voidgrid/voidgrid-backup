package fsusage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func write(t *testing.T, p string, n int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, make([]byte, n), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMeasureWalk(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a"), 100)
	write(t, filepath.Join(dir, "sub", "b"), 50)
	// A symlink to a big outside file must not be followed or counted by size.
	outside := filepath.Join(t.TempDir(), "big")
	write(t, outside, 10_000)
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}

	u, err := Measure(context.Background(), dir, true, DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if u.Bytes != 150 {
		t.Errorf("Bytes = %d, want 150", u.Bytes)
	}
	if !u.Walked || u.Truncated {
		t.Errorf("Walked=%v Truncated=%v, want true/false", u.Walked, u.Truncated)
	}
	if u.FSTotal <= 0 || u.FSUsed < 0 || u.FSAvail < 0 {
		t.Errorf("implausible filesystem totals: %+v", u)
	}
}

func TestMeasureNoWalk(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a"), 100)
	u, err := Measure(context.Background(), dir, false, DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if u.Walked || u.Bytes != 0 {
		t.Errorf("no walk requested but got %+v", u)
	}
}

func TestMeasureTruncates(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a", "b", "c", "d"} {
		write(t, filepath.Join(dir, n), 10)
	}
	u, err := Measure(context.Background(), dir, true, Limits{MaxFiles: 2, MaxTime: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if !u.Truncated {
		t.Errorf("want Truncated with MaxFiles=2, got %+v", u)
	}
}

func TestMeasureMissing(t *testing.T) {
	if _, err := Measure(context.Background(), filepath.Join(t.TempDir(), "nope"), true, DefaultLimits); err == nil {
		t.Error("want an error for a missing path")
	}
}

func TestMeasureSingleFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "f")
	write(t, f, 42)
	u, err := Measure(context.Background(), f, true, DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if u.Bytes != 42 || u.Files != 1 {
		t.Errorf("got %+v, want 42 bytes / 1 file", u)
	}
}
