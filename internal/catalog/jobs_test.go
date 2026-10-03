package catalog

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/repocfg"
)

func TestRepositoriesJobsRuns(t *testing.T) {
	ctx := context.Background()
	c, err := Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	now := time.Now()

	if err := c.AddAgent(ctx, Agent{ID: "a1", Name: "box", Address: "x:1", CertFingerprint: "f", EnrolledAt: now}); err != nil {
		t.Fatal(err)
	}
	cfg := repocfg.Config{Kind: repocfg.KindSFTP, ECC: true, SFTP: &repocfg.SFTP{Host: "h", Port: 23, User: "u", Path: "p", KeyFile: "/k"}}
	if err := c.AddRepository(ctx, Repository{ID: "r1", Name: "box", Config: cfg, Password: "pw", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	cfg.SFTP.KnownHosts = "[h]:23 ssh-ed25519 AAAA\n"
	if err := c.MarkRepositoryInitialized(ctx, "r1", cfg, now); err != nil {
		t.Fatal(err)
	}
	r, err := c.Repository(ctx, "r1")
	if err != nil || r.Password != "pw" || r.InitializedAt.IsZero() || r.Config.SFTP.KnownHosts == "" || !r.Config.ECC {
		t.Fatalf("repository: %+v %v", r, err)
	}

	j := Job{ID: "j1", Name: "docs", AgentID: "a1", RepositoryID: "r1", Paths: []string{"/srv/docs"},
		Schedule: "0 3 * * *", Keep: DefaultRetention, Enabled: true, CreatedAt: now}
	if err := c.AddJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	bad := j
	bad.ID, bad.Name, bad.AgentID = "j2", "other", "nope"
	if err := c.AddJob(ctx, bad); err == nil {
		t.Fatal("job with an unknown agent accepted (foreign keys off?)")
	}
	got, err := c.JobByName(ctx, "docs")
	if err != nil || got.Paths[0] != "/srv/docs" || got.Excludes == nil || got.Keep != DefaultRetention || !got.Enabled {
		t.Fatalf("job: %+v %v", got, err)
	}

	id, err := c.StartRun(ctx, "j1", "backup", "manual", now)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := c.FailInterruptedRuns(ctx, now); err != nil || n != 1 {
		t.Fatalf("FailInterruptedRuns: %d %v", n, err)
	}
	id2, _ := c.StartRun(ctx, "j1", "backup", "schedule", now.Add(time.Minute))
	if err := c.FinishRun(ctx, Run{ID: id2, Status: RunSuccess, FinishedAt: now.Add(2 * time.Minute), Bytes: 10, Files: 2}); err != nil {
		t.Fatal(err)
	}
	last, err := c.LastRun(ctx, "j1", "backup")
	if err != nil || last.ID != id2 || last.Status != RunSuccess || last.Files != 2 {
		t.Fatalf("last run: %+v %v", last, err)
	}
	runs, err := c.ListRuns(ctx, "j1", 10)
	if err != nil || len(runs) != 2 || runs[1].ID != id || runs[1].Status != RunFailed {
		t.Fatalf("runs: %+v %v", runs, err)
	}

	if got.Kind != JobPaths || got.Stack != nil {
		t.Fatalf("paths job kind/stack: %q %+v", got.Kind, got.Stack)
	}
	sj := Job{ID: "j3", Name: "notes", Kind: JobStack, AgentID: "a1", RepositoryID: "r1", Keep: DefaultRetention, CreatedAt: now,
		Stack: &StackConfig{Project: "notes", WorkingDir: "/srv/notes", Include: []string{"/srv/media"},
			Dumps: map[string]string{"db": "postgres"}, Quiesce: "pause"}}
	if err := c.AddJob(ctx, sj); err != nil {
		t.Fatal(err)
	}
	gotStack, err := c.Job(ctx, "j3")
	if err != nil || gotStack.Kind != JobStack || gotStack.Stack == nil || gotStack.Stack.Dumps["db"] != "postgres" || gotStack.Stack.Quiesce != "pause" {
		t.Fatalf("stack job: %+v %v", gotStack, err)
	}

	vj := Job{ID: "j4", Name: "ha-vm", Kind: JobVM, AgentID: "a1", RepositoryID: "r1", Keep: DefaultRetention, CreatedAt: now,
		VM: &VMConfig{Name: "home-assistant", Disks: []string{"vda"}, Quiesce: true}}
	if err := c.AddJob(ctx, vj); err != nil {
		t.Fatal(err)
	}
	if got, err := c.Job(ctx, "j4"); err != nil || got.Kind != JobVM || got.VM == nil || got.VM.Name != "home-assistant" || !got.VM.Quiesce || got.Stack != nil {
		t.Fatalf("vm job: %+v %v", got, err)
	}

	if err := c.DeleteJob(ctx, "j1"); err != nil {
		t.Fatal(err)
	}
	if runs, _ := c.ListRuns(ctx, "j1", 10); len(runs) != 0 {
		t.Fatal("runs survived job deletion")
	}
	if _, err := c.Job(ctx, "j1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted job: %v", err)
	}
}
