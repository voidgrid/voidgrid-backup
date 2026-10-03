// Command voidgrid-backup-recover is the break-glass restore tool: it talks
// to a backup repository directly, with no server, catalog or agent
// involved. Use it when the server host, its SQLite catalog, or any file
// derived from either is gone or untrusted, and the normal web UI restore
// flow isn't an option.
//
// The repository connection is given entirely as flags (host/bucket,
// credentials, path), the same information a repository's "add" form on the
// server takes. Nothing needs to already exist on the machine this runs on
// beyond whatever secret material (an SSH key, S3 keys) reaching the storage
// requires anyway. A repository config JSON file is accepted as a shortcut
// when one happens to be available, never as a requirement.
//
// It opens the repository read-only and can see every host's snapshots, not
// just one. It never writes a snapshot.
//
//	voidgrid-backup-recover -password-file pw.txt \
//	    -kind sftp -sftp-host u123.your-storagebox.de -sftp-port 23 \
//	    -sftp-user u123 -sftp-path backups/homelab -sftp-key-file ./key \
//	    list
//
//	voidgrid-backup-recover -password-file pw.txt \
//	    -kind sftp -sftp-host ... -sftp-user ... -sftp-path ... -sftp-key-file ./key \
//	    restore -snapshot <id> -target /restore/here [-path some/dir] [-overwrite]
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/engine"
	"github.com/voidgrid/voidgrid-backup/internal/guard"
	"github.com/voidgrid/voidgrid-backup/internal/repocfg"
	"github.com/voidgrid/voidgrid-backup/internal/version"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "list":
		err = runList(os.Args[2:])
	case "restore":
		err = runRestore(os.Args[2:])
	case "-h", "-help", "--help":
		usage()
		return
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `voidgrid-backup-recover %s

Restores from a backup repository directly: no server, catalog or agent, and
no file has to already exist on this machine except whatever the storage
itself requires (an SSH private key for SFTP). See docs/recovery.md for the
full list of what to have kept safe for this moment, and for every flag.

Usage:
  voidgrid-backup-recover -password-file pw.txt <connection flags> list
  voidgrid-backup-recover -password-file pw.txt <connection flags> restore \
      -snapshot <id> -target /restore/here [-path some/dir] [-overwrite]

Connection: either -kind plus that kind's flags, or -config <file> (a
repocfg.Config as JSON) as a shortcut when one is available. -h after a
subcommand (e.g. "restore -h") lists every flag.

  -kind sftp|s3|filesystem
  -password-file <path>   file holding the repository password
                          ($VB_RECOVER_PASSWORD if neither is given)
`, version.Version)
}

// repoFlags is the repository connection, given entirely as flags so
// nothing besides the storage credentials themselves has to survive
// whatever this tool is recovering from. -config is accepted as a shortcut
// when a repocfg.Config JSON file happens to be available, never required.
type repoFlags struct {
	config string
	kind   string

	sftpHost, sftpUser, sftpPath       string
	sftpPort                           int
	sftpKeyFile, sftpPassword          string
	sftpKnownHostsFile, sftpKnownHosts string

	s3Endpoint, s3Bucket, s3Prefix, s3Region string
	s3AccessKey, s3SecretKey                 string
	s3Insecure                               bool

	fsPath string

	passwordFile string
	data         string
}

