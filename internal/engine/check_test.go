package engine

import (
	"context"
	"crypto/rand"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/voidgrid/voidgrid-backup/internal/repocfg"
)

// checkFixture backs up a directory with some incompressible data so the
// repository has real pack blobs to damage.
func checkFixture(t *testing.T, ecc bool) (*Engine, Repo, string, string) {
	t.Helper()
	ctx := context.Background()
	src := t.TempDir()
	blob := make([]byte, 256<<10)
	rand.Read(blob)
	os.WriteFile(filepath.Join(src, "random.bin"), blob, 0o644)
	writeFile(t, filepath.Join(src, "small.txt"), "hello")
	repoDir := filepath.Join(t.TempDir(), "repo")
	e := newEngine(t, "host-a")
	r := Repo{ID: "r", Password: "pw", Config: repocfg.Config{Kind: repocfg.KindFilesystem, ECC: ecc,
		Filesystem: &repocfg.Filesystem{Path: repoDir}}}
	if _, _, err := e.Init(ctx, r); err != nil {
		t.Fatal(err)
	}
	res, err := e.Backup(ctx, r, []string{src}, nil, keepOne)
	if err != nil || res[0].Err != nil {
		t.Fatalf("%+v %v", res, err)
	}
	return e, r, repoDir, res[0].SnapshotID
}

// damagePacks flips a few bytes in the middle of every data pack blob.
func damagePacks(t *testing.T, repoDir string) int {
	t.Helper()
	n := 0
	filepath.WalkDir(repoDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		// Data packs are the "p" blobs; the filesystem backend shards the
		// blob ID into directories, so match on the path, not the file name.
		if rel, _ := filepath.Rel(repoDir, p); !strings.HasPrefix(rel, "p") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil || len(b) < 64<<10 {
			return err
		}
		for i := 0; i < 4; i++ {
			b[len(b)/2+i*97] ^= 0xff
		}
		os.Chmod(p, 0o644)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		n++
		return nil
	})
	if n == 0 {
		t.Fatal("no pack blobs found to damage")
	}
	return n
}

func TestCheckHealthyRepository(t *testing.T) {
	e, r, _, _ := checkFixture(t, false)
	res, err := e.Check(context.Background(), r, CheckOptions{VerifyPercent: 100, TestRestoreMaxBytes: 1 << 30})
	if err != nil || !res.OK() || res.Snapshots != 1 || res.FilesRead != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	if !strings.Contains(res.TestRestore, "2 files") {
		t.Fatalf("test restore did not run: %+v", res)
	}
	if left, _ := filepath.Glob(filepath.Join(e.dir, "test-restore-*")); len(left) != 0 {
		t.Fatalf("scratch restore left behind: %v", left)
	}
	// Over the cap: skipped, not failed.
	res, _ = e.Check(context.Background(), r, CheckOptions{VerifyPercent: 0, TestRestoreMaxBytes: 10})
	if !res.OK() || res.TestRestore != "" {
		t.Fatalf("capped test restore: %+v", res)
	}
}

func TestCheckFindsBitrot(t *testing.T) {
	e, r, repoDir, _ := checkFixture(t, false)
	damagePacks(t, repoDir)
	res, err := e.Check(context.Background(), r, CheckOptions{VerifyPercent: 100})
	if err != nil {
		t.Fatal(err)
	}
	if res.OK() {
		t.Fatalf("damaged repository passed the check: %+v", res)
	}
}

func TestECCRepairsBitrot(t *testing.T) {
	ctx := context.Background()
	e, r, repoDir, snap := checkFixture(t, true)
	damagePacks(t, repoDir)
	res, err := e.Check(ctx, r, CheckOptions{VerifyPercent: 100, TestRestoreMaxBytes: 1 << 30})
	if err != nil || !res.OK() {
		t.Fatalf("ECC did not repair small corruption: %+v %v", res, err)
	}
	out := filepath.Join(t.TempDir(), "out")
	if _, err := e.Restore(ctx, r, snap, "", out, false); err != nil {
		t.Fatal(err)
	}
	if readFile(t, filepath.Join(out, "small.txt")) != "hello" {
		t.Fatal("restore after repair returned wrong data")
	}
}

func TestRecoveryModeSeesAllHosts(t *testing.T) {
	ctx := context.Background()
	e, r, _, snap := checkFixture(t, false)
	_ = e
	rec := newEngine(t, "recovery")
	rec.Recovery()
	all, err := rec.ListAllSnapshots(ctx, r)
	if err != nil || len(all) != 1 || all[0].Host != "host-a" || all[0].ID != snap {
		t.Fatalf("%+v %v", all, err)
	}
	out := filepath.Join(t.TempDir(), "out")
	if _, err := rec.Restore(ctx, r, snap, "", out, false); err != nil {
		t.Fatalf("recovery restore of another host's snapshot: %v", err)
	}
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "x"), "x")
	if res, err := rec.Backup(ctx, r, []string{src}, nil, keepOne); err == nil && res[0].Err == nil {
		t.Fatal("recovery mode wrote a snapshot")
	}
}
