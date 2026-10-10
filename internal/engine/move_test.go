package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/voidgrid/voidgrid-backup/internal/repocfg"
)

func fsConfig(p string) repocfg.Config {
	return repocfg.Config{Kind: repocfg.KindFilesystem, Filesystem: &repocfg.Filesystem{Path: p}}
}

func TestMoveFilesystemRepository(t *testing.T) {
	ctx := context.Background()
	e, r, src, oldDir := removeFixture(t)
	if _, err := e.Backup(ctx, r, []string{src}, nil, Retention{Latest: 10}); err != nil {
		t.Fatal(err)
	}
	newDir := filepath.Join(t.TempDir(), "not", "yet", "there", "repo")

	if err := e.MoveRepository(ctx, r.Config, fsConfig(newDir)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Fatalf("old directory still exists: %v", err)
	}
	moved := r
	moved.Config = fsConfig(newDir)
	if _, err := e.TestRepository(ctx, moved); err != nil {
		t.Fatalf("test at the new path: %v", err)
	}
	snaps, err := e.ListSnapshots(ctx, moved, nil)
	if err != nil || len(snaps) != 1 {
		t.Fatalf("snapshots after the move: %d %v", len(snaps), err)
	}

	// Moving back works the same way (what the server does after a failed test).
	if err := e.MoveRepository(ctx, moved.Config, r.Config); err != nil {
		t.Fatalf("move back: %v", err)
	}
}

func TestMoveRefusals(t *testing.T) {
	ctx := context.Background()
	e, r, _, oldDir := removeFixture(t)
	taken := t.TempDir() // exists, empty
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.MkdirAll(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		from, to repocfg.Config
		want     string
	}{
		"destination exists":  {r.Config, fsConfig(taken), "already exists"},
		"same path":           {r.Config, fsConfig(oldDir), "same as the current"},
		"into itself":         {r.Config, fsConfig(filepath.Join(oldDir, "sub")), "inside the current"},
		"no repository there": {fsConfig(empty), fsConfig(filepath.Join(t.TempDir(), "x")), "no repository was found"},
		"different kind": {r.Config, repocfg.Config{Kind: repocfg.KindSFTP, SFTP: &repocfg.SFTP{
			Host: "h", Port: 22, User: "u", Path: "p", Password: "x"}}, "different kind"},
		"s3": {repocfg.Config{Kind: repocfg.KindS3, S3: &repocfg.S3{Endpoint: "e", Bucket: "b", AccessKeyID: "a", SecretAccessKey: "s"}},
			repocfg.Config{Kind: repocfg.KindS3, S3: &repocfg.S3{Endpoint: "e", Bucket: "b", Prefix: "q", AccessKeyID: "a", SecretAccessKey: "s"}},
			"cannot be moved"},
	} {
		t.Run(name, func(t *testing.T) {
			err := e.MoveRepository(ctx, tc.from, tc.to)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
	// Nothing was touched by any refusal.
	if _, err := e.TestRepository(ctx, r); err != nil {
		t.Fatalf("repository damaged by a refused move: %v", err)
	}
}

func TestMoveSFTPRepository(t *testing.T) {
	ctx := context.Background()
	port, keyFile := startSFTPServer(t)
	base := t.TempDir()
	e := newEngine(t, "host-a")
	r := Repo{ID: "box", Password: "pw", Config: repocfg.Config{Kind: repocfg.KindSFTP,
		SFTP: &repocfg.SFTP{Host: "127.0.0.1", Port: port, User: "u", Path: filepath.Join(base, "old"), KeyFile: keyFile}}}
	_, kh, err := e.Init(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	r.Config.SFTP.KnownHosts = kh
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "f.txt"), "over sftp")
	if res, err := e.Backup(ctx, r, []string{src}, nil, keepOne); err != nil || res[0].Err != nil {
		t.Fatalf("backup: %+v %v", res, err)
	}

	to := r.Config
	sftpTo := *r.Config.SFTP
	sftpTo.Path = filepath.Join(base, "hosts", "a", "new")
	to.SFTP = &sftpTo

	// Without a pinned host key the move must not connect.
	noKey := r.Config
	noKeySFTP := *r.Config.SFTP
	noKeySFTP.KnownHosts = ""
	noKey.SFTP = &noKeySFTP
	if err := e.MoveRepository(ctx, noKey, to); err == nil || !strings.Contains(err.Error(), "host key") {
		t.Fatalf("move without a pinned host key: %v", err)
	}

	if err := e.MoveRepository(ctx, r.Config, to); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, "old")); !os.IsNotExist(err) {
		t.Fatalf("old directory still exists: %v", err)
	}
	moved := r
	moved.Config = to
	if _, err := e.TestRepository(ctx, moved); err != nil {
		t.Fatalf("test at the new path: %v", err)
	}
	snaps, err := e.ListSnapshots(ctx, moved, nil)
	if err != nil || len(snaps) != 1 {
		t.Fatalf("snapshots after the move: %d %v", len(snaps), err)
	}
	// The destination now exists, so moving there again is refused.
	if err := e.MoveRepository(ctx, to, r.Config); err != nil {
		t.Fatalf("move back: %v", err)
	}
}

