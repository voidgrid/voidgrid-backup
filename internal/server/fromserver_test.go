package server

import (
	"context"
	"database/sql"
	"errors"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
	"github.com/voidgrid/voidgrid-backup/internal/engine"
	"github.com/voidgrid/voidgrid-backup/internal/repocfg"
	"github.com/voidgrid/voidgrid-backup/internal/sftptest"
	"github.com/voidgrid/voidgrid-backup/internal/tidy"
)

func withServerEngine(t *testing.T, c *Controller) {
	t.Helper()
	eng, err := engine.New(t.TempDir(), EngineHost)
	if err != nil {
		t.Fatal(err)
	}
	c.Engine = eng
}

func TestCreateARepositoryFromTheServer(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)
	dir := filepath.Join(t.TempDir(), "repo")
	cfg := repocfg.Config{Kind: repocfg.KindFilesystem, Filesystem: &repocfg.Filesystem{Path: dir, ServerPath: dir}}

	// Without an engine, or without a path the server can use, nothing is created.
	if _, _, err := c.AddRepository(ctx, "srv", cfg, "", ServerHost); err == nil {
		t.Fatal("a repository was created from a server with no engine")
	}
	withServerEngine(t, c)
	bare := repocfg.Config{Kind: repocfg.KindFilesystem, Filesystem: &repocfg.Filesystem{Path: dir}}
	if _, _, err := c.AddRepository(ctx, "srv", bare, "", ServerHost); err == nil || !strings.Contains(err.Error(), "path on the server") {
		t.Fatalf("a filesystem repository with no server path: %v", err)
	}
	if _, err := c.Catalog.RepositoryByName(ctx, "srv"); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("a refused repository left a record: %v", err)
	}

	repo, generated, err := c.AddRepository(ctx, "srv", cfg, "", ServerHost)
	if err != nil || generated == "" || repo.InitializedAt.IsZero() {
		t.Fatalf("create from the server: %+v %q %v", repo, generated, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) == 0 {
		t.Fatal("nothing was written at the server's path")
	}
	if got := c.MaintainedBy(ctx, repo.ID); got != MaintByServer {
		t.Fatalf("maintained by %q, want the server (it created the repository)", got)
	}
	if ids, _ := c.Catalog.RepositoryAgents(ctx, repo.ID); len(ids) != 0 {
		t.Fatalf("agents assigned to a repository only the server opened: %v", ids)
	}

	// No agent counts as assigned until one is tested.
	if repos, _ := c.ReposFor(ctx, job.AgentID); len(repos) != 1 || repos[0].ID != job.RepositoryID {
		t.Fatalf("the agent is offered %d repositories, want only its own", len(repos))
	}
	in := JobInput{Name: "fromsrv", AgentID: job.AgentID, RepositoryID: repo.ID, Paths: []string{t.TempDir()},
		Schedule: "0 3 * * *", Keep: catalog.DefaultRetention, Enabled: true}
	if _, err := c.AddJob(ctx, in); !errors.Is(err, ErrNotAssigned) {
		t.Fatalf("a job on an unassigned server-created repository: %v", err)
	}
	if _, err := c.AssignAgent(ctx, repo.ID, job.AgentID, ""); err != nil {
		t.Fatalf("assign: %v", err)
	}
	j, err := c.AddJob(ctx, in)
	if err != nil {
		t.Fatalf("job after assigning: %v", err)
	}
	if run, err := c.RunJob(ctx, j.ID, "manual"); err != nil || run.Status == catalog.RunFailed {
		t.Fatalf("backup into it: %+v %v", run, err)
	}
	if snaps, _ := c.Snapshots(ctx, j.ID); len(snaps) != 1 {
		t.Fatalf("snapshots: %d", len(snaps))
	}
}

func TestCreateFromTheServerThroughTheForm(t *testing.T) {
	ctx := context.Background()
	c, _, _ := setupJob(t)
	withServerEngine(t, c)
	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	client := authedClient(t, c, srv)
	page := get(t, client, srv.URL+"/repositories", "Add a repository")
	if !strings.Contains(page, `<option value="server" `) {
		t.Fatalf("the add form does not offer the server:\n%s", page)
	}
	dir := filepath.Join(t.TempDir(), "viaform")
	post(t, client, srv.URL+"/repositories", url.Values{"name": {"formsrv"}, "kind": {"filesystem"}, "agent": {ServerHost},
		"password": {"pw"}, "fs_path": {dir}, "fs_server_path": {dir}})
	repo, err := c.Catalog.RepositoryByName(ctx, "formsrv")
	if err != nil || repo.InitializedAt.IsZero() {
		t.Fatalf("the form did not create it: %+v %v", repo, err)
	}
}

