package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/voidgrid/voidgrid-backup/internal/agent"
	"github.com/voidgrid/voidgrid-backup/internal/catalog"
	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
	"github.com/voidgrid/voidgrid-backup/internal/repocfg"
)

// startStoppableAgent is startAgent with a way to take the agent down.
func startStoppableAgent(t *testing.T, dir string) (*agent.Agent, string, func()) {
	t.Helper()
	a, err := agent.New(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := grpc.NewServer(grpc.Creds(credentials.NewTLS(a.TLSConfig())), grpc.UnaryInterceptor(a.Interceptor))
	agentpb.RegisterAgentServer(s, a)
	go s.Serve(lis)
	t.Cleanup(s.Stop)
	return a, lis.Addr().String(), s.Stop
}

func waitSynced(t *testing.T, c *Controller, jobID string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := c.SnapshotsSynced(context.Background(), jobID); ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("snapshot list was never reconciled")
}

func ids(snaps []Snapshot) []string {
	var out []string
	for _, s := range snaps {
		out = append(out, s.ID)
	}
	return out
}

func TestSnapshotIndex(t *testing.T) {
	ctx := context.Background()
	a, addr, stop := startStoppableAgent(t, t.TempDir())
	c := newController(t)
	ag, err := enrollTestAgent(t, c, a, "box", addr)
	if err != nil {
		t.Fatal(err)
	}
	repo, _, err := c.AddRepository(ctx, "local", repocfg.Config{Kind: repocfg.KindFilesystem,
		Filesystem: &repocfg.Filesystem{Path: filepath.Join(t.TempDir(), "repo")}}, "pw", ag.ID)
	if err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("one"), 0o644)
	job, err := c.AddJob(ctx, JobInput{Name: "docs", AgentID: ag.ID, RepositoryID: repo.ID,
		Paths: []string{src}, Keep: catalog.Retention{Latest: 1}})
	if err != nil {
		t.Fatal(err)
	}

	// Nothing recorded and never reconciled: the page may not claim a list.
	if got, _ := c.Snapshots(ctx, job.ID); len(got) != 0 {
		t.Fatalf("snapshots before any backup: %+v", got)
	}
	if _, ok := c.SnapshotsSynced(ctx, job.ID); ok {
		t.Fatal("a never-reconciled job reports as synced")
	}

	run, err := c.RunJob(ctx, job.ID, "manual")
	if err != nil || run.Status != catalog.RunSuccess {
		t.Fatalf("backup: %+v %v", run, err)
	}
	waitSynced(t, c, job.ID) // the first backup of such a job reconciles in the background
	first, _ := c.Snapshots(ctx, job.ID)
	if len(first) != 1 || first[0].Files != 1 || first[0].Path != src || first[0].Start.IsZero() {
		t.Fatalf("recorded snapshot: %+v", first)
	}

	// Retention keeps one: the pruned snapshot leaves the record without a reconcile.
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("two!"), 0o644)
	if run, err := c.RunJob(ctx, job.ID, "manual"); err != nil || run.Status != catalog.RunSuccess {
		t.Fatalf("second backup: %+v %v", run, err)
	}
	second, _ := c.Snapshots(ctx, job.ID)
	live, err := c.listRemoteSnapshots(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || second[0].ID == first[0].ID || fmt.Sprint(ids(second)) != fmt.Sprint(ids(live)) {
		t.Fatalf("record %v does not match the repository %v after pruning", ids(second), ids(live))
	}

	// The record is served without the agent or the storage.
	stop()
	if got, err := c.Snapshots(ctx, job.ID); err != nil || len(got) != 1 {
		t.Fatalf("snapshots with the agent down: %+v %v", got, err)
	}
	if err := c.RefreshSnapshots(ctx, job.ID); err == nil {
		t.Fatal("refresh with the agent down should fail")
	}
	if got, _ := c.Snapshots(ctx, job.ID); len(got) != 1 {
		t.Fatalf("a failed refresh changed the record: %+v", got)
	}
}

