package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
	"github.com/voidgrid/voidgrid-backup/internal/notify"
	"github.com/voidgrid/voidgrid-backup/internal/repocfg"
	"github.com/voidgrid/voidgrid-backup/internal/tidy"
)

// setupJob enrolls a local agent, creates a filesystem repository through
// it, and adds a job backing up a fresh directory.
func setupJob(t *testing.T) (*Controller, catalog.Job, string) {
	t.Helper()
	ctx := context.Background()
	a, addr := startAgent(t, t.TempDir())
	c := newController(t)
	ag, err := enrollTestAgent(t, c, a, "box", addr)
	if err != nil {
		t.Fatal(err)
	}
	cfg := repocfg.Config{Kind: repocfg.KindFilesystem, ECC: true,
		Filesystem: &repocfg.Filesystem{Path: filepath.Join(t.TempDir(), "repo")}}
	repo, generated, err := c.AddRepository(ctx, "local", cfg, "", ag.ID)
	if err != nil {
		t.Fatal(err)
	}
	if generated == "" || repo.InitializedAt.IsZero() {
		t.Fatalf("repository: generated=%q %+v", generated, repo)
	}
	if _, _, err := c.AddRepository(ctx, "local", cfg, "x", ag.ID); err == nil {
		t.Fatal("duplicate repository name accepted")
	}

	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "docs", "note.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "junk.tmp"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	job, err := c.AddJob(ctx, JobInput{
		Name: "docs", AgentID: ag.ID, RepositoryID: repo.ID,
		Paths: []string{src + "/", ""}, Excludes: []string{"*.tmp"},
		Schedule: "0 3 * * *", Keep: catalog.DefaultRetention, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(job.Paths) != 1 || job.Paths[0] != src {
		t.Fatalf("paths not cleaned: %q", job.Paths)
	}
	return c, job, src
}

func TestJobBackupBrowseRestore(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)

	run, err := c.RunJob(ctx, job.ID, "manual")
	if err != nil || run.Status != catalog.RunSuccess || run.Files != 1 {
		t.Fatalf("backup run: %+v %v", run, err)
	}
	snaps, err := c.Snapshots(ctx, job.ID)
	if err != nil || len(snaps) != 1 {
		t.Fatalf("snapshots: %+v %v", snaps, err)
	}
	root, err := c.Browse(ctx, job.ID, "", snaps[0].ID, "")
	if err != nil || len(root) != 1 || root[0].GetName() != "docs" || !root[0].GetDir() {
		t.Fatalf("browse root (junk.tmp must be excluded): %+v %v", root, err)
	}

	target := filepath.Join(t.TempDir(), "restored")
	rr, err := c.Restore(ctx, job.ID, "", snaps[0].ID, "docs", target, false)
	if err != nil || !restoreOK(rr) {
		t.Fatalf("restore run: %+v %v", rr, err)
	}
	if b, err := os.ReadFile(filepath.Join(target, "note.txt")); err != nil || string(b) != "hello" {
		t.Fatalf("restored note.txt: %q %v", b, err)
	}
	if rr, _ := c.Restore(ctx, job.ID, "", snaps[0].ID, "docs", target, false); rr.Status != catalog.RunFailed {
		t.Fatalf("restore into a non-empty target without overwrite: %+v", rr)
	}

	runs, err := c.Catalog.ListRuns(ctx, job.ID, 10)
	if err != nil || len(runs) != 3 || runs[0].Kind != "restore" || runs[2].Kind != "backup" {
		t.Fatalf("runs: %+v %v", runs, err)
	}
}

func TestRestoreOnAnotherAgent(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)
	if run, err := c.RunJob(ctx, job.ID, "manual"); err != nil || run.Status != catalog.RunSuccess {
		t.Fatalf("backup run: %+v %v", run, err)
	}
	snaps, err := c.Snapshots(ctx, job.ID)
	if err != nil || len(snaps) != 1 {
		t.Fatalf("snapshots: %+v %v", snaps, err)
	}
	a2, addr2 := startAgent(t, t.TempDir())
	other, err := enrollTestAgent(t, c, a2, "other", addr2)
	if err != nil {
		t.Fatal(err)
	}

	root, err := c.Browse(ctx, job.ID, other.ID, snaps[0].ID, "")
	if err != nil || len(root) != 1 || root[0].GetName() != "docs" {
		t.Fatalf("browse on the other agent: %+v %v", root, err)
	}
	target := filepath.Join(t.TempDir(), "restored")
	rr, err := c.Restore(ctx, job.ID, other.ID, snaps[0].ID, "docs", target, false)
	if err != nil || !restoreOK(rr) || !strings.Contains(rr.Summary, " on other") {
		t.Fatalf("restore on the other agent: %+v %v", rr, err)
	}
	if b, err := os.ReadFile(filepath.Join(target, "note.txt")); err != nil || string(b) != "hello" {
		t.Fatalf("restored note.txt: %q %v", b, err)
	}
	if _, err := c.Restore(ctx, job.ID, "no-such-agent", snaps[0].ID, "docs", filepath.Join(t.TempDir(), "x"), false); err == nil {
		t.Fatal("restore on an unknown agent accepted")
	}
}

