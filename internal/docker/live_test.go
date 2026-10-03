//go:build dockerlive

package docker_test

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/voidgrid/voidgrid-backup/internal/docker"
)

// TestLive runs against a real Docker daemon. scripts/docker-live-test.sh
// starts the container it expects and removes it afterwards.
func TestLive(t *testing.T) {
	ctx := context.Background()
	id := os.Getenv("VB_LIVE_CONTAINER")
	if id == "" {
		t.Skip("VB_LIVE_CONTAINER not set")
	}
	c := docker.New("/var/run/docker.sock")

	rc, err := c.Exec(ctx, id, []string{"cat"}, nil, strings.NewReader("real docker stdin"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || string(out) != "real docker stdin" {
		t.Fatalf("cat: %q %v", out, err)
	}

	rc, _ = c.Exec(ctx, id, []string{"sh", "-c", "head -c 3000000 /dev/zero"}, nil, nil)
	out, err = io.ReadAll(rc)
	rc.Close()
	if err != nil || len(out) != 3000000 {
		t.Fatalf("large output: %d bytes, %v", len(out), err)
	}

	rc, _ = c.Exec(ctx, id, []string{"sh", "-c", "echo oops >&2; exit 4"}, nil, nil)
	_, err = io.ReadAll(rc)
	rc.Close()
	if err == nil || !strings.Contains(err.Error(), "exited with 4") || !strings.Contains(err.Error(), "oops") {
		t.Fatalf("failing command: %v", err)
	}

	if err := c.Pause(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := c.Unpause(ctx, id); err != nil {
		t.Fatal(err)
	}

	st, found, err := c.FindStack(ctx, "hblive")
	if err != nil || !found || len(st.Services) != 1 || st.Services[0].Name != "probe" {
		t.Fatalf("stack discovery: %+v found=%v %v", st, found, err)
	}
}
