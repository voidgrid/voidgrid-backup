package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// hasFormatBlob reports whether dir still holds a Kopia repository (its
// format blob is stored as kopia.repository plus a file extension).
func hasFormatBlob(t *testing.T, dir string) bool {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "kopia.repository*"))
	if err != nil {
		t.Fatal(err)
	}
	return len(m) > 0
}

func TestDeleteSnapshot(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)
	for i := 0; i < 2; i++ {
		if run, err := c.RunJob(ctx, job.ID, "manual"); err != nil {
			t.Fatalf("backup %d: %+v %v", i, run, err)
		}
		time.Sleep(1100 * time.Millisecond) // snapshots need distinct start times
	}
	snaps, err := c.Catalog.ListSnapshots(ctx, job.ID)
	if err != nil || len(snaps) != 2 {
		t.Fatalf("snapshots: %+v %v", snaps, err)
	}
	if err := c.DeleteSnapshot(ctx, job.ID, "not-a-snapshot"); err == nil {
		t.Fatal("an unknown snapshot must be refused")
	}
	if err := c.DeleteSnapshot(ctx, job.ID, snaps[0].ID); err != nil {
		t.Fatal(err)
	}
	left, err := c.Catalog.ListSnapshots(ctx, job.ID)
	if err != nil || len(left) != 1 || left[0].ID != snaps[1].ID {
		t.Fatalf("after delete: %+v %v", left, err)
	}
	// The repository agrees: a refresh from it lists one snapshot.
	listed, err := c.Snapshots(ctx, job.ID)
	if err != nil || len(listed) != 1 {
		t.Fatalf("repository still lists the deleted snapshot: %+v %v", listed, err)
	}
}

func TestRemoveRepositoryKeepsItsData(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)
	if _, err := c.RunJob(ctx, job.ID, "manual"); err != nil {
		t.Fatal(err)
	}
	repo, err := c.Catalog.Repository(ctx, job.RepositoryID)
	if err != nil {
		t.Fatal(err)
	}

	var inUse RepoInUseError
	if err := c.RemoveRepository(ctx, repo.ID); !errors.As(err, &inUse) || len(inUse.Jobs) != 1 {
		t.Fatalf("a repository with jobs must not be removed, got %v", err)
	}
	if err := c.Catalog.DeleteJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveRepository(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Catalog.Repository(ctx, repo.ID); err == nil {
		t.Fatal("record still there")
	}
	if !hasFormatBlob(t, repo.Config.Filesystem.Path) {
		t.Fatal("removing the record must not touch the data")
	}
}

func TestWipeRepository(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)
	if _, err := c.RunJob(ctx, job.ID, "manual"); err != nil {
		t.Fatal(err)
	}
	repo, err := c.Catalog.Repository(ctx, job.RepositoryID)
	if err != nil {
		t.Fatal(err)
	}

	var inUse RepoInUseError
	if err := c.StartWipe(ctx, repo.ID, job.AgentID); !errors.As(err, &inUse) {
		t.Fatalf("a repository with jobs must not be wiped, got %v", err)
	}
	if err := c.Catalog.DeleteJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.StartWipe(ctx, repo.ID, job.AgentID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for c.IsRunning(repoWipeKey(repo.ID)) {
		if time.Now().After(deadline) {
			t.Fatal("wipe did not finish")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := c.Catalog.Repository(ctx, repo.ID); err == nil {
		t.Fatalf("record still there, wipe error: %q", c.WipeError(ctx, repo.ID))
	}
	if hasFormatBlob(t, repo.Config.Filesystem.Path) {
		t.Fatal("repository data still in storage")
	}
}

func TestWipeFailureKeepsTheRecord(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)
	repo, err := c.Catalog.Repository(ctx, job.RepositoryID)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Catalog.DeleteJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	// Point at where the data used to be: nothing to wipe there.
	if err := os.RemoveAll(repo.Config.Filesystem.Path); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(repo.Config.Filesystem.Path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := c.StartWipe(ctx, repo.ID, job.AgentID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for c.IsRunning(repoWipeKey(repo.ID)) {
		if time.Now().After(deadline) {
			t.Fatal("wipe did not finish")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := c.Catalog.Repository(ctx, repo.ID); err != nil {
		t.Fatalf("a failed wipe must keep the record: %v", err)
	}
	if got := c.WipeError(ctx, repo.ID); !strings.Contains(got, "nothing was deleted") {
		t.Fatalf("wipe error not recorded: %q", got)
	}
}

// Every destructive route insists on the typed name or ID before it does anything.
func TestRemovalsNeedTheTypedName(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)
	if _, err := c.RunJob(ctx, job.ID, "manual"); err != nil {
		t.Fatal(err)
	}
	snaps, err := c.Catalog.ListSnapshots(ctx, job.ID)
	if err != nil || len(snaps) != 1 {
		t.Fatalf("snapshots: %+v %v", snaps, err)
	}
	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	client := authedClient(t, c, srv)
	post := func(path string, form url.Values) string {
		t.Helper()
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.Request.URL.Query().Get("notice")
	}

	if n := post("/jobs/"+job.ID+"/snapshots/"+snaps[0].ID+"/delete", url.Values{"confirm": {"nope"}}); !strings.Contains(n, "Type the snapshot ID") {
		t.Fatalf("snapshot delete without the ID: %q", n)
	}
	if n := post("/repositories/"+job.RepositoryID+"/remove", url.Values{"confirm": {"nope"}}); !strings.Contains(n, "Type the repository name") {
		t.Fatalf("remove without the name: %q", n)
	}
	if n := post("/repositories/"+job.RepositoryID+"/wipe", url.Values{"confirm": {"nope"}, "agent": {job.AgentID}}); !strings.Contains(n, "Type the repository name") {
		t.Fatalf("wipe without the name: %q", n)
	}
	if n := post("/repositories/"+job.RepositoryID+"/wipe", url.Values{"confirm": {"local"}, "agent": {job.AgentID}}); !strings.Contains(n, "still use this repository") {
		t.Fatalf("wipe of a repository with jobs: %q", n)
	}
	if left, _ := c.Catalog.ListSnapshots(ctx, job.ID); len(left) != 1 {
		t.Fatal("a refused request deleted a snapshot")
	}
	if _, err := c.Catalog.Repository(ctx, job.RepositoryID); err != nil {
		t.Fatalf("a refused request removed the repository: %v", err)
	}
	// With the right ID the snapshot goes.
	if n := post("/jobs/"+job.ID+"/snapshots/"+snaps[0].ID+"/delete", url.Values{"confirm": {shortID(snaps[0].ID)}}); !strings.Contains(n, "Deleted snapshot") {
		t.Fatalf("snapshot delete with the ID: %q", n)
	}
}
