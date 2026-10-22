package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
)

func TestStatusPage(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t) // one agent, one repository, one scheduled job that has never run
	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	client := authedClient(t, c, srv)

	// Home is the status page, and every line links to where to look closer.
	page := get(t, client, srv.URL+"/", "Status")
	for _, want := range []string{`href="/agents"`, `href="/jobs"`, `href="/repositories"`, `href="/schedule"`,
		"1 of 1 reachable", "1 job", "1 never run", "1 repository", "maintenance ok", "docs"} {
		if !strings.Contains(page, want) {
			t.Errorf("the status page is missing %q:\n%s", want, page)
		}
	}
	// The navigation starts with Status, and Agents has its own address.
	if !strings.Contains(page, `<a href="/" class="on">Status</a>`) || !strings.Contains(page, `<a href="/agents" >Agents</a>`) {
		t.Errorf("navigation: %s", page)
	}
	get(t, client, srv.URL+"/agents", "Waiting for approval")

	// A good backup: all good.
	if run, err := c.RunJob(ctx, job.ID, "manual"); err != nil || run.Status != catalog.RunSuccess {
		t.Fatalf("backup: %+v %v", run, err)
	}
	page = get(t, client, srv.URL+"/", "all good")
	if !strings.Contains(page, "last backups all ok") {
		t.Errorf("a good backup is not reported as ok:\n%s", page)
	}

	// An agent that cannot be reached, and one waiting for approval.
	if err := c.Catalog.RecordFailure(ctx, job.AgentID, "connection refused"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Catalog.RegisterAgent(ctx, catalog.Registration{ID: "reg1", Fingerprint: "fp1", AgentCert: []byte{1},
		Hostname: "newbox", Address: "10.0.0.9:9443", Version: "v", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	page = get(t, client, srv.URL+"/", "unreachable")
	for _, want := range []string{"0 of 1 reachable", "box unreachable", "1 waiting for approval", "1 thing needs attention"} {
		if !strings.Contains(page, want) {
			t.Errorf("missing %q:\n%s", want, page)
		}
	}
	if err := c.Catalog.RecordSeen(ctx, job.AgentID, time.Now(), "h", "v", nil); err != nil {
		t.Fatal(err)
	}

	// A failed backup and a failed maintenance cycle name what failed.
	id, err := c.Catalog.StartRun(ctx, job.ID, "backup", "schedule", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Catalog.FinishRun(ctx, catalog.Run{ID: id, Status: catalog.RunFailed, Error: "boom", StartedAt: time.Now(), FinishedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	c.saveMaintenance(ctx, job.RepositoryID, RepoMaintenance{LastRun: time.Now(), Status: "failed", Error: "x"})
	page = get(t, client, srv.URL+"/", "failed")
	for _, want := range []string{"1 failed", "docs failed", "maintenance failed on local", "3 things need attention"} {
		if !strings.Contains(page, want) {
			t.Errorf("missing %q:\n%s", want, page)
		}
	}
}

func TestStatusPageWithNothingSetUp(t *testing.T) {
	c := newController(t)
	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	client := authedClient(t, c, srv)
	page := get(t, client, srv.URL+"/", "all good")
	if strings.Count(page, "none yet") != 3 || !strings.Contains(page, "nothing scheduled") {
		t.Errorf("an empty system:\n%s", page)
	}
}

// A failed restore must show where the operator looks: the Jobs list and the
// Status page, not only the job's own Runs table. The job's last backup is fine.
func TestFailedRestoreShowsOnStatusUntilAcknowledged(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)
	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	client := authedClient(t, c, srv)

	if run, err := c.RunJob(ctx, job.ID, "manual"); err != nil || run.Status != catalog.RunSuccess {
		t.Fatalf("backup: %+v %v", run, err)
	}
	snaps, _ := c.Snapshots(ctx, job.ID)
	target := filepath.Join(t.TempDir(), "restored")
	if rr, err := c.Restore(ctx, job.ID, "", snaps[0].ID, "docs", target, false); err != nil || !restoreOK(rr) {
		t.Fatalf("restore: %+v %v", rr, err)
	}
	page := get(t, client, srv.URL+"/", "all good")
	if strings.Contains(page, "Acknowledge") {
		t.Errorf("a successful restore is reported as a problem:\n%s", page)
	}
	// The Jobs list is about backups only.
	if jobs := get(t, client, srv.URL+"/jobs", "docs"); strings.Contains(jobs, "restore <span") {
		t.Errorf("the Jobs list shows a restore:\n%s", jobs)
	}

	// Into a non-empty target without overwrite: the restore fails.
	if rr, _ := c.Restore(ctx, job.ID, "", snaps[0].ID, "docs", target, false); rr.Status != catalog.RunFailed {
		t.Fatalf("expected a failed restore: %+v", rr)
	}
	page = get(t, client, srv.URL+"/", "Jobs")
	for _, want := range []string{"last backups all ok", "restore of docs failed", "Acknowledge"} {
		if !strings.Contains(page, want) {
			t.Errorf("the status page is missing %q:\n%s", want, page)
		}
	}
	failed, err := c.Catalog.LastRun(ctx, job.ID, "restore")
	if err != nil {
		t.Fatal(err)
	}
	jobPage := get(t, client, srv.URL+"/jobs/"+job.ID, "Runs")
	if !strings.Contains(jobPage, "/runs/"+strconv.FormatInt(failed.ID, 10)+"/ack") {
		t.Errorf("the job page offers no Acknowledge for the failed restore:\n%s", jobPage)
	}

	// Acknowledging removes it from Status and keeps the run.
	post(t, client, srv.URL+"/jobs/"+job.ID+"/runs/"+strconv.FormatInt(failed.ID, 10)+"/ack", url.Values{"back": {"/"}})
	page = get(t, client, srv.URL+"/", "Jobs")
	if strings.Contains(page, "restore of docs failed") || strings.Contains(page, "Acknowledge") {
		t.Errorf("an acknowledged restore still counts:\n%s", page)
	}
	if got, _ := c.Catalog.LastRun(ctx, job.ID, "restore"); got.AckedAt.IsZero() || got.Status != catalog.RunFailed {
		t.Errorf("the run after acknowledging: %+v", got)
	}
	if err := c.Catalog.AckRun(ctx, job.ID, failed.ID, "x", time.Now()); !errors.Is(err, catalog.ErrNotFound) {
		t.Errorf("acknowledging twice: %v", err)
	}
	backup, _ := c.Catalog.LastRun(ctx, job.ID, "backup")
	if err := c.Catalog.AckRun(ctx, job.ID, backup.ID, "x", time.Now()); !errors.Is(err, catalog.ErrNotFound) {
		t.Errorf("a backup must not be acknowledgeable: %v", err)
	}

	// The next failure counts again, and a later success clears it unacknowledged.
	if rr, _ := c.Restore(ctx, job.ID, "", snaps[0].ID, "docs", target, false); rr.Status != catalog.RunFailed {
		t.Fatalf("expected a second failed restore: %+v", rr)
	}
	if page = get(t, client, srv.URL+"/", "Jobs"); !strings.Contains(page, "restore of docs failed") {
		t.Errorf("a new failure must show again:\n%s", page)
	}
	other := filepath.Join(t.TempDir(), "again")
	if rr, err := c.Restore(ctx, job.ID, "", snaps[0].ID, "docs", other, false); err != nil || !restoreOK(rr) {
		t.Fatalf("second restore: %+v %v", rr, err)
	}
	if page = get(t, client, srv.URL+"/", "Jobs"); strings.Contains(page, "Acknowledge") {
		t.Errorf("a later success must clear the problem:\n%s", page)
	}
}
