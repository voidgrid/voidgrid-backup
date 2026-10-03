//go:build s3live

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

// TestLiveS3Backup runs against a real S3-compatible bucket (e.g. Backblaze
// B2's S3-compatible endpoint). scripts/s3-live-test.sh supplies the
// connection from .env and picks a throwaway prefix so nothing has to be
// cleaned up in the bucket afterwards except that prefix, which the test
// removes itself.
func TestLiveS3Backup(t *testing.T) {
	ctx := context.Background()
	endpoint := os.Getenv("VB_LIVE_S3_ENDPOINT")
	bucket := os.Getenv("VB_LIVE_S3_BUCKET")
	if endpoint == "" || bucket == "" {
		t.Skip("VB_LIVE_S3_ENDPOINT, VB_LIVE_S3_BUCKET (and region/keys/prefix) not set")
	}
	prefix := os.Getenv("VB_LIVE_S3_PREFIX")
	if prefix == "" {
		t.Fatal("VB_LIVE_S3_PREFIX must be set to a throwaway prefix (scripts/s3-live-test.sh sets one)")
	}
	insecure, _ := strconv.ParseBool(os.Getenv("VB_LIVE_S3_INSECURE"))
	cfg := repocfg.Config{Kind: repocfg.KindS3, S3: &repocfg.S3{
		Endpoint:        endpoint,
		Bucket:          bucket,
		Prefix:          prefix,
		Region:          os.Getenv("VB_LIVE_S3_REGION"),
		AccessKeyID:     os.Getenv("VB_LIVE_S3_ACCESS_KEY"),
		SecretAccessKey: os.Getenv("VB_LIVE_S3_SECRET_KEY"),
		Insecure:        insecure,
	}}
	r := Repo{ID: "live-s3", Password: "correct-horse-battery-staple", Config: cfg}
	e := newEngine(t, "live-s3-host")

	// Cleanup happens even if the test fails partway through: everything
	// this test writes lives under prefix, and nothing else in the bucket
	// is touched.
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
		t.Logf("cleanup: removed %d blobs under prefix %s", len(ids), prefix)
	}()

	created, _, err := e.Init(ctx, r)
	if err != nil || !created {
		t.Fatalf("init: created=%v err=%v", created, err)
	}

	src := t.TempDir()
	writeFile(t, filepath.Join(src, "hello.txt"), "hello from a real S3 bucket")
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
	if readFile(t, filepath.Join(out, "hello.txt")) != "hello from a real S3 bucket" {
		t.Fatal("restored content does not match")
	}
}