func TestEditAnSFTPRepositoryThroughTheServer(t *testing.T) {
	ctx := context.Background()
	c, _, _ := setupJob(t)
	withServerEngine(t, c)
	port, keyFile := sftptest.Start(t)
	base := t.TempDir()
	oldPath, newPath := filepath.Join(base, "old"), filepath.Join(base, "hosts", "box", "new")
	cfg := repocfg.Config{Kind: repocfg.KindSFTP, SFTP: &repocfg.SFTP{Host: "127.0.0.1", Port: port, User: "u", Path: oldPath, KeyFile: keyFile}}

	repo, _, err := c.AddRepository(ctx, "box", cfg, "pw", ServerHost)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if repo.Config.SFTP.KnownHosts == "" {
		t.Fatal("the host key was not pinned")
	}
	er, err := serverRepo(repo)
	if err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "f.txt"), []byte("through the server"), 0o644); err != nil {
		t.Fatal(err)
	}
	keep := engine.Retention{Latest: 5}
	if res, err := c.Engine.Backup(ctx, er, []string{src}, nil, keep); err != nil || res[0].Err != nil {
		t.Fatalf("backup: %+v %v", res, err)
	}

	// No agent can do it, and the server only does it when asked.
	if _, err := c.EditRepository(ctx, repo.ID, RepoEdit{SFTPPath: newPath}); err == nil || !strings.Contains(err.Error(), "through the server") {
		t.Fatalf("an edit with no agent and no choice: %v", err)
	}
	if _, err := c.EditRepository(ctx, repo.ID, RepoEdit{SFTPPath: newPath, On: "elsewhere"}); err == nil {
		t.Fatal("an unknown place to edit from was accepted")
	}

	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	client := authedClient(t, c, srv)
	page := get(t, client, srv.URL+"/repositories/"+repo.ID+"/edit", "Open, test and move it through")
	if !strings.Contains(page, `<option value="server" selected>`) {
		t.Fatalf("the server is not preselected when no agent can do it:\n%s", page)
	}

	if _, err := c.EditRepository(ctx, repo.ID, RepoEdit{SFTPPath: newPath, On: ServerHost}); err != nil {
		t.Fatalf("edit through the server: %v", err)
	}
	got, _ := c.Catalog.Repository(ctx, repo.ID)
	if got.Config.SFTP.Path != newPath {
		t.Fatalf("saved path %q", got.Config.SFTP.Path)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("the old directory is still there: %v", err)
	}
	if entries, err := os.ReadDir(newPath); err != nil || len(entries) == 0 {
		t.Fatalf("nothing at the new path: %v", err)
	}
	er, _ = serverRepo(got)
	snaps, err := c.Engine.ListSnapshotsOf(ctx, er, EngineHost, nil)
	if err != nil || len(snaps) != 1 {
		t.Fatalf("snapshots after the move: %d %v", len(snaps), err)
	}
	res, err := c.Engine.Backup(ctx, er, []string{src}, nil, keep)
	if err != nil || res[0].Err != nil || res[0].CachedFiles != 1 {
		t.Fatalf("the backup after the move was not incremental: %+v %v", res, err)
	}
}

func TestEditAFilesystemRepositoryIsNotDoneThroughTheServer(t *testing.T) {
	ctx := context.Background()
	c, job, repo, _ := serverOnlyRepo(t)
	if _, err := c.EditRepository(ctx, job.RepositoryID, RepoEdit{FSPath: filepath.Join(t.TempDir(), "elsewhere"), On: ServerHost}); err == nil ||
		!strings.Contains(err.Error(), "through an agent") {
		t.Fatalf("a filesystem edit through the server: %v", err)
	}
	if got, _ := c.Catalog.Repository(ctx, repo.ID); got.Config.Filesystem.Path != repo.Config.Filesystem.Path {
		t.Fatal("a refused edit changed the saved path")
	}
}

func TestAddFormDefaultsThePathFromTheHostname(t *testing.T) {
	c, job, _ := setupJob(t)
	ag, err := c.Catalog.Agent(context.Background(), job.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Catalog.RecordSeen(context.Background(), ag.ID, time.Now(), "harbinger", "v", nil); err != nil {
		t.Fatal(err)
	}
	withServerEngine(t, c)
	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	client := authedClient(t, c, srv)
	page := get(t, client, srv.URL+"/repositories", "Add a repository")
	for _, want := range []string{`data-host="harbinger"`, `data-host="voidgrid-server"`, "first 6 characters of the agent ID", `agent.value.slice(0, 6)`} {
		if !strings.Contains(page, want) {
			t.Errorf("the add form is missing %q", want)
		}
	}
}

// When recording a repository fails after it was created with a generated
// password, the password comes back with the error: nothing else has it.
func TestGeneratedPasswordSurvivesAFailureAfterCreating(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "catalog.db")
	cat, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tidy.CloseWarn("catalog", cat) })
	eng, err := engine.New(filepath.Join(dir, "kopia"), EngineHost)
	if err != nil {
		t.Fatal(err)
	}
	c := &Controller{Catalog: cat, Engine: eng}
	// The write that follows a successful create fails, as on a full disk.
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER fail_update BEFORE UPDATE ON repositories BEGIN SELECT RAISE(FAIL, 'disk full'); END`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(dir, "store")
	cfg := repocfg.Config{Kind: repocfg.KindFilesystem, Filesystem: &repocfg.Filesystem{Path: store, ServerPath: store}}
	_, generated, err := c.AddRepository(ctx, "r", cfg, "", ServerHost)
	if err == nil || generated == "" {
		t.Fatalf("err=%v, generated password returned=%v", err, generated != "")
	}
	// It is the password of what is in storage.
	if _, err := eng.Inspect(ctx, engine.Repo{ID: "check", Config: repocfg.Config{Kind: repocfg.KindFilesystem, Filesystem: &repocfg.Filesystem{Path: store}}, Password: generated}); err != nil {
		t.Fatalf("the returned password does not open the repository: %v", err)
	}
}
