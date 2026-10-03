package catalog

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAgentsAndReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "catalog.db")
	c, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	a := Agent{ID: "a1", Name: "box", Address: "10.0.0.5:9443", CertFingerprint: "ff", EnrolledAt: time.Now()}
	if err := c.AddAgent(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := c.AddAgent(ctx, Agent{ID: "a2", Name: "box", Address: "x:1", CertFingerprint: "ee", EnrolledAt: time.Now()}); err == nil {
		t.Fatal("duplicate agent name accepted")
	}
	if err := c.RecordFailure(ctx, "a1", "connection refused"); err != nil {
		t.Fatal(err)
	}
	if err := c.RecordFailure(ctx, "nope", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("RecordFailure on unknown agent: %v", err)
	}
	c.Close()

	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("catalog mode: %v %v", fi.Mode(), err)
	}

	// Reopening re-runs migrations, which must be a no-op.
	c, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	got, err := c.AgentByName(ctx, "box")
	if err != nil {
		t.Fatal(err)
	}
	if got.LastError != "connection refused" || !got.LastSeen.IsZero() {
		t.Fatalf("after failure: %+v", got)
	}

	seen := time.Now()
	if err := c.RecordSeen(ctx, "a1", seen, "host", "v1", []string{"runs as uid 1000"}); err != nil {
		t.Fatal(err)
	}
	list, err := c.ListAgents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].LastError != "" || !list[0].LastSeen.Equal(seen.UTC()) || list[0].Hostname != "host" ||
		len(list[0].Warnings) != 1 || list[0].Warnings[0] != "runs as uid 1000" {
		t.Fatalf("after seen: %+v", list)
	}
	if _, err := c.Agent(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Agent(missing): %v", err)
	}
}

func TestSettings(t *testing.T) {
	ctx := context.Background()
	c, err := Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if v, err := c.GetSetting(ctx, "notify"); err != nil || v != "" {
		t.Fatalf("unset setting: %q %v", v, err)
	}
	if err := c.SetSetting(ctx, "notify", "one"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetSetting(ctx, "notify", "two"); err != nil {
		t.Fatal(err)
	}
	if v, err := c.GetSetting(ctx, "notify"); err != nil || v != "two" {
		t.Fatalf("after update: %q %v", v, err)
	}
}
