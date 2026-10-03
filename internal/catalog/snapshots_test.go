package catalog

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/repocfg"
)

func TestSnapshotRecord(t *testing.T) {
	ctx := context.Background()
	c, err := Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	now := time.Now().Truncate(time.Second)
	if err := c.AddAgent(ctx, Agent{ID: "a1", Name: "box", Address: "x:1", CertFingerprint: "f", EnrolledAt: now}); err != nil {
		t.Fatal(err)
	}
	cfg := repocfg.Config{Kind: repocfg.KindFilesystem, Filesystem: &repocfg.Filesystem{Path: "/r"}}
	if err := c.AddRepository(ctx, Repository{ID: "r1", Name: "r", Config: cfg, Password: "pw", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"j1", "j2"} {
		if err := c.AddJob(ctx, Job{ID: id, Name: id, AgentID: "a1", RepositoryID: "r1", Paths: []string{"/srv"},
			Keep: DefaultRetention, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	snap := func(id string, age time.Duration, files int) Snapshot {
		return Snapshot{ID: id, Path: "/srv", Start: now.Add(-age), End: now.Add(-age + time.Minute), Bytes: 100, Files: files, Errors: 1, Incomplete: "x"}
	}

	if err := c.UpsertSnapshots(ctx, "j1", []Snapshot{snap("old", 48*time.Hour, 1), snap("mid", 24*time.Hour, 2)}, nil); err != nil {
		t.Fatal(err)
	}
	got, err := c.ListSnapshots(ctx, "j1")
	if err != nil || len(got) != 2 || got[0].ID != "mid" || got[1].ID != "old" {
		t.Fatalf("newest first: %+v %v", got, err)
	}
	if g := got[0]; g.Files != 2 || g.Bytes != 100 || g.Errors != 1 || g.Incomplete != "x" || g.Path != "/srv" ||
		!g.Start.Equal(now.Add(-24*time.Hour)) || !g.End.Equal(now.Add(-24*time.Hour+time.Minute)) {
		t.Fatalf("fields round trip: %+v", g)
	}

	// Upsert replaces a row with the same ID, adds new ones and drops the named ones, together.
	if err := c.UpsertSnapshots(ctx, "j1", []Snapshot{snap("mid", 24*time.Hour, 9), snap("new", 0, 3)}, []string{"old"}); err != nil {
		t.Fatal(err)
	}
	got, _ = c.ListSnapshots(ctx, "j1")
	if len(got) != 2 || got[0].ID != "new" || got[1].ID != "mid" || got[1].Files != 9 {
		t.Fatalf("after upsert+delete: %+v", got)
	}

	// Jobs are separate even on the same path and snapshot ID.
	if err := c.UpsertSnapshots(ctx, "j2", []Snapshot{snap("new", 0, 3)}, nil); err != nil {
		t.Fatal(err)
	}
	if other, _ := c.ListSnapshots(ctx, "j2"); len(other) != 1 {
		t.Fatalf("j2: %+v", other)
	}

	// Replace makes the list exactly what it is given.
	if err := c.ReplaceSnapshots(ctx, "j1", []Snapshot{snap("only", 0, 1)}); err != nil {
		t.Fatal(err)
	}
	got, _ = c.ListSnapshots(ctx, "j1")
	if len(got) != 1 || got[0].ID != "only" {
		t.Fatalf("after replace: %+v", got)
	}
	if other, _ := c.ListSnapshots(ctx, "j2"); len(other) != 1 {
		t.Fatalf("replace touched another job: %+v", other)
	}

	// Deleting a job takes its snapshot record with it.
	if err := c.DeleteJob(ctx, "j2"); err != nil {
		t.Fatal(err)
	}
	if other, _ := c.ListSnapshots(ctx, "j2"); len(other) != 0 {
		t.Fatalf("record outlived its job: %+v", other)
	}
}
