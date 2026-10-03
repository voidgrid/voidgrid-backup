//go:build dockerlive

package docker_test

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/docker"
)

// TestLiveDumps runs the real dump/import commands inside real database
// images started by scripts/docker-live-test.sh.
func TestLiveDumps(t *testing.T) {
	ctx := context.Background()
	c := docker.New("/var/run/docker.sock")

	run := func(id string, cmd []string, stdin string) (string, error) {
		var in io.Reader
		if stdin != "" {
			in = strings.NewReader(stdin)
		}
		rc, err := c.Exec(ctx, id, cmd, nil, in)
		if err != nil {
			return "", err
		}
		defer rc.Close()
		b, err := io.ReadAll(rc)
		return string(b), err
	}
	// Databases take a few seconds to accept connections after start.
	retry := func(id string, cmd []string, stdin string) (string, error) {
		var out string
		var err error
		for deadline := time.Now().Add(90 * time.Second); time.Now().Before(deadline); time.Sleep(time.Second) {
			if out, err = run(id, cmd, stdin); err == nil {
				return out, nil
			}
		}
		return out, err
	}

	for _, tc := range []struct {
		env, kind, sql, want string
	}{
		{"VB_LIVE_PG", docker.DumpPostgres, "CREATE TABLE vb_probe (v text); INSERT INTO vb_probe VALUES ('pg-ok');", "pg-ok"},
		{"VB_LIVE_MARIA", docker.DumpMariaDB, "CREATE DATABASE vb; CREATE TABLE vb.probe (v text); INSERT INTO vb.probe VALUES ('maria-ok');", "maria-ok"},
	} {
		id := os.Getenv(tc.env)
		if id == "" {
			t.Fatalf("%s not set", tc.env)
		}
		imp, _ := docker.ImportCommand(tc.kind)
		if _, err := retry(id, imp, tc.sql); err != nil {
			t.Fatalf("%s import: %v", tc.kind, err)
		}
		dump, _, _ := docker.DumpCommand(tc.kind)
		out, err := run(id, dump, "")
		if err != nil || !strings.Contains(out, tc.want) {
			t.Fatalf("%s dump (%d bytes) missing %q: %v", tc.kind, len(out), tc.want, err)
		}
	}

	id := os.Getenv("VB_LIVE_VALKEY")
	if _, err := retry(id, []string{"valkey-cli", "SET", "vb", "valkey-ok"}, ""); err != nil {
		t.Fatalf("valkey set: %v", err)
	}
	dump, _, _ := docker.DumpCommand(docker.DumpRedis)
	out, err := run(id, dump, "")
	if err != nil || !strings.HasPrefix(out, "REDIS") || !strings.Contains(out, "valkey-ok") {
		t.Fatalf("valkey dump (%d bytes): %v", len(out), err)
	}
}
