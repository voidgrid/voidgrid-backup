package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
)

func TestPathUsage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f"), make([]byte, 123), 0o644); err != nil {
		t.Fatal(err)
	}
	resp, err := (&Agent{}).PathUsage(context.Background(), &agentpb.PathUsageRequest{
		Paths: []string{dir, "relative/path", "/proc/1", filepath.Join(dir, "missing")},
		Walk:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	u := resp.GetUsages()
	if len(u) != 4 {
		t.Fatalf("got %d results, want 4", len(u))
	}
	if u[0].GetError() != "" || u[0].GetDirBytes() != 123 || !u[0].GetWalked() {
		t.Errorf("real dir: %+v", u[0])
	}
	for i, name := range []string{"relative path", "/proc", "missing path"} {
		if u[i+1].GetError() == "" {
			t.Errorf("%s: want an error, got %+v", name, u[i+1])
		}
	}
}
