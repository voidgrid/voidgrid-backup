package engine

import (
	"context"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/voidgrid/voidgrid-backup/internal/docker"
	"github.com/voidgrid/voidgrid-backup/internal/docker/dockertest"
	"github.com/voidgrid/voidgrid-backup/internal/repocfg"
)

type stackFixture struct {
	e        *Engine
	r        Repo
	dk       *docker.Client
	fake     *dockertest.Fake
	workdir  string
	media    string
	imported *strings.Builder
	mu       *sync.Mutex
}

func newStackFixture(t *testing.T) *stackFixture {
	t.Helper()
	workdir := filepath.Join(t.TempDir(), "notes")
	writeFile(t, filepath.Join(workdir, "docker-compose.yaml"), "services: {}")
	writeFile(t, filepath.Join(workdir, ".env"), "TZ=America/Chicago")
	writeFile(t, filepath.Join(workdir, "config", "app.yaml"), "port: 80")
	writeFile(t, filepath.Join(workdir, "pgdata", "PG_VERSION"), "16") // raw DB files: excluded, dumped instead
	media := filepath.Join(t.TempDir(), "media")
	writeFile(t, filepath.Join(media, "photo.jpg"), "jpeg")

	f := &stackFixture{workdir: workdir, media: media, imported: &strings.Builder{}, mu: &sync.Mutex{}}
	f.fake = dockertest.New(t,
		dockertest.ComposeContainer("app1", "notes", workdir, "app", "ghcr.io/x/notes:1", "running",
			dockertest.Bind(filepath.Join(workdir, "config"), "/config"), dockertest.Bind(media, "/media")),
		dockertest.ComposeContainer("db1", "notes", workdir, "db", "postgres:16", "running",
			dockertest.Bind(filepath.Join(workdir, "pgdata"), "/var/lib/postgresql/data")),
		dockertest.ComposeContainer("self", "notes", workdir, "backup-agent", "github.com/voidgrid/voidgrid-backup:latest", "running"),
	)
	f.fake.Exec = func(container string, cmd []string, stdin io.Reader, stdout, stderr io.Writer) int {
		script := strings.Join(cmd, " ")
		switch {
		case strings.Contains(script, "pg_dumpall"):
			io.WriteString(stdout, "-- PostgreSQL database cluster dump\nCREATE ROLE app;\n")
			return 0
		case strings.Contains(script, "psql"):
			b, _ := io.ReadAll(stdin)
			f.mu.Lock()
			f.imported.Write(b)
			f.mu.Unlock()
			io.WriteString(stdout, "CREATE ROLE")
			return 0
		}
		io.WriteString(stderr, "unexpected command")
		return 127
	}
	f.dk = docker.New(f.fake.Socket)
	f.e = newEngine(t, "host-a")
	f.r = Repo{ID: "r", Password: "pw", Config: repocfg.Config{
		Kind: repocfg.KindFilesystem, Filesystem: &repocfg.Filesystem{Path: filepath.Join(t.TempDir(), "repo")}}}
	if _, _, err := f.e.Init(context.Background(), f.r); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *stackFixture) spec() StackSpec {
	return StackSpec{
		Project:    "notes",
		WorkingDir: f.workdir,
		Include:    []string{filepath.Join(f.workdir, "config"), f.media},
		Exclude:    []string{filepath.Join(f.workdir, "pgdata")},
		Dumps:      map[string]string{"db": docker.DumpPostgres},
		Quiesce:    QuiescePause,
	}
}

func names(entries []DirEntry) []string {
	var out []string
	for _, e := range entries {
		out = append(out, e.Name)
	}
	return out
}