func TestRefreshRepairsDrift(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)
	if _, err := c.RunJob(ctx, job.ID, "manual"); err != nil {
		t.Fatal(err)
	}
	waitSynced(t, c, job.ID)
	want, _ := c.Snapshots(ctx, job.ID)
	if len(want) != 1 {
		t.Fatalf("setup: %+v", want)
	}
	// Something outside this server changed the repository, or the record was lost.
	if err := c.Catalog.ReplaceSnapshots(ctx, job.ID, []Snapshot{{ID: "gone", Path: "/x", Start: time.Now(), End: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	if err := c.RefreshSnapshots(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := c.Snapshots(ctx, job.ID)
	if fmt.Sprint(ids(got)) != fmt.Sprint(ids(want)) {
		t.Fatalf("after refresh: %v, want %v", ids(got), ids(want))
	}
}

// An agent that predates the start/end fields reports start_unix 0: the
// server can't record it, so it reconciles.
func TestOldAgentResultReconciles(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)
	if _, err := c.RunJob(ctx, job.ID, "manual"); err != nil {
		t.Fatal(err)
	}
	waitSynced(t, c, job.ID)
	c.Catalog.SetSetting(ctx, snapshotsSyncedKey(job.ID), "") // pretend nothing was reconciled yet
	c.Catalog.ReplaceSnapshots(ctx, job.ID, nil)
	c.recordSnapshots(ctx, job, []*agentpb.PathResult{{Path: job.Paths[0], SnapshotId: "whatever"}})
	waitSynced(t, c, job.ID)
	if got, _ := c.Snapshots(ctx, job.ID); len(got) != 1 {
		t.Fatalf("reconcile did not restore the list: %+v", got)
	}
}

func TestJobPageWithSnapshotIndex(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)
	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	client := authedClient(t, c, srv)

	// Never reconciled: the page opens at once and starts a reconcile.
	get(t, client, srv.URL+"/jobs/"+job.ID, "Refresh from repository")
	waitSynced(t, c, job.ID)
	get(t, client, srv.URL+"/jobs/"+job.ID, "checked against the repository")
	post(t, client, srv.URL+"/jobs/"+job.ID+"/snapshots/refresh", nil)

	if _, err := c.RunJob(ctx, job.ID, "manual"); err != nil {
		t.Fatal(err)
	}
	snaps, _ := c.Snapshots(ctx, job.ID)
	if len(snaps) != 1 {
		t.Fatalf("snapshots: %+v", snaps)
	}
	get(t, client, srv.URL+"/jobs/"+job.ID, snaps[0].ID[:8])
}

func TestBrowseCache(t *testing.T) {
	var b browseCache
	k := browseKey{"r", "s", "dir"}
	var calls atomic.Int32
	fetch := func() ([]*agentpb.DirEntry, error) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		return []*agentpb.DirEntry{{Name: "f"}}, nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ { // concurrent identical requests share one call
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e, err := b.do(k, fetch); err != nil || len(e) != 1 {
				t.Errorf("do: %v %v", e, err)
			}
		}()
	}
	wg.Wait()
	b.do(k, fetch) // and a later one is served from the cache
	if calls.Load() != 1 {
		t.Fatalf("fetch ran %d times, want 1", calls.Load())
	}

	// Failures are not cached.
	bad := browseKey{"r", "s", "bad"}
	boom := errors.New("boom")
	for i := 0; i < 2; i++ {
		if _, err := b.do(bad, func() ([]*agentpb.DirEntry, error) { calls.Add(1); return nil, boom }); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("a failure was cached: %d calls", calls.Load())
	}

	// Size is bounded and the oldest entries go first.
	for i := 0; i < browseCacheEntries+5; i++ {
		b.do(browseKey{"r", "s", fmt.Sprint(i)}, fetch)
	}
	if n := len(b.data); n != browseCacheEntries {
		t.Fatalf("cache holds %d listings, want %d", n, browseCacheEntries)
	}
	before := calls.Load()
	b.do(browseKey{"r", "s", "0"}, fetch) // evicted, so fetched again
	if calls.Load() != before+1 {
		t.Fatal("evicted entry was served from the cache")
	}
}

func TestRepoGate(t *testing.T) {
	var g repoGate
	ctx := context.Background()
	var rel []func()
	for i := 0; i < repoConcurrency; i++ {
		r, err := g.acquire(ctx, "repo")
		if err != nil {
			t.Fatal(err)
		}
		rel = append(rel, r)
	}
	// Another repository has its own slots.
	if r, err := g.acquire(ctx, "other"); err != nil {
		t.Fatal(err)
	} else {
		r()
	}
	// Full: a waiter is held until its context ends...
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := g.acquire(short, "repo"); err == nil {
		t.Fatal("acquired a slot beyond the limit")
	}
	// ...or a slot is released.
	got := make(chan struct{})
	go func() {
		r, err := g.acquire(ctx, "repo")
		if err == nil {
			r()
		}
		close(got)
	}()
	rel[0]()
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("waiter was not released")
	}
	rel[1]()
}

// Pages that change by themselves mark the parts to swap with <div id="live">.
// This pins which pages are live when, that every listed region exists on the
// page, and that nothing falls back to reloading the whole page.
func TestLiveRegions(t *testing.T) {
	c, job, _ := setupJob(t)
	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	client := authedClient(t, c, srv)
	liveRE := regexp.MustCompile(`id="live" data-ids="([^"]+)"`)

	check := func(path string, wantLive bool) string {
		t.Helper()
		page := get(t, client, srv.URL+path, "")
		m := liveRE.FindStringSubmatch(page)
		if (m != nil) != wantLive {
			t.Errorf("%s: live region present = %v, want %v", path, m != nil, wantLive)
		}
		if m != nil {
			for _, id := range strings.Split(m[1], ",") {
				if !strings.Contains(page, `id="`+id+`"`) {
					t.Errorf("%s: live region lists %q but the page has no such element", path, id)
				}
			}
		}
		if strings.Contains(page, "location.reload") {
			t.Errorf("%s reloads the whole page", path)
		}
		return page
	}

	// Lists that go stale as the server polls agents and jobs run.
	check("/", true)
	check("/jobs", true)

	// The log page only refreshes when asked to.
	check("/logs", false)
	check("/logs?auto=1", true)

	// A repository being measured keeps its table current until it is done.
	check("/repositories", false)
	if !c.inflight.start(repoMeasureKey(job.RepositoryID)) {
		t.Fatal("could not mark the repository as being measured")
	}
	if page := check("/repositories", true); !strings.Contains(page, "measuring") {
		t.Error("a repository being measured doesn't say so")
	}
	c.inflight.done(repoMeasureKey(job.RepositoryID))
	check("/repositories", false)

	// A job page is live while something runs on the job, and not otherwise.
	get(t, client, srv.URL+"/jobs/"+job.ID, "Back up now") // a never-synced job refreshes its list on first view
	waitSynced(t, c, job.ID)
	for i := 0; i < 200 && c.IsRunning(snapshotRefreshKey(job.ID)); i++ { // until the refresh has let go
		time.Sleep(10 * time.Millisecond)
	}
	check("/jobs/"+job.ID, false)
	if !c.inflight.start(job.ID) {
		t.Fatal("could not mark the job running")
	}
	if page := check("/jobs/"+job.ID, true); !strings.Contains(page, "running") {
		t.Error("a running job's page doesn't say so")
	}
	c.inflight.done(job.ID)
	check("/jobs/"+job.ID, false)
}
