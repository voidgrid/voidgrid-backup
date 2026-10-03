package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/voidgrid/voidgrid-backup/internal/repocfg"
)

func permsRepo(t *testing.T, e *Engine) Repo {
	t.Helper()
	r := Repo{ID: "r", Password: "pw", Config: repocfg.Config{
		Kind: repocfg.KindFilesystem, Filesystem: &repocfg.Filesystem{Path: filepath.Join(t.TempDir(), "repo")}}}
	if _, _, err := e.Init(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestUnreadableEntriesAreNamed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read everything")
	}
	ctx := context.Background()
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "ok.txt"), "fine")
	writeFile(t, filepath.Join(src, "secret", "key"), "x")
	os.Chmod(filepath.Join(src, "secret"), 0o000)
	t.Cleanup(func() { os.Chmod(filepath.Join(src, "secret"), 0o755) })

	e := newEngine(t, "h")
	res, err := e.Backup(ctx, permsRepo(t, e), []string{src}, nil, keepOne)
	if err != nil || res[0].Err != nil {
		t.Fatalf("%+v %v", res, err)
	}
	if res[0].Errors == 0 || len(res[0].Warnings) == 0 || !strings.Contains(res[0].Warnings[0], "secret") {
		t.Fatalf("unreadable directory not reported by name: errors=%d warnings=%q", res[0].Errors, res[0].Warnings)
	}
}

func TestRestoreReportsOwnership(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "a"), "a")
	e := newEngine(t, "h")
	r := permsRepo(t, e)
	res, _ := e.Backup(ctx, r, []string{src}, nil, keepOne)

	st, err := e.Restore(ctx, r, res[0].SnapshotID, "", filepath.Join(t.TempDir(), "out"), false)
	if err != nil {
		t.Fatal(err)
	}
	warned := len(st.Warnings) == 1 && strings.Contains(st.Warnings[0], "ownership was not restored")
	if e.canChown == warned {
		t.Fatalf("canChown=%v but warnings=%q", e.canChown, st.Warnings)
	}
}

func TestRestoreRefusesReadOnlyTarget(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "a"), "a")
	e := newEngine(t, "h")
	r := permsRepo(t, e)
	res, _ := e.Backup(ctx, r, []string{src}, nil, keepOne)

	roDir := filepath.Join(t.TempDir(), "ro-mount")
	orig := readOnly
	readOnly = func(p string) (bool, error) { return strings.HasPrefix(p, roDir), nil }
	t.Cleanup(func() { readOnly = orig })

	_, err := e.Restore(ctx, r, res[0].SnapshotID, "", filepath.Join(roDir, "out"), false)
	if err == nil || !strings.Contains(err.Error(), "mounted read-only") {
		t.Fatalf("restore onto a read-only mount: %v", err)
	}
	if _, err := os.Stat(filepath.Join(roDir, "out")); err == nil {
		t.Fatal("restore wrote into the read-only target")
	}
}