func TestTestRepositoryNeverCreates(t *testing.T) {
	ctx := context.Background()
	e := newEngine(t, "host-a")
	empty := t.TempDir()
	r := Repo{ID: "r9", Password: "pw", Config: fsConfig(empty)}
	if _, err := e.TestRepository(ctx, r); err == nil {
		t.Fatal("TestRepository succeeded on an empty directory")
	}
	if names, _ := os.ReadDir(empty); len(names) != 0 {
		t.Fatalf("TestRepository wrote to the storage: %v", names)
	}
	if left, _ := filepath.Glob(filepath.Join(e.dir, "r9-*")); len(left) != 0 {
		t.Fatalf("a failed test left local state behind: %v", left)
	}
}

func TestTestRepositoryWrongPassword(t *testing.T) {
	e, r, _, _ := removeFixture(t)
	r.Password = "not the password"
	if _, err := e.TestRepository(context.Background(), r); err == nil {
		t.Fatal("TestRepository accepted a wrong password")
	}
}

const sampleMountinfo = `22 28 0:21 / /sys rw,nosuid shared:7 - sysfs sysfs rw
28 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw
40 28 0:35 / /mnt/share rw,relatime shared:50 - cifs //nas/backup rw,vers=3.1.1
41 28 0:36 / /mnt/with\040space rw,relatime shared:51 - nfs4 nas:/x rw
`

func TestMountOf(t *testing.T) {
	for p, want := range map[string]string{
		"/mnt/share/backups/host": "cifs",
		"/mnt/share":              "cifs",
		"/mnt/sharex":             "ext4", // a sibling, not under /mnt/share
		"/var/backup":             "ext4",
		"/mnt/with space/repo":    "nfs4",
	} {
		if got, _ := mountOf(sampleMountinfo, p); got != want {
			t.Errorf("mountOf(%q) = %q, want %q", p, got, want)
		}
	}
	if fstype, _ := mountOf("", "/x"); fstype != "" {
		t.Errorf("an empty table gave %q", fstype)
	}
}

func TestTestRepositoryNeverCreatesOverSFTP(t *testing.T) {
	port, keyFile := startSFTPServer(t)
	dir := t.TempDir()
	e := newEngine(t, "host-a")
	kh, err := FetchHostKey(context.Background(), "127.0.0.1", port)
	if err != nil {
		t.Fatal(err)
	}
	r := Repo{ID: "r8", Password: "pw", Config: repocfg.Config{Kind: repocfg.KindSFTP,
		SFTP: &repocfg.SFTP{Host: "127.0.0.1", Port: port, User: "u", Path: dir, KeyFile: keyFile, KnownHosts: kh}}}
	if _, err := e.TestRepository(context.Background(), r); err == nil {
		t.Fatal("TestRepository succeeded on an empty SFTP directory")
	}
	if names, _ := os.ReadDir(dir); len(names) != 0 {
		t.Fatalf("TestRepository wrote to the SFTP storage: %v", names)
	}
}

func TestWipeWrongPathWritesNothing(t *testing.T) {
	e := newEngine(t, "host-a")
	empty := t.TempDir()
	if _, _, err := e.Wipe(context.Background(), Repo{ID: "r7", Config: fsConfig(empty)}); err == nil {
		t.Fatal("Wipe succeeded where there is no repository")
	}
	if names, _ := os.ReadDir(empty); len(names) != 0 {
		t.Fatalf("Wipe wrote into a directory with no repository: %v", names)
	}
}