func addRepoFlags(fs *flag.FlagSet, c *repoFlags) {
	fs.StringVar(&c.config, "config", "", "shortcut: repository config as JSON, if one is available (see -kind for the normal path)")
	fs.StringVar(&c.kind, "kind", "", "repository kind: sftp, s3 or filesystem")

	fs.StringVar(&c.sftpHost, "sftp-host", "", "sftp: host (Hetzner Storage Box: uXXXXXX.your-storagebox.de)")
	fs.IntVar(&c.sftpPort, "sftp-port", 22, "sftp: port (Hetzner Storage Box: 23)")
	fs.StringVar(&c.sftpUser, "sftp-user", "", "sftp: user")
	fs.StringVar(&c.sftpPath, "sftp-path", "", "sftp: path on the server, e.g. backups/homelab")
	fs.StringVar(&c.sftpKeyFile, "sftp-key-file", "", "sftp: private key file (or use -sftp-password)")
	fs.StringVar(&c.sftpPassword, "sftp-password", "", "sftp: password ($VB_RECOVER_SFTP_PASSWORD if empty; or use -sftp-key-file)")
	fs.StringVar(&c.sftpKnownHosts, "sftp-known-hosts", "", "sftp: known_hosts content, if you saved it (empty: trust the host key seen on connect)")
	fs.StringVar(&c.sftpKnownHostsFile, "sftp-known-hosts-file", "", "sftp: known_hosts file, if you saved it")

	fs.StringVar(&c.s3Endpoint, "s3-endpoint", "", "s3: endpoint")
	fs.StringVar(&c.s3Bucket, "s3-bucket", "", "s3: bucket")
	fs.StringVar(&c.s3Prefix, "s3-prefix", "", "s3: prefix")
	fs.StringVar(&c.s3Region, "s3-region", "", "s3: region")
	fs.StringVar(&c.s3AccessKey, "s3-access-key", "", "s3: access key ID")
	fs.StringVar(&c.s3SecretKey, "s3-secret-key", "", "s3: secret access key ($VB_RECOVER_S3_SECRET_KEY if empty)")
	fs.BoolVar(&c.s3Insecure, "s3-insecure", false, "s3: plain HTTP (local test server only)")

	fs.StringVar(&c.fsPath, "fs-path", "", "filesystem: directory holding the repository")

	fs.StringVar(&c.passwordFile, "password-file", "", "file holding the repository password ($VB_RECOVER_PASSWORD if empty)")
	fs.StringVar(&c.data, "data", "", "scratch directory for the Kopia connection cache (default: a temp directory)")
}

// buildConfig turns repoFlags into a repocfg.Config: from -config if given,
// otherwise from -kind and that kind's flags. Secrets left as flags fall
// back to an environment variable, so they don't have to be typed where a
// shell might log them, without requiring a file either.
func buildConfig(c repoFlags) (repocfg.Config, error) {
	if c.config != "" {
		if c.kind != "" {
			return repocfg.Config{}, errors.New("give either -config or -kind, not both")
		}
		b, err := os.ReadFile(c.config)
		if err != nil {
			return repocfg.Config{}, fmt.Errorf("read config: %w", err)
		}
		var cfg repocfg.Config
		if err := json.Unmarshal(b, &cfg); err != nil {
			return repocfg.Config{}, fmt.Errorf("parse config: %w", err)
		}
		return cfg, cfg.Validate()
	}

	cfg := repocfg.Config{Kind: c.kind}
	switch c.kind {
	case repocfg.KindSFTP:
		knownHosts := c.sftpKnownHosts
		if c.sftpKnownHostsFile != "" {
			b, err := os.ReadFile(c.sftpKnownHostsFile)
			if err != nil {
				return repocfg.Config{}, fmt.Errorf("read known_hosts file: %w", err)
			}
			knownHosts = string(b)
		}
		if knownHosts != "" && !strings.HasSuffix(knownHosts, "\n") {
			knownHosts += "\n"
		}
		cfg.SFTP = &repocfg.SFTP{
			Host: c.sftpHost, Port: c.sftpPort, User: c.sftpUser, Path: c.sftpPath,
			KeyFile:    c.sftpKeyFile,
			Password:   envDefault(c.sftpPassword, "VB_RECOVER_SFTP_PASSWORD"),
			KnownHosts: knownHosts,
		}
	case repocfg.KindS3:
		cfg.S3 = &repocfg.S3{
			Endpoint: c.s3Endpoint, Bucket: c.s3Bucket, Prefix: c.s3Prefix, Region: c.s3Region,
			AccessKeyID:     c.s3AccessKey,
			SecretAccessKey: envDefault(c.s3SecretKey, "VB_RECOVER_S3_SECRET_KEY"),
			Insecure:        c.s3Insecure,
		}
	case repocfg.KindFilesystem:
		cfg.Filesystem = &repocfg.Filesystem{Path: c.fsPath}
	case "":
		return repocfg.Config{}, errors.New("give -config <file> or -kind sftp|s3|filesystem with its connection flags")
	default:
		return repocfg.Config{}, fmt.Errorf("unknown -kind %q: want sftp, s3 or filesystem", c.kind)
	}
	return cfg, cfg.Validate()
}

