package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/voidgrid/voidgrid-backup/internal/engine"
	"github.com/voidgrid/voidgrid-backup/internal/repocfg"
	"github.com/voidgrid/voidgrid-backup/internal/sftptest"
)

// seedRepo backs up a small directory from "host-a" and returns what a
// recover invocation needs, plus the snapshot ID for restore tests.
func seedRepo(t *testing.T) (repoDir, configPath, passwordFile, snapshotID string) {
	t.Helper()
	ctx := context.Background()
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "hello.txt"), []byte("hi there"), 0o644); err != nil {
		t.Fatal(err)
	}
	repoDir = filepath.Join(t.TempDir(), "repo")
	cfg := repocfg.Config{Kind: repocfg.KindFilesystem, Filesystem: &repocfg.Filesystem{Path: repoDir}}
	r := engine.Repo{ID: "r", Password: "correct-horse", Config: cfg}

	e, err := engine.New(t.TempDir(), "host-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Init(ctx, r); err != nil {
		t.Fatal(err)
	}
	res, err := e.Backup(ctx, r, []string{src}, nil, engine.Retention{Latest: 1})
	if err != nil || len(res) != 1 || res[0].Err != nil {
		t.Fatalf("seed backup: %+v %v", res, err)
	}

	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	configPath = filepath.Join(t.TempDir(), "repo.json")
	if err := os.WriteFile(configPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	passwordFile = filepath.Join(t.TempDir(), "pw.txt")
	if err := os.WriteFile(passwordFile, []byte(r.Password), 0o600); err != nil {
		t.Fatal(err)
	}
	return repoDir, configPath, passwordFile, res[0].SnapshotID
}

func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	f()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stdout = orig
	out, _ := io.ReadAll(r)
	return string(out)
}

func TestListSeesSnapshotFromAnyHost(t *testing.T) {
	_, configPath, passwordFile, snapshotID := seedRepo(t)
	out := captureStdout(t, func() {
		if err := runList([]string{"-config", configPath, "-password-file", passwordFile}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "host-a") || !strings.Contains(out, snapshotID) {
		t.Fatalf("list output missing host or snapshot: %q", out)
	}
}

// TestListWithoutAnyFile exercises the primary, bulletproof path: the
// connection given entirely as flags, no repo.json required at all.
func TestListWithoutAnyFile(t *testing.T) {
	repoDir, _, passwordFile, snapshotID := seedRepo(t)
	out := captureStdout(t, func() {
		err := runList([]string{"-password-file", passwordFile,
			"-kind", "filesystem", "-fs-path", repoDir})
		if err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "host-a") || !strings.Contains(out, snapshotID) {
		t.Fatalf("list output missing host or snapshot: %q", out)
	}
}

func TestRestoreFromRecovery(t *testing.T) {
	_, configPath, passwordFile, snapshotID := seedRepo(t)
	target := filepath.Join(t.TempDir(), "out")
	out := captureStdout(t, func() {
		err := runRestore([]string{"-config", configPath, "-password-file", passwordFile,
			"-snapshot", snapshotID, "-target", target})
		if err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "restored 1 files") {
		t.Fatalf("restore output: %q", out)
	}
	got, err := os.ReadFile(filepath.Join(target, "hello.txt"))
	if err != nil || string(got) != "hi there" {
		t.Fatalf("restored file: %q %v", got, err)
	}
}

func TestOpenRepoRequiresConnectionAndPassword(t *testing.T) {
	if _, _, _, err := openRepo(repoFlags{}); err == nil {
		t.Fatal("no -config and no -kind accepted")
	}
	repoDir, configPath, _, _ := seedRepo(t)
	if _, _, _, err := openRepo(repoFlags{config: configPath}); err == nil {
		t.Fatal("missing password accepted")
	}
	if _, _, _, err := openRepo(repoFlags{kind: "filesystem", config: configPath}); err == nil {
		t.Fatal("-config and -kind together accepted")
	}
	if _, _, _, err := openRepo(repoFlags{kind: "carrier-pigeon", fsPath: repoDir}); err == nil {
		t.Fatal("unknown -kind accepted")
	}
}

func TestRestoreRejectsBadTarget(t *testing.T) {
	_, configPath, passwordFile, snapshotID := seedRepo(t)
	err := runRestore([]string{"-config", configPath, "-password-file", passwordFile,
		"-snapshot", snapshotID, "-target", "relative/path"})
	if err == nil {
		t.Fatal("relative restore target accepted")
	}
}

// The docs and the usage text put the connection flags before the subcommand;
// both orders work, and a flag's value is never taken for the subcommand.
func TestSplitCommand(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		sub  string
		args []string
	}{
		{[]string{"-password-file", "pw", "-kind", "s3", "list"}, "list", []string{"-password-file", "pw", "-kind", "s3"}},
		{[]string{"list", "-password-file", "pw"}, "list", []string{"-password-file", "pw"}},
		{[]string{"-kind", "s3", "restore", "-snapshot", "abc", "-target", "/r"}, "restore", []string{"-kind", "s3", "-snapshot", "abc", "-target", "/r"}},
		{[]string{"-fs-path", "list", "-kind", "filesystem", "restore", "-target", "/r"}, "restore", []string{"-fs-path", "list", "-kind", "filesystem", "-target", "/r"}},
	} {
		sub, args, err := splitCommand(tc.argv)
		if err != nil || sub != tc.sub || !slices.Equal(args, tc.args) {
			t.Errorf("%v: got %q %v %v, want %q %v", tc.argv, sub, args, err, tc.sub, tc.args)
		}
	}
	for _, argv := range [][]string{{}, {"-kind", "s3"}, {"frobnicate"}, {"-no-such-flag", "list"}} {
		if sub, _, err := splitCommand(argv); err == nil {
			t.Errorf("%v: accepted as %q", argv, sub)
		}
	}
	if _, _, err := splitCommand([]string{"-h"}); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("-h: %v", err)
	}
}

// The flags-first form the docs show, end to end.
func TestListWithFlagsBeforeTheSubcommand(t *testing.T) {
	repoDir, _, passwordFile, snapshotID := seedRepo(t)
	sub, args, err := splitCommand([]string{"-password-file", passwordFile, "-kind", "filesystem", "-fs-path", repoDir, "list"})
	if err != nil || sub != "list" {
		t.Fatalf("%q %v", sub, err)
	}
	out := captureStdout(t, func() {
		if err := runList(args); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, snapshotID) {
		t.Fatalf("list output: %q", out)
	}
}

// Over SFTP with no known_hosts saved, the host key seen is trusted, as the
// flag's help says: a recovering machine has no ~/.ssh/known_hosts.
func TestListOverSFTPWithoutKnownHosts(t *testing.T) {
	port, keyFile := sftptest.Start(t)
	path := filepath.Join(t.TempDir(), "repo")
	cfg := repocfg.Config{Kind: repocfg.KindSFTP, SFTP: &repocfg.SFTP{Host: "127.0.0.1", Port: port, User: "u", Path: path, KeyFile: keyFile}}
	e, err := engine.New(t.TempDir(), "host-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Init(context.Background(), engine.Repo{ID: "r", Password: "pw", Config: cfg}); err != nil {
		t.Fatal(err)
	}
	pw := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(pw, []byte("pw"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		if err := runList([]string{"-password-file", pw, "-kind", "sftp", "-sftp-host", "127.0.0.1", "-sftp-port", strconv.Itoa(port),
			"-sftp-user", "u", "-sftp-path", path, "-sftp-key-file", keyFile}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "no snapshots found") {
		t.Fatalf("list output: %q", out)
	}
}
