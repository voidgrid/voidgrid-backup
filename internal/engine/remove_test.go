package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/voidgrid/voidgrid-backup/internal/repocfg"
)

func removeFixture(t *testing.T) (*Engine, Repo, string, string) {
	t.Helper()
	repoDir := filepath.Join(t.TempDir(), "repo")
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "a.txt"), "one")
	e := newEngine(t, "host-a")
	r := Repo{ID: "r1", Password: "pw", Config: repocfg.Config{
		Kind: repocfg.KindFilesystem, Filesystem: &repocfg.Filesystem{Path: repoDir}}}
	if _, _, err := e.Init(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	return e, r, src, repoDir
}

func TestDeleteSnapshot(t *testing.T) {
	ctx := context.Background()
	e, r, src, _ := removeFixture(t)
	res, err := e.Backup(ctx, r, []string{src}, nil, Retention{Latest: 10})
	if err != nil || res[0].Err != nil {
		t.Fatalf("backup: %+v %v", res, err)
	}
	id := res[0].SnapshotID
	if err := e.DeleteSnapshot(ctx, r, id); err != nil {
		t.Fatal(err)
	}
	snaps, err := e.ListSnapshots(ctx, r, []string{src})
	if err != nil || len(snaps) != 0 {
		t.Fatalf("snapshot still listed: %+v %v", snaps, err)
	}
	if err := e.DeleteSnapshot(ctx, r, id); err == nil {
		t.Fatal("deleting a snapshot twice must fail")
	}
}

func TestDeleteSnapshotRefusesOtherHosts(t *testing.T) {
	ctx := context.Background()
	e, r, src, _ := removeFixture(t)
	res, err := e.Backup(ctx, r, []string{src}, nil, Retention{Latest: 10})
	if err != nil || res[0].Err != nil {
		t.Fatalf("backup: %+v %v", res, err)
	}
	other := newEngine(t, "host-b")
	err = other.DeleteSnapshot(ctx, r, res[0].SnapshotID)
	if err == nil || !strings.Contains(err.Error(), "not to this agent") {
		t.Fatalf("a snapshot of another host must be refused, got %v", err)
	}
	if snaps, _ := e.ListSnapshots(ctx, r, []string{src}); len(snaps) != 1 {
		t.Fatal("the snapshot must survive a refused delete")
	}
}

func TestWipe(t *testing.T) {
	ctx := context.Background()
	e, r, src, repoDir := removeFixture(t)
	if res, err := e.Backup(ctx, r, []string{src}, nil, keepOne); err != nil || res[0].Err != nil {
		t.Fatalf("backup: %+v %v", res, err)
	}
	stray := filepath.Join(repoDir, "notes.txt")
	writeFile(t, stray, "not a kopia file")

	blobs, bytes, err := e.Wipe(ctx, r)
	if err != nil || blobs == 0 || bytes == 0 {
		t.Fatalf("wipe: blobs=%d bytes=%d err=%v", blobs, bytes, err)
	}
	if _, err := os.Stat(stray); err != nil {
		t.Fatalf("a file that is not a Kopia blob must survive: %v", err)
	}
	if m, _ := filepath.Glob(filepath.Join(e.dir, "r1-*")); len(m) != 0 {
		t.Fatalf("local cache not removed: %v", m)
	}
	if _, _, err := e.Wipe(ctx, r); err == nil || !strings.Contains(err.Error(), "nothing was deleted") {
		t.Fatalf("a second wipe must refuse, got %v", err)
	}
	// The location can host a new repository afterwards.
	if created, _, err := e.Init(ctx, r); err != nil || !created {
		t.Fatalf("re-init after wipe: created=%v err=%v", created, err)
	}
}

func TestWipeRefusesWhereThereIsNoRepository(t *testing.T) {
	e := newEngine(t, "host-a")
	dir := t.TempDir()
	keep := filepath.Join(dir, "keep.txt")
	writeFile(t, keep, "x")
	r := Repo{ID: "r2", Config: repocfg.Config{Kind: repocfg.KindFilesystem, Filesystem: &repocfg.Filesystem{Path: dir}}}
	if _, _, err := e.Wipe(context.Background(), r); err == nil {
		t.Fatal("wipe of a directory with no repository must fail")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatal(err)
	}
}

func TestMaintainRunsForTheOwner(t *testing.T) {
	ctx := context.Background()
	e, r, _, _ := removeFixture(t) // Init made this client the maintenance owner
	ran, owner, err := e.Maintain(ctx, r)
	if err != nil || !ran || owner != "" {
		t.Fatalf("owner: ran=%v owner=%q err=%v", ran, owner, err)
	}
	other := newEngine(t, "host-b")
	ran, owner, err = other.Maintain(ctx, r)
	if err != nil || ran || !strings.HasSuffix(owner, "@host-a") {
		t.Fatalf("non-owner: ran=%v owner=%q err=%v", ran, owner, err)
	}
}
