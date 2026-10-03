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

func TestRegistrations(t *testing.T) {
	ctx := context.Background()
	c, err := Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	now := time.Now()
	r, err := c.RegisterAgent(ctx, Registration{ID: "r1", Fingerprint: "f1", AgentCert: []byte("cert"),
		Hostname: "box", Address: "10.0.0.5:9443", Version: "v1", CreatedAt: now})
	if err != nil || r.Status != RegPending || r.ID != "r1" || r.LastPoll.IsZero() {
		t.Fatalf("register: %+v %v", r, err)
	}
	// The same agent registering again keeps its ID and status, refreshes details.
	r, err = c.RegisterAgent(ctx, Registration{ID: "other", Fingerprint: "f1", AgentCert: []byte("cert"),
		Hostname: "box2", Address: "10.0.0.6:9443", Version: "v2", CreatedAt: now})
	if err != nil || r.ID != "r1" || r.Hostname != "box2" || r.Address != "10.0.0.6:9443" || r.Version != "v2" {
		t.Fatalf("re-register: %+v %v", r, err)
	}
	if _, err := c.RegisterAgent(ctx, Registration{ID: "r2", Fingerprint: "f2", AgentCert: []byte("c2"),
		Address: "10.0.0.7:9443", CreatedAt: now.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	if list, err := c.ListRegistrations(ctx); err != nil || len(list) != 2 || list[0].ID != "r1" {
		t.Fatalf("list: %+v %v", list, err)
	}

	if err := c.ApproveRegistration(ctx, "r1", "a1", []byte("issued")); err != nil {
		t.Fatal(err)
	}
	if err := c.ApproveRegistration(ctx, "r1", "a1", []byte("issued")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("approving twice: %v", err)
	}
	if got, err := c.RegistrationByFingerprint(ctx, "f1"); err != nil || got.Status != RegApproved || got.AgentID != "a1" || string(got.IssuedCert) != "issued" {
		t.Fatalf("approved: %+v %v", got, err)
	}
	if err := c.RejectRegistration(ctx, "r2"); err != nil {
		t.Fatal(err)
	}
	if err := c.RejectRegistration(ctx, "r1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rejecting an approved registration: %v", err)
	}
	if list, _ := c.ListRegistrations(ctx); len(list) != 1 || list[0].Status != RegRejected {
		t.Fatalf("approved rows should not be listed: %+v", list)
	}

	if err := c.DeleteUndecidedRegistrations(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RegistrationByID(ctx, "r2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rejected row should be gone: %v", err)
	}
	if _, err := c.RegistrationByID(ctx, "r1"); err != nil {
		t.Fatalf("approved row must survive: %v", err)
	}
	if err := c.DeleteApprovedRegistrations(ctx, "a1"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RegistrationByID(ctx, "r1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("approved row should be gone: %v", err)
	}
}

func TestRenameAndReplaceAgent(t *testing.T) {
	ctx := context.Background()
	c, err := Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.AddAgent(ctx, Agent{ID: "a1", Name: "box", Address: "10.0.0.5:9443", CertFingerprint: "old", EnrolledAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := c.RenameAgent(ctx, "a1", "renamed"); err != nil {
		t.Fatal(err)
	}
	if err := c.RenameAgent(ctx, "missing", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rename missing: %v", err)
	}
	if err := c.RecordFailure(ctx, "a1", "boom"); err != nil {
		t.Fatal(err)
	}
	if err := c.ReplaceAgent(ctx, "a1", "10.0.0.9:9500", "newhost", "v2", "new"); err != nil {
		t.Fatal(err)
	}
	a, err := c.Agent(ctx, "a1")
	if err != nil || a.ID != "a1" || a.Name != "renamed" || a.Address != "10.0.0.9:9500" || a.CertFingerprint != "new" || a.LastError != "" || a.Hostname != "newhost" {
		t.Fatalf("after replace: %+v %v", a, err)
	}
}
