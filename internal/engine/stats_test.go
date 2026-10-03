package engine

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/voidgrid/voidgrid-backup/internal/repocfg"
)

func TestRepoStatsFilesystem(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "a.txt"), "some content worth storing")

	e := newEngine(t, "host-a")
	r := Repo{ID: "r1", Password: "pw", Config: repocfg.Config{
		Kind:       repocfg.KindFilesystem,
		Filesystem: &repocfg.Filesystem{Path: filepath.Join(t.TempDir(), "repo")},
	}}
	if _, _, err := e.Init(ctx, r); err != nil {
		t.Fatal(err)
	}
	before, err := e.RepoStats(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if res, err := e.Backup(ctx, r, []string{src}, nil, keepOne); err != nil || res[0].Err != nil {
		t.Fatalf("backup: %+v %v", res, err)
	}
	after, err := e.RepoStats(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if after.Blobs <= before.Blobs || after.StoredBytes <= before.StoredBytes {
		t.Errorf("a backup should grow the repository: before %+v, after %+v", before, after)
	}
	if after.FSTotal == 0 || after.FSAvail == 0 {
		t.Errorf("a filesystem repository should report its volume: %+v", after)
	}
}