func TestStackBackupRestoreImport(t *testing.T) {
	ctx := context.Background()
	f := newStackFixture(t)

	res, err := f.e.BackupStack(ctx, f.r, f.dk, f.spec(), keepOne)
	if err != nil || res.Err != nil || len(res.Warnings) != 0 || res.Errors != 0 {
		t.Fatalf("stack backup: %+v %v", res, err)
	}
	if res.Path != "/stacks/notes" {
		t.Fatalf("source path %q", res.Path)
	}
	// Only the app was paused (not the dumped db, not the agent itself), and it was resumed.
	if calls := f.fake.Calls(); !slices.Equal(calls, []string{"pause app1", "unpause app1"}) {
		t.Fatalf("quiesce calls: %v", calls)
	}

	root, _ := f.e.ListDirectory(ctx, f.r, res.SnapshotID, "")
	if got := names(root); !slices.Equal(got, []string{"dumps", "mounts", "stack", "voidgrid-backup.json"}) {
		t.Fatalf("snapshot root: %v", got)
	}
	stack, _ := f.e.ListDirectory(ctx, f.r, res.SnapshotID, "stack")
	if got := names(stack); !slices.Equal(got, []string{"config", ".env", "docker-compose.yaml"}) {
		t.Fatalf("stack dir (pgdata must be excluded): %v", got)
	}
	mounts, _ := f.e.ListDirectory(ctx, f.r, res.SnapshotID, "mounts")
	if len(mounts) != 1 || mounts[0].Name != mountName(f.media) {
		t.Fatalf("mounts (config is inside the stack dir, so only media): %+v", mounts)
	}

	// Restore to an alternate root: the whole tree, no containers touched.
	alt := filepath.Join(t.TempDir(), "alt")
	if _, err := f.e.RestoreStack(ctx, f.r, f.dk, res.SnapshotID, alt, true); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(alt, "dumps", "db.sql")); !strings.Contains(got, "CREATE ROLE app") {
		t.Fatalf("restored dump: %q", got)
	}
	if got := readFile(t, filepath.Join(alt, "mounts", mountName(f.media), "photo.jpg")); got != "jpeg" {
		t.Fatalf("restored mount: %q", got)
	}
	if n := len(f.fake.Calls()); n != 2 {
		t.Fatalf("alternate-root restore touched containers: %v", f.fake.Calls())
	}

	// Restore in place: files come back, stack stopped then started.
	writeFile(t, filepath.Join(f.workdir, "config", "app.yaml"), "broken")
	writeFile(t, filepath.Join(f.media, "photo.jpg"), "corrupt")
	if _, err := f.e.RestoreStack(ctx, f.r, f.dk, res.SnapshotID, "", true); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(f.workdir, "config", "app.yaml")); got != "port: 80" {
		t.Fatalf("in-place config: %q", got)
	}
	if got := readFile(t, filepath.Join(f.media, "photo.jpg")); got != "jpeg" {
		t.Fatalf("in-place mount: %q", got)
	}
	if got := readFile(t, filepath.Join(f.workdir, dumpsRestoreDir, "db.sql")); !strings.Contains(got, "CREATE ROLE") {
		t.Fatalf("in-place dump copy: %q", got)
	}
	calls := f.fake.Calls()[2:]
	if !slices.Equal(calls, []string{"stop app1", "stop db1", "start db1", "start app1"}) {
		t.Fatalf("in-place restore calls: %v", calls)
	}
	if f.fake.State("self") != "running" {
		t.Fatal("the agent stopped itself")
	}

	// Import the dump into the running db.
	out, err := f.e.ImportDump(ctx, f.r, f.dk, res.SnapshotID, "db")
	if err != nil || out != "CREATE ROLE" {
		t.Fatalf("import: %q %v", out, err)
	}
	if !strings.Contains(f.imported.String(), "CREATE ROLE app;") {
		t.Fatalf("import stdin: %q", f.imported.String())
	}
	if _, err := f.e.ImportDump(ctx, f.r, f.dk, res.SnapshotID, "app"); err == nil {
		t.Fatal("imported a dump that does not exist")
	}
}

func TestStackDumpFailureIsReported(t *testing.T) {
	ctx := context.Background()
	f := newStackFixture(t)
	f.fake.Exec = func(_ string, _ []string, _ io.Reader, stdout, stderr io.Writer) int {
		io.WriteString(stdout, "-- truncated")
		io.WriteString(stderr, "pg_dumpall: error: connection failed")
		return 1
	}
	res, err := f.e.BackupStack(ctx, f.r, f.dk, f.spec(), keepOne)
	if err != nil || res.Err != nil {
		t.Fatalf("a failed dump must not fail the file backup: %+v %v", res, err)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "connection failed") {
		t.Fatalf("warnings: %q", res.Warnings)
	}
	dumps, _ := f.e.ListDirectory(ctx, f.r, res.SnapshotID, "dumps")
	for _, d := range dumps {
		if d.Name == "db.sql" && d.Size > 0 {
			t.Fatalf("a truncated dump was stored: %+v", d)
		}
	}
	if calls := f.fake.Calls(); !slices.Equal(calls, []string{"pause app1", "unpause app1"}) {
		t.Fatalf("containers not resumed after a failed dump: %v", calls)
	}
}

func TestStackStoppedDBIsSkipped(t *testing.T) {
	ctx := context.Background()
	f := newStackFixture(t)
	if err := f.dk.Stop(ctx, "db1"); err != nil {
		t.Fatal(err)
	}
	res, err := f.e.BackupStack(ctx, f.r, f.dk, f.spec(), keepOne)
	if err != nil || res.Err != nil || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "not running") {
		t.Fatalf("%+v %v", res, err)
	}
}
