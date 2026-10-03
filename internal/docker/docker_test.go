package docker_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/voidgrid/voidgrid-backup/internal/docker"
	"github.com/voidgrid/voidgrid-backup/internal/docker/dockertest"
)

func TestExecStreamsAndReportsExit(t *testing.T) {
	ctx := context.Background()
	fake := dockertest.New(t, dockertest.ComposeContainer("db", "app", "/srv/app", "db", "postgres:16", "running"))
	fake.Exec = func(_ string, cmd []string, stdin io.Reader, stdout, stderr io.Writer) int {
		switch cmd[0] {
		case "cat":
			io.Copy(stdout, stdin)
			return 0
		case "big":
			chunk := strings.Repeat("x", 64<<10)
			for i := 0; i < 32; i++ { // 2 MiB across many frames
				io.WriteString(stdout, chunk)
			}
			return 0
		default:
			io.WriteString(stdout, "partial")
			io.WriteString(stderr, "boom: permission denied")
			return 3
		}
	}
	c := docker.New(fake.Socket)

	rc, err := c.Exec(ctx, "db", []string{"cat"}, nil, strings.NewReader("round trip through stdin"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || string(out) != "round trip through stdin" {
		t.Fatalf("cat: %q %v", out, err)
	}

	rc, _ = c.Exec(ctx, "db", []string{"big"}, nil, nil)
	out, err = io.ReadAll(rc)
	rc.Close()
	if err != nil || len(out) != 2<<20 {
		t.Fatalf("big: %d bytes, %v", len(out), err)
	}

	rc, _ = c.Exec(ctx, "db", []string{"fail"}, nil, nil)
	out, err = io.ReadAll(rc)
	rc.Close()
	if err == nil || !strings.Contains(err.Error(), "exited with 3") || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("failing command: %q %v", out, err)
	}

	if err := c.Pause(ctx, "db"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, "db", []string{"cat"}, nil, nil); err == nil {
		t.Fatal("exec into a paused container succeeded")
	}
	if err := c.Unpause(ctx, "db"); err != nil {
		t.Fatal(err)
	}
	if err := c.Stop(ctx, "db"); err != nil || fake.State("db") != "exited" {
		t.Fatalf("stop: %v %s", err, fake.State("db"))
	}
	if err := c.Stop(ctx, "db"); err != nil {
		t.Fatalf("stopping a stopped container: %v", err)
	}
	if err := c.Start(ctx, "db"); err != nil || fake.State("db") != "running" {
		t.Fatalf("start: %v %s", err, fake.State("db"))
	}
	if err := c.Pause(ctx, "missing"); err == nil {
		t.Fatal("pausing a missing container succeeded")
	}
}

func TestStacks(t *testing.T) {
	data := t.TempDir()
	os.WriteFile(filepath.Join(data, "app.db"), []byte("SQLite format 3\x00rest"), 0o644)
	os.WriteFile(filepath.Join(data, "fake.db"), []byte("not a database at all"), 0o644)
	fake := dockertest.New(t,
		dockertest.ComposeContainer("a1", "notes", "/srv/notes", "app", "ghcr.io/x/notes:latest", "running", dockertest.Bind(data, "/data")),
		dockertest.ComposeContainer("d1", "notes", "/srv/notes", "db", "docker.io/library/postgres:16-alpine", "running"),
		dockertest.ComposeContainer("r1", "notes", "/srv/notes", "cache", "valkey/valkey:9", "running"),
		dockertest.ComposeContainer("m1", "wiki", "/srv/wiki", "db", "lscr.io/linuxserver/mariadb:11.4", "exited"),
		docker.Container{ID: "loose", Names: []string{"/loose"}, Image: "alpine", State: "running"},
	)
	stacks, err := docker.New(fake.Socket).Stacks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(stacks) != 2 || stacks[0].Project != "notes" || stacks[1].Project != "wiki" {
		t.Fatalf("stacks: %+v", stacks)
	}
	notes := stacks[0]
	if notes.WorkingDir != "/srv/notes" || len(notes.Services) != 3 {
		t.Fatalf("notes: %+v", notes)
	}
	kinds := map[string]string{}
	for _, s := range notes.Services {
		kinds[s.Name] = s.DumpKind
	}
	if kinds["db"] != docker.DumpPostgres || kinds["cache"] != docker.DumpRedis || kinds["app"] != "" {
		t.Fatalf("dump detection: %v", kinds)
	}
	app := notes.Services[0]
	if app.Name != "app" || len(app.SQLiteFiles) != 1 || filepath.Base(app.SQLiteFiles[0]) != "app.db" {
		t.Fatalf("sqlite detection: %+v", app)
	}
	if stacks[1].Services[0].DumpKind != docker.DumpMariaDB {
		t.Fatalf("mariadb detection: %+v", stacks[1].Services[0])
	}
}
