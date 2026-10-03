package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
	"github.com/voidgrid/voidgrid-backup/internal/docker/dockertest"
	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
	"github.com/voidgrid/voidgrid-backup/internal/repocfg"
)

func TestUpdateJob(t *testing.T) {
	ctx := context.Background()
	c, job, src := setupJob(t)
	other, err := c.AddJob(ctx, JobInput{Name: "other", AgentID: job.AgentID, RepositoryID: job.RepositoryID,
		Paths: []string{src}, Keep: catalog.DefaultRetention})
	if err != nil {
		t.Fatal(err)
	}
	newDir := t.TempDir()
	base := JobInput{Name: "docs", Paths: []string{src, newDir}, Excludes: []string{"*.log"},
		Schedule: "0 4 * * *", Keep: catalog.Retention{Latest: 2, Daily: 5}, Enabled: false}

	got, err := c.UpdateJob(ctx, job.ID, base)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := c.Catalog.Job(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Paths) != 2 || saved.Paths[1] != newDir || saved.Schedule != "0 4 * * *" ||
		saved.Keep.Latest != 2 || saved.Keep.Daily != 5 || saved.Enabled || len(saved.Excludes) != 1 {
		t.Fatalf("not saved: %+v", saved)
	}
	// Identity is untouched by an edit.
	if saved.ID != job.ID || saved.Kind != job.Kind || saved.AgentID != job.AgentID ||
		saved.RepositoryID != job.RepositoryID || !saved.CreatedAt.Equal(job.CreatedAt) || got.ID != job.ID {
		t.Fatalf("identity changed: before %+v after %+v", job, saved)
	}

	for name, mutate := range map[string]func(*JobInput){
		"duplicate name":    func(in *JobInput) { in.Name = other.Name },
		"empty name":        func(in *JobInput) { in.Name = " " },
		"root path":         func(in *JobInput) { in.Paths = []string{"/"} },
		"no paths":          func(in *JobInput) { in.Paths = nil },
		"bad schedule":      func(in *JobInput) { in.Schedule = "whenever" },
		"keeps nothing":     func(in *JobInput) { in.Keep = catalog.Retention{} },
		"stack on path job": func(in *JobInput) { in.Paths = nil; in.Stack = &catalog.StackConfig{Quiesce: "none"} },
		"vm on path job":    func(in *JobInput) { in.Paths = nil; in.VM = &catalog.VMConfig{Disks: []string{"vda"}} },
		"stack and vm":      func(in *JobInput) { in.Stack = &catalog.StackConfig{}; in.VM = &catalog.VMConfig{} },
	} {
		in := base
		mutate(&in)
		if _, err := c.UpdateJob(ctx, job.ID, in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if after, _ := c.Catalog.Job(ctx, job.ID); len(after.Paths) != 2 || after.Name != "docs" {
		t.Errorf("a rejected edit changed the job: %+v", after)
	}

	// Keeping its own name is not a collision.
	if _, err := c.UpdateJob(ctx, job.ID, base); err != nil {
		t.Errorf("saving without renaming: %v", err)
	}
	if _, err := c.UpdateJob(ctx, "nope", base); !errors.Is(err, catalog.ErrNotFound) {
		t.Errorf("unknown job: %v", err)
	}

	// A running job can't be edited.
	if !c.inflight.start(job.ID) {
		t.Fatal("could not mark the job running")
	}
	if _, err := c.UpdateJob(ctx, job.ID, base); !errors.Is(err, ErrRunning) {
		t.Errorf("edit while running: %v", err)
	}
	c.inflight.done(job.ID)
}

// Adding a mount to a stack job through the edit form changes only what the
// next snapshot contains: the earlier snapshot is left as it was.
func TestEditStackJobAddsMount(t *testing.T) {
	ctx := context.Background()
	workdir := filepath.Join(t.TempDir(), "notes")
	os.MkdirAll(workdir, 0o755)
	os.WriteFile(filepath.Join(workdir, "docker-compose.yaml"), []byte("services: {}"), 0o644)
	extra := filepath.Join(t.TempDir(), "uploads")
	os.MkdirAll(extra, 0o755)
	os.WriteFile(filepath.Join(extra, "a.txt"), []byte("hello"), 0o644)

	fake := dockertest.New(t,
		dockertest.ComposeContainer("app1", "notes", workdir, "app", "notes:1", "running",
			dockertest.Bind(extra, "/uploads")),
	)
	a, addr := startAgentWithDocker(t, t.TempDir(), fake.Socket)
	c := newController(t)
	ag, err := c.Enroll(ctx, "box", addr, a.EnrollmentCode())
	if err != nil {
		t.Fatal(err)
	}
	repo, _, err := c.AddRepository(ctx, "local", repocfg.Config{Kind: repocfg.KindFilesystem,
		Filesystem: &repocfg.Filesystem{Path: filepath.Join(t.TempDir(), "repo")}}, "pw", ag.ID)
	if err != nil {
		t.Fatal(err)
	}
	job, err := c.AddJob(ctx, JobInput{Name: "notes", AgentID: ag.ID, RepositoryID: repo.ID,
		Stack:    &catalog.StackConfig{Project: "notes", WorkingDir: workdir, Exclude: []string{extra}, Quiesce: "none"},
		Keep:     catalog.Retention{Latest: 5},
		Schedule: ""})
	if err != nil {
		t.Fatal(err)
	}
	if run, err := c.RunJob(ctx, job.ID, "manual"); err != nil || run.Status != catalog.RunSuccess {
		t.Fatalf("first backup: %+v %v", run, err)
	}
	first, err := c.Snapshots(ctx, job.ID)
	if err != nil || len(first) != 1 {
		t.Fatalf("snapshots: %+v %v", first, err)
	}
	mountDir := "mounts/" + strings.ReplaceAll(strings.TrimPrefix(extra, "/"), "/", "__")
	if hasEntry(t, c, job.ID, first[0].ID, "mounts", mountDir[len("mounts/"):]) {
		t.Fatal("excluded mount present in the first snapshot")
	}

	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	client := authedClient(t, c, srv)
	page := get(t, client, srv.URL+"/jobs/"+job.ID+"/edit", "Mounts to back up")
	if !strings.Contains(page, extra) {
		t.Fatalf("edit page doesn't list the mount %s:\n%s", extra, page)
	}

	post(t, client, srv.URL+"/jobs/"+job.ID+"/edit", url.Values{
		"name": {"notes"}, "mount": {extra}, "include": {extra}, "quiesce": {"none"},
		"keep_latest": {"5"}, "keep_hourly": {"0"}, "keep_daily": {"0"}, "keep_weekly": {"0"}, "keep_monthly": {"0"}, "keep_annual": {"0"},
		// Identity fields in a crafted post are ignored.
		"project": {"hijack"}, "working_dir": {"/tmp"}, "agent": {"x"},
	})
	saved, err := c.Catalog.Job(ctx, job.ID)
	if err != nil || saved.Stack == nil || len(saved.Stack.Include) != 1 || saved.Stack.Include[0] != extra ||
		len(saved.Stack.Exclude) != 0 || saved.Stack.Project != "notes" || saved.Stack.WorkingDir != workdir {
		t.Fatalf("edit not applied cleanly: %+v %v", saved.Stack, err)
	}

	if run, err := c.RunJob(ctx, job.ID, "manual"); err != nil || run.Status != catalog.RunSuccess {
		t.Fatalf("second backup: %+v %v", run, err)
	}
	all, err := c.Snapshots(ctx, job.ID)
	if err != nil || len(all) != 2 {
		t.Fatalf("snapshots after edit: %+v %v", all, err)
	}
	var second string
	for _, s := range all {
		if s.ID != first[0].ID {
			second = s.ID
		}
	}
	if !hasEntry(t, c, job.ID, second, "mounts", mountDir[len("mounts/"):]) {
		t.Fatal("the new mount is missing from the next snapshot")
	}
	if hasEntry(t, c, job.ID, first[0].ID, "mounts", mountDir[len("mounts/"):]) {
		t.Fatal("the edit changed the earlier snapshot")
	}
}

// hasEntry reports whether dir in a snapshot lists name; a missing dir is "no".
func hasEntry(t *testing.T, c *Controller, jobID, snapID, dir, name string) bool {
	t.Helper()
	entries, err := c.Browse(context.Background(), jobID, snapID, dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.GetName() == name {
			return true
		}
	}
	return false
}

func TestBuildStackViewMarksMountsInsideComposeDir(t *testing.T) {
	st := &agentpb.Stack{Project: "p", WorkingDir: "/srv/p", Services: []*agentpb.Service{{
		Name: "app", Mounts: []*agentpb.Mount{
			{Type: "bind", Source: "/srv/p/data", Destination: "/data"},
			{Type: "bind", Source: "/srv/pdata", Destination: "/other"}, // shares a prefix, not inside
			{Type: "bind", Source: "/mnt/media", Destination: "/media"},
		}}}}
	got := map[string]bool{}
	for _, m := range buildStackView(st).Mounts {
		got[m.Source] = m.Inside
	}
	want := map[string]bool{"/srv/p/data": true, "/srv/pdata": false, "/mnt/media": false}
	for src, inside := range want {
		if got[src] != inside {
			t.Errorf("%s: Inside = %v, want %v", src, got[src], inside)
		}
	}
}
