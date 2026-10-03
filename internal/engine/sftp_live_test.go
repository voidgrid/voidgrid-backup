//go:build sftplive

package engine

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kopia/kopia/repo/blob"

	"github.com/voidgrid/voidgrid-backup/internal/repocfg"
)

// TestLiveSFTPBackup runs against a real SFTP destination (e.g. a Hetzner
// Storage Box). scripts/sftp-live-test.sh supplies the connection from
// .env and picks a throwaway path so nothing has to be cleaned up on the
// box afterwards except that path's blobs, which the test removes itself.
func TestLiveSFTPBackup(t *testing.T) {
	ctx := context.Background()
	host := os.Getenv("VB_LIVE_SFTP_HOST")
	user := os.Getenv("VB_LIVE_SFTP_USER")
	if host == "" || user == "" {
		t.Skip("VB_LIVE_SFTP_HOST, VB_LIVE_SFTP_USER (and port/key or password/path) not set")
	}
	path := os.Getenv("VB_LIVE_SFTP_PATH")
	if path == "" {
		t.Fatal("VB_LIVE_SFTP_PATH must be set to a throwaway path (scripts/sftp-live-test.sh sets one)")
	}
	port, err := strconv.Atoi(os.Getenv("VB_LIVE_SFTP_PORT"))
	if err != nil {
		port = 22
	}
	cfg := repocfg.Config{Kind: repocfg.KindSFTP, SFTP: &repocfg.SFTP{
		Host: host, Port: port, User: user, Path: path,
		KeyFile:  os.Getenv("VB_LIVE_SFTP_KEY_FILE"),
		Password: os.Getenv("VB_LIVE_SFTP_PASSWORD"),
	}}
	r := Repo{ID: "live-sftp", Password: "correct-horse-battery-staple", Config: cfg}
	e := newEngine(t, "live-sftp-host")

	// Cleanup happens even if the test fails partway through: everything
	// this test writes lives under path, and nothing else on the box is
	// touched. The now-empty directory shell (Kopia shards blobs into
	// subdirectories) may be left behind; it costs nothing.
	defer func() {
		st, err := storage(ctx, cfg, false)
		if err != nil {
			t.Logf("cleanup: open storage: %v", err)
			return
		}
		defer st.Close(ctx)
		var ids []blob.ID
		if err := st.ListBlobs(ctx, "", func(m blob.Metadata) error {
			ids = append(ids, m.BlobID)
			return nil
		}); err != nil {
			t.Logf("cleanup: list blobs: %v", err)
			return
		}
		for _, id := range ids {
			if err := st.DeleteBlob(ctx, id); err != nil {
				t.Logf("cleanup: delete %s: %v", id, err)
			}
		}
		t.Logf("cleanup: removed %d blobs under %s", len(ids), path)
	}()

	created, _, err := e.Init(ctx, r)
	if err != nil || !created {
		t.Fatalf("init: created=%v err=%v", created, err)
	}

	src := t.TempDir()
	writeFile(t, filepath.Join(src, "hello.txt"), "hello over sftp")
	res, err := e.Backup(ctx, r, []string{src}, nil, keepOne)
	if err != nil || len(res) != 1 || res[0].Err != nil {
		t.Fatalf("backup: %+v %v", res, err)
	}

	check, err := e.Check(ctx, r, CheckOptions{VerifyPercent: 100, TestRestoreMaxBytes: 1 << 20})
	if err != nil || !check.OK() {
		t.Fatalf("check: %+v %v", check, err)
	}
	if !strings.Contains(check.TestRestore, "1 files") {
		t.Fatalf("check did not test-restore: %+v", check)
	}

	out := filepath.Join(t.TempDir(), "out")
	if _, err := e.Restore(ctx, r, res[0].SnapshotID, "", out, false); err != nil {
		t.Fatal(err)
	}
	if readFile(t, filepath.Join(out, "hello.txt")) != "hello over sftp" {
		t.Fatal("restored content does not match")
	}
}
