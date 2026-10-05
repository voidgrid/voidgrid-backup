package server

import (
	"context"
	"testing"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
)

func fixedJitter(t *testing.T, d time.Duration) {
	t.Helper()
	old := maintRand
	maintRand = func(time.Duration) time.Duration { return d }
	t.Cleanup(func() { maintRand = old })
}

func waitIdle(t *testing.T, c *Controller, key string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for c.IsRunning(key) {
		if time.Now().After(deadline) {
			t.Fatal("still running")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestMaintainRepositoryRecordsAndSchedules(t *testing.T) {
	ctx := context.Background()
	fixedJitter(t, 30*time.Minute)
	c, job, _ := setupJob(t)

	before := time.Now()
	m, err := c.MaintainRepository(ctx, job.RepositoryID)
	if err != nil || m.Status != "ok" || m.AgentID != job.AgentID {
		t.Fatalf("maintain: %+v %v", m, err)
	}
	if want := before.Add(maintInterval + 30*time.Minute); m.NextDue.Before(want) || m.NextDue.After(want.Add(time.Minute)) {
		t.Fatalf("next due %v, want about %v (24h plus the jitter)", m.NextDue, want)
	}
	got, ok := c.RepoMaintenance(ctx, job.RepositoryID)
	if !ok || got.Status != "ok" || got.LastRun.IsZero() {
		t.Fatalf("saved state: %+v ok=%v", got, ok)
	}
}

// Kopia lets only the owner run maintenance: when the first agent asked is not
// it, the server follows the owner named in the answer.
func TestMaintainRepositoryFindsTheOwner(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t) // agent A created the repository and owns maintenance
	b, addrB := startAgent(t, t.TempDir())
	agB, err := enrollTestAgent(t, c, b, "box2", addrB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.AddJob(ctx, JobInput{Name: "b-job", AgentID: agB.ID, RepositoryID: job.RepositoryID,
		Paths: []string{t.TempDir()}, Keep: catalog.DefaultRetention}); err != nil {
		t.Fatal(err)
	}
	if err := c.Catalog.DeleteJob(ctx, job.ID); err != nil { // only B has a job now, so B is asked first
		t.Fatal(err)
	}
	m, err := c.MaintainRepository(ctx, job.RepositoryID)
	if err != nil || m.Status != "ok" || m.AgentID != job.AgentID {
		t.Fatalf("the owner (agent A) must run it: %+v %v", m, err)
	}
}

func TestStartMaintenanceSchedule(t *testing.T) {
	ctx := context.Background()
	fixedJitter(t, time.Hour)
	c, job, _ := setupJob(t)
	now := time.Now()

	// First sight: a random start time is recorded, nothing runs.
	c.startMaintenance(ctx, now)
	m, ok := c.RepoMaintenance(ctx, job.RepositoryID)
	if !ok || !m.LastRun.IsZero() || !m.NextDue.Equal(now.Add(time.Hour)) {
		t.Fatalf("first sight: %+v ok=%v", m, ok)
	}
	waitIdle(t, c, repoMaintRunKey(job.RepositoryID))
	if m2, _ := c.RepoMaintenance(ctx, job.RepositoryID); !m2.LastRun.IsZero() {
		t.Fatal("a repository not yet due must not run")
	}

	// Due, but a job on it is running: wait for the next tick.
	if !c.inflight.start(job.ID) {
		t.Fatal("job already running")
	}
	c.startMaintenance(ctx, now.Add(2*time.Hour))
	waitIdle(t, c, repoMaintRunKey(job.RepositoryID))
	if m2, _ := c.RepoMaintenance(ctx, job.RepositoryID); !m2.LastRun.IsZero() {
		t.Fatal("maintenance started while a job was running")
	}
	c.inflight.done(job.ID)

	// Due and idle: it runs.
	c.startMaintenance(ctx, now.Add(2*time.Hour))
	deadline := time.Now().Add(60 * time.Second)
	for {
		if m2, _ := c.RepoMaintenance(ctx, job.RepositoryID); !m2.LastRun.IsZero() {
			if m2.Status != "ok" || m2.NextDue.Before(time.Now().Add(maintInterval)) {
				t.Fatalf("after the run: %+v", m2)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("due repository did not run")
		}
		time.Sleep(50 * time.Millisecond)
	}
	waitIdle(t, c, repoMaintRunKey(job.RepositoryID))
}

// A repository with no jobs is never scheduled; its button still works.
func TestStartMaintenanceSkipsRepositoriesWithoutJobs(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)
	if err := c.Catalog.DeleteJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	c.startMaintenance(ctx, time.Now())
	if _, ok := c.RepoMaintenance(ctx, job.RepositoryID); ok {
		t.Fatal("a repository without jobs must not be scheduled")
	}
	if m, err := c.MaintainRepository(ctx, job.RepositoryID); err != nil || m.Status != "ok" {
		t.Fatalf("manual run: %+v %v", m, err)
	}
}