func envDefault(v, env string) string {
	if v != "" {
		return v
	}
	return os.Getenv(env)
}

// openRepo builds the repository connection and password, and returns a
// recovery-mode engine over a scratch directory. The returned cleanup
// removes that directory if it was created just for this run.
func openRepo(c repoFlags) (*engine.Engine, engine.Repo, func(), error) {
	noop := func() {}
	cfg, err := buildConfig(c)
	if err != nil {
		return nil, engine.Repo{}, noop, err
	}
	password, err := loadPassword(c.passwordFile)
	if err != nil {
		return nil, engine.Repo{}, noop, err
	}

	dir, cleanup := c.data, noop
	if dir == "" {
		dir, err = os.MkdirTemp("", "voidgrid-backup-recover-*")
		if err != nil {
			return nil, engine.Repo{}, noop, err
		}
		cleanup = func() { os.RemoveAll(dir) } //nolint:errcheck // best-effort cleanup of a temporary directory
	}
	e, err := engine.New(dir, "recovery")
	if err != nil {
		cleanup()
		return nil, engine.Repo{}, noop, err
	}
	e.Recovery()
	return e, engine.Repo{ID: "recovery", Config: cfg, Password: password}, cleanup, nil
}

func loadPassword(file string) (string, error) {
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("read password file: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	if p, ok := os.LookupEnv("VB_RECOVER_PASSWORD"); ok {
		return p, nil
	}
	return "", errors.New("no repository password: set -password-file or $VB_RECOVER_PASSWORD")
}

func runList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	var c repoFlags
	addRepoFlags(fs, &c)
	if err := fs.Parse(args); err != nil {
		return err
	}
	e, r, cleanup, err := openRepo(c)
	if err != nil {
		return err
	}
	defer cleanup()

	snaps, err := e.ListAllSnapshots(context.Background(), r)
	if err != nil {
		return fmt.Errorf("list snapshots: %w", err)
	}
	if len(snaps) == 0 {
		fmt.Println("no snapshots found")
		return nil
	}
	fmt.Printf("%-24s  %-16s  %-8s  %10s  %-40s  %s\n", "STARTED", "HOST", "FILES", "BYTES", "PATH", "ID")
	for _, s := range snaps {
		started := time.Unix(s.Start, 0).Format(time.RFC3339)
		mark := ""
		if s.Errors > 0 || s.Incomplete != "" {
			mark = " !"
		}
		fmt.Printf("%-24s  %-16s  %-8d  %10d  %-40s  %s%s\n", started, s.Host, s.Files, s.Bytes, s.Path, s.ID, mark)
	}
	return nil
}

func runRestore(args []string) error {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	var c repoFlags
	addRepoFlags(fs, &c)
	snapshotID := fs.String("snapshot", "", "snapshot ID to restore (required; see 'list')")
	target := fs.String("target", "", "directory to restore into (required)")
	path := fs.String("path", "", "path inside the snapshot to restore; empty restores everything")
	overwrite := fs.Bool("overwrite", false, "allow restoring into a non-empty target")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *snapshotID == "" {
		return errors.New("-snapshot is required")
	}
	cleanTarget, err := guard.RestoreTarget(*target)
	if err != nil {
		return err
	}

	e, r, cleanup, err := openRepo(c)
	if err != nil {
		return err
	}
	defer cleanup()

	st, err := e.Restore(context.Background(), r, *snapshotID, *path, cleanTarget, *overwrite)
	if err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	fmt.Printf("restored %d files, %d directories, %d bytes to %s\n", st.Files, st.Dirs, st.Bytes, cleanTarget)
	if st.Skipped > 0 {
		fmt.Printf("skipped %d existing entries (use -overwrite to replace them)\n", st.Skipped)
	}
	for _, w := range st.Warnings {
		fmt.Println("warning:", w)
	}
	return nil
}