func TestNotifyOnEveryFailure(t *testing.T) {
	ctx := context.Background()
	c, job, src := setupJob(t)

	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	if err := c.SetNotifyConfig(ctx, notify.Config{Discord: &notify.DiscordConfig{Enabled: true, WebhookURL: srv.URL}}); err != nil {
		t.Fatal(err)
	}

	// The source directory is gone, so the backup fails.
	if err := os.RemoveAll(src); err != nil {
		t.Fatal(err)
	}

	if run, err := c.RunJob(ctx, job.ID, "manual"); err != nil || run.Status != catalog.RunFailed {
		t.Fatalf("manual run: %+v %v", run, err)
	}
	if posts != 1 {
		t.Fatalf("a manual failure must notify too: %d posts", posts)
	}

	if run, err := c.RunJob(ctx, job.ID, "schedule"); err != nil || run.Status != catalog.RunFailed {
		t.Fatalf("scheduled run: %+v %v", run, err)
	}
	if posts != 2 {
		t.Fatalf("scheduled failure did not notify: %d posts", posts)
	}
}

func TestRunCheck(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)
	if _, err := c.RunJob(ctx, job.ID, "manual"); err != nil {
		t.Fatal(err)
	}

	// VerifyPercent is a spot check, so whether the one file's contents get
	// read is random; only the snapshot metadata check and test restore are
	// guaranteed to run.
	run, err := c.RunCheck(ctx, job.ID)
	if err != nil || run.Status != catalog.RunSuccess || run.Kind != "check" || !strings.Contains(run.Summary, "test restored") {
		t.Fatalf("check run: %+v %v", run, err)
	}

	runs, err := c.Catalog.ListRuns(ctx, job.ID, 10)
	if err != nil || len(runs) != 2 || runs[0].Kind != "check" || runs[1].Kind != "backup" {
		t.Fatalf("runs: %+v %v", runs, err)
	}
}

func TestAddJobValidation(t *testing.T) {
	ctx := context.Background()
	c, job, src := setupJob(t)
	base := JobInput{Name: "x", AgentID: job.AgentID, RepositoryID: job.RepositoryID,
		Paths: []string{src}, Keep: catalog.DefaultRetention}
	for name, mutate := range map[string]func(*JobInput){
		"root path":      func(in *JobInput) { in.Paths = []string{"/"} },
		"relative path":  func(in *JobInput) { in.Paths = []string{"data"} },
		"proc":           func(in *JobInput) { in.Paths = []string{"/proc/self"} },
		"no paths":       func(in *JobInput) { in.Paths = []string{" "} },
		"bad schedule":   func(in *JobInput) { in.Schedule = "every day" },
		"keeps nothing":  func(in *JobInput) { in.Keep = catalog.Retention{} },
		"negative keep":  func(in *JobInput) { in.Keep.Daily = -1 },
		"duplicate name": func(in *JobInput) { in.Name = job.Name },
		"unknown agent":  func(in *JobInput) { in.AgentID = "nope" },
		"unknown repo":   func(in *JobInput) { in.RepositoryID = "nope" },
		"empty name":     func(in *JobInput) { in.Name = "  " },
	} {
		in := base
		mutate(&in)
		if _, err := c.AddJob(ctx, in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestNextRun(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)

	created := time.Date(2026, 1, 1, 12, 0, 0, 0, time.Local)
	job.CreatedAt = created
	next, err := NextRun(ctx, c.Catalog, job)
	if err != nil || !next.Equal(time.Date(2026, 1, 2, 3, 0, 0, 0, time.Local)) {
		t.Fatalf("first run: %v %v", next, err)
	}
	id, _ := c.Catalog.StartRun(ctx, job.ID, "backup", "schedule", time.Date(2026, 1, 2, 3, 0, 5, 0, time.Local))
	if err := c.Catalog.FinishRun(ctx, catalog.Run{ID: id, Status: catalog.RunSuccess, FinishedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	next, err = NextRun(ctx, c.Catalog, job)
	if err != nil || !next.Equal(time.Date(2026, 1, 3, 3, 0, 0, 0, time.Local)) {
		t.Fatalf("after a run: %v %v", next, err)
	}
}

func TestSchedulerStartsDueJob(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)
	// Created at 03:00 yesterday with a daily 03:00 schedule: due now.
	c.startDue(ctx, time.Now().Add(48*time.Hour))
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if last, err := c.Catalog.LastRun(ctx, job.ID, "backup"); err == nil && last.Status != catalog.RunRunning {
			if last.Trigger != "schedule" || last.Status != catalog.RunSuccess {
				t.Fatalf("scheduled run: %+v", last)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("scheduler did not run the due job")
}

func TestPagesRender(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)
	run, err := c.RunJob(ctx, job.ID, "manual")
	if err != nil || run.Status != catalog.RunSuccess {
		t.Fatalf("%+v %v", run, err)
	}
	snaps, _ := c.Snapshots(ctx, job.ID)
	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	client := authedClient(t, c, srv)

	for _, p := range []string{"/", "/repositories", "/jobs", "/jobs/" + job.ID, "/jobs/" + job.ID + "/edit", "/logs",
		"/jobs/" + job.ID + "/snapshots/" + snaps[0].ID + "?path=docs",
		"/api/agents", "/api/repositories", "/api/jobs", "/api/jobs/" + job.ID + "/runs"} {
		resp, err := client.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		tidy.Close(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d: %s", p, resp.StatusCode, body)
		}
		if strings.Contains(string(body), "ZgotmplZ") {
			t.Errorf("GET %s: template sanitized an unsafe value", p)
		}
	}

	// The repository password never leaves the server through the API.
	resp, _ := client.Get(srv.URL + "/api/repositories")
	body, _ := io.ReadAll(resp.Body)
	tidy.Close(resp.Body)
	repo, _ := c.Catalog.Repository(ctx, job.RepositoryID)
	if strings.Contains(string(body), repo.Password) || strings.Contains(string(body), "password") {
		t.Fatalf("/api/repositories leaks the password: %s", body)
	}

	// Restore to / is refused before anything runs.
	form := url.Values{"target": {"/"}, "path": {""}}
	req, _ := http.NewRequest("POST", srv.URL+"/jobs/"+job.ID+"/snapshots/"+snaps[0].ID+"/restore", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	tidy.Close(resp.Body)
	if !strings.Contains(string(body), "never allowed") {
		t.Fatalf("restore to / was not refused: %d", resp.StatusCode)
	}
	if runs, _ := c.Catalog.ListRuns(ctx, job.ID, 10); len(runs) != 1 {
		t.Fatalf("a refused restore created a run: %+v", runs)
	}
}

// A restore is started by hand and can take hours, so its failure must alert
// like a scheduled backup's.
func TestNotifyWhenARestoreFails(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	if err := c.SetNotifyConfig(ctx, notify.Config{Discord: &notify.DiscordConfig{Enabled: true, WebhookURL: srv.URL}}); err != nil {
		t.Fatal(err)
	}
	if run, err := c.RunJob(ctx, job.ID, "schedule"); err != nil || run.Status != catalog.RunSuccess {
		t.Fatalf("backup: %+v %v", run, err)
	}
	snaps, _ := c.Snapshots(ctx, job.ID)
	target := filepath.Join(t.TempDir(), "restored")
	before := posts
	if rr, err := c.Restore(ctx, job.ID, "", snaps[0].ID, "docs", target, false); err != nil || !restoreOK(rr) {
		t.Fatalf("restore: %+v %v", rr, err)
	}
	if posts != before+1 {
		t.Fatalf("a manual restore that succeeded did not notify: %d posts", posts)
	}
	// Into a non-empty target without overwrite: it fails.
	if rr, _ := c.Restore(ctx, job.ID, "", snaps[0].ID, "docs", target, false); rr.Status != catalog.RunFailed {
		t.Fatalf("expected a failed restore: %+v", rr)
	}
	if posts != before+2 {
		t.Fatalf("a failed manual restore did not notify: %d posts", posts)
	}
}

func TestCancelRunStopsARestoreAndReportsIt(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)
	if err := c.CancelRun(job.ID, "me"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("cancelling nothing: %v", err)
	}
	// beginRun and CancelRun are what every run goes through.
	rctx, end := c.beginRun(ctx, job.ID)
	if err := c.CancelRun(job.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	if rctx.Err() == nil {
		t.Fatal("the run's context was not cancelled")
	}
	err := cancelledErr(rctx, context.Canceled)
	if err == nil || err.Error() != "cancelled by alice" {
		t.Fatalf("cancelledErr = %v", err)
	}
	if got := runError(err); got != "cancelled by alice" {
		t.Fatalf("recorded as %q", got)
	}
	end()
	if err := c.CancelRun(job.ID, "alice"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("a finished run must not be cancellable: %v", err)
	}
	// A run that ends on its own keeps its own error.
	rctx2, end2 := c.beginRun(ctx, job.ID)
	defer end2()
	if err := cancelledErr(rctx2, errors.New("disk full")); err == nil || err.Error() != "disk full" {
		t.Fatalf("unrelated error replaced: %v", err)
	}
}

func TestNotifyOnManualSuccess(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	if err := c.SetNotifyConfig(ctx, notify.Config{Discord: &notify.DiscordConfig{Enabled: true, WebhookURL: srv.URL}}); err != nil {
		t.Fatal(err)
	}
	if run, err := c.RunJob(ctx, job.ID, "schedule"); err != nil || run.Status != catalog.RunSuccess {
		t.Fatalf("scheduled run: %+v %v", run, err)
	}
	if posts != 0 {
		t.Fatalf("a scheduled success notified: %d posts", posts)
	}
	if run, err := c.RunJob(ctx, job.ID, "manual"); err != nil || run.Status != catalog.RunSuccess {
		t.Fatalf("manual run: %+v %v", run, err)
	}
	if posts != 1 {
		t.Fatalf("a manual success must notify: %d posts", posts)
	}
}
