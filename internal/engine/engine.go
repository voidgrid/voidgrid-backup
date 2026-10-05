// Package engine is the thin layer over Kopia that agents use to create
// repositories, take snapshots, apply retention, browse and restore.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	kfs "github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/fs/localfs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/filesystem"
	"github.com/kopia/kopia/repo/blob/s3"
	"github.com/kopia/kopia/repo/blob/sftp"
	"github.com/kopia/kopia/repo/content"
	"github.com/kopia/kopia/repo/maintenance"
	"github.com/kopia/kopia/repo/manifest"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/policy"
	"github.com/kopia/kopia/snapshot/restore"
	"github.com/kopia/kopia/snapshot/snapshotfs"
	"github.com/kopia/kopia/snapshot/snapshotmaintenance"
	"github.com/kopia/kopia/snapshot/upload"

	"github.com/voidgrid/voidgrid-backup/internal/caps"
	"github.com/voidgrid/voidgrid-backup/internal/guard"
	"github.com/voidgrid/voidgrid-backup/internal/repocfg"
)

// Username is the Kopia user recorded on every snapshot this tool takes.
const Username = "voidgrid-backup"

// readOnly is guard.ReadOnly, swappable in tests.
var readOnly = guard.ReadOnly

// eccOverheadPercent is the Reed-Solomon space overhead when ECC is on.
const eccOverheadPercent = 2

// Repo identifies a repository and carries everything needed to open it.
type Repo struct {
	ID       string
	Config   repocfg.Config
	Password string
}

type Retention struct {
	Latest, Hourly, Daily, Weekly, Monthly, Annual int
}

type PathResult struct {
	Path       string
	SnapshotID string
	Bytes      int64
	Files      int
	Errors     int
	Pruned     int
	PrunedIDs  []string // snapshots retention deleted as part of this backup
	Start, End int64    // Unix seconds
	Incomplete string   // why the snapshot is incomplete, "" if complete
	Err        error    // the path was not backed up
	Warnings   []string // backed up, but something needs attention
}

type Snapshot struct {
	ID         string
	Path       string
	Start, End int64 // unix seconds
	Bytes      int64
	Files      int
	Errors     int
	Incomplete string
}

type DirEntry struct {
	Name    string
	Dir     bool
	Size    int64
	ModUnix int64
	Mode    uint32
}

type RestoreStats struct {
	Bytes                int64
	Files, Dirs, Skipped int
	Warnings             []string
}

// Engine keeps a Kopia connection config and cache per repository under dir.
type Engine struct {
	dir      string
	hostname string
	// canChown: the process can restore ownership and modes of files it
	// does not own (CAP_CHOWN + CAP_FOWNER, i.e. root in practice).
	canChown bool
	// Recovery mode (the standalone restore tool): see every host, never write.
	anyHost, readOnly bool
	uid               int
}

func New(dir, hostname string) (*Engine, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	e := &Engine{dir: abs, hostname: hostname, uid: os.Geteuid()}
	if c, err := caps.Current(); err == nil {
		e.canChown = c.Has(caps.Chown) && c.Has(caps.FOwner)
	}
	return e, nil
}

// Init creates the repository if the storage is empty, or connects to the
// one already there. For SFTP without known_hosts it trusts the host key it
// sees and returns it so the server can pin it from then on.
func (e *Engine) Init(ctx context.Context, r Repo) (created bool, knownHosts string, err error) {
	if err := r.Config.Validate(); err != nil {
		return false, "", err
	}
	if s := r.Config.SFTP; s != nil && s.KnownHosts == "" {
		if s.KnownHosts, err = FetchHostKey(ctx, s.Host, s.Port); err != nil {
			return false, "", err
		}
		knownHosts = s.KnownHosts
	}
	st, err := storage(ctx, r.Config, true)
	if err != nil {
		return false, "", err
	}
	defer st.Close(ctx)

	opts := &repo.NewRepositoryOptions{}
	if r.Config.ECC {
		opts.BlockFormat.ECCOverheadPercent = eccOverheadPercent
	}
	switch err := repo.Initialize(ctx, st, opts, r.Password); {
	case err == nil:
		created = true
	case errors.Is(err, repo.ErrAlreadyInitialized):
	default:
		return false, "", fmt.Errorf("initialize repository: %w", err)
	}
	rep, err := e.open(ctx, r)
	if err != nil {
		return created, knownHosts, err
	}
	defer rep.Close(ctx)
	if created {
		if err := claimMaintenance(ctx, rep); err != nil {
			return created, knownHosts, fmt.Errorf("set maintenance owner: %w", err)
		}
	}
	return created, knownHosts, nil
}

// Backup snapshots each path, applies retention, then runs maintenance if
// this agent owns it. A failing path does not stop the others.
func (e *Engine) Backup(ctx context.Context, r Repo, paths, excludes []string, keep Retention) ([]PathResult, error) {
	rep, err := e.open(ctx, r)
	if err != nil {
		return nil, err
	}
	defer rep.Close(ctx)

	results := make([]PathResult, 0, len(paths))
	for _, p := range paths {
		results = append(results, e.backupOne(ctx, rep, p, excludes, keep))
	}
	if err := e.maintain(ctx, rep); err != nil && len(results) > 0 {
		results[0].Warnings = append(results[0].Warnings, "repository maintenance: "+err.Error())
	}
	return results, nil
}

func (e *Engine) backupOne(ctx context.Context, rep repo.Repository, p string, excludes []string, keep Retention) PathResult {
	res := PathResult{Path: p}
	path, err := guard.SourcePath(p)
	if err != nil {
		res.Err = err
		return res
	}
	res.Path = path
	entry, err := localfs.NewEntry(path)
	if err != nil {
		res.Err = err
		return res
	}
	rules := append([]string(nil), excludes...)
	if rel, ok := relInside(e.dir, path); ok {
		rules = append(rules, "/"+rel+"/") // never our own Kopia cache
	}
	si := snapshot.SourceInfo{Host: e.hostname, UserName: Username, Path: path}
	return e.takeSnapshot(ctx, rep, si, entry, rules, keep, nil, res)
}

// takeSnapshot uploads entry as a snapshot of si, applies retention and
// fills in res. around, if set, wraps just the upload (for quiescing).
func (e *Engine) takeSnapshot(ctx context.Context, rep repo.Repository, si snapshot.SourceInfo, entry kfs.Entry,
	rules []string, keep Retention, around func(func() error) error, res PathResult) PathResult {
	if around == nil {
		around = func(fn func() error) error { return fn() }
	}
	res.Err = repo.WriteSession(ctx, rep, repo.WriteSessionOptions{Purpose: "voidgrid-backup:backup"},
		func(ctx context.Context, w repo.RepositoryWriter) error {
			pol := &policy.Policy{
				RetentionPolicy: policy.RetentionPolicy{
					KeepLatest:  optInt(keep.Latest),
					KeepHourly:  optInt(keep.Hourly),
					KeepDaily:   optInt(keep.Daily),
					KeepWeekly:  optInt(keep.Weekly),
					KeepMonthly: optInt(keep.Monthly),
					KeepAnnual:  optInt(keep.Annual),
				},
				FilesPolicy:       policy.FilesPolicy{IgnoreRules: rules},
				CompressionPolicy: policy.CompressionPolicy{CompressorName: "zstd"},
			}
			if err := policy.SetPolicy(ctx, w, si, pol); err != nil {
				return fmt.Errorf("set policy: %w", err)
			}
			tree, err := policy.TreeForSource(ctx, w, si)
			if err != nil {
				return err
			}
			prev, err := snapshot.FindPreviousManifests(ctx, w, si, nil)
			if err != nil {
				return err
			}
			var man *snapshot.Manifest
			if err := around(func() error {
				var err error
				man, err = upload.NewUploader(w).Upload(ctx, entry, tree, si, prev...)
				return err
			}); err != nil {
				return fmt.Errorf("upload: %w", err)
			}
			id, err := snapshot.SaveSnapshot(ctx, w, man)
			if err != nil {
				return fmt.Errorf("save snapshot: %w", err)
			}
			res.SnapshotID = string(id)
			res.Bytes = man.Stats.TotalFileSize
			res.Files = int(man.Stats.TotalFileCount)
			res.Errors = int(man.Stats.ErrorCount)
			res.Start, res.End = man.StartTime.ToTime().Unix(), man.EndTime.ToTime().Unix()
			res.Incomplete = man.IncompleteReason
			if man.RootEntry != nil && man.RootEntry.DirSummary != nil {
				failed := man.RootEntry.DirSummary.FailedEntries
				for i, fe := range failed {
					if i == 5 {
						res.Warnings = append(res.Warnings, fmt.Sprintf("... and %d more unreadable entries", res.Errors-5))
						break
					}
					res.Warnings = append(res.Warnings, "could not read "+fe.EntryPath+": "+fe.Error)
				}
			}
			deleted, err := policy.ApplyRetentionPolicy(ctx, w, si, true)
			if err != nil {
				return fmt.Errorf("apply retention: %w", err)
			}
			res.Pruned = len(deleted)
			for _, d := range deleted {
				res.PrunedIDs = append(res.PrunedIDs, string(d))
			}
			return nil
		})
	return res
}

// maintain runs Kopia's scheduled maintenance (compaction, garbage
// collection). Only the repository's maintenance owner does it: the agent
// that created the repository, or the first agent to back up to a
// repository that has no owner yet. Other agents skip it.
func (e *Engine) maintain(ctx context.Context, rep repo.Repository) error {
	dr, ok := rep.(repo.DirectRepository)
	if !ok {
		return nil
	}
	if err := claimMaintenance(ctx, rep); err != nil {
		return err
	}
	err := repo.DirectWriteSession(ctx, dr, repo.WriteSessionOptions{Purpose: "voidgrid-backup:maintenance"},
		func(ctx context.Context, dw repo.DirectRepositoryWriter) error {
			return snapshotmaintenance.Run(ctx, dw, maintenance.ModeAuto, false, maintenance.SafetyFull)
		})
	var notOwner maintenance.NotOwnedError
	if errors.As(err, &notOwner) {
		return nil
	}
	return err
}

// claimMaintenance makes this client the maintenance owner if there is none.
func claimMaintenance(ctx context.Context, rep repo.Repository) error {
	p, err := maintenance.GetParams(ctx, rep)
	if err != nil {
		return err
	}
	if p.Owner != "" {
		return nil
	}
	def := maintenance.DefaultParams()
	def.Owner = rep.ClientOptions().UsernameAtHost()
	return repo.WriteSession(ctx, rep, repo.WriteSessionOptions{Purpose: "voidgrid-backup:claim-maintenance"},
		func(ctx context.Context, w repo.RepositoryWriter) error {
			return maintenance.SetParams(ctx, w, &def)
		})
}

// ListSnapshots returns this host's snapshots of the given paths (all of
// this host's snapshots when paths is empty), newest first.
func (e *Engine) ListSnapshots(ctx context.Context, r Repo, paths []string) ([]Snapshot, error) {
	rep, err := e.open(ctx, r)
	if err != nil {
		return nil, err
	}
	defer rep.Close(ctx)

	var sources []snapshot.SourceInfo
	if len(paths) == 0 {
		all, err := snapshot.ListSources(ctx, rep)
		if err != nil {
			return nil, err
		}
		for _, s := range all {
			if s.Host == e.hostname && s.UserName == Username {
				sources = append(sources, s)
			}
		}
	} else {
		for _, p := range paths {
			path, err := guard.SourcePath(p)
			if err != nil {
				return nil, err
			}
			sources = append(sources, snapshot.SourceInfo{Host: e.hostname, UserName: Username, Path: path})
		}
	}

	var out []Snapshot
	for _, si := range sources {
		mans, err := snapshot.ListSnapshots(ctx, rep, si)
		if err != nil {
			return nil, err
		}
		for _, m := range mans {
			out = append(out, Snapshot{
				ID:         string(m.ID),
				Path:       m.Source.Path,
				Start:      m.StartTime.ToTime().Unix(),
				End:        m.EndTime.ToTime().Unix(),
				Bytes:      m.Stats.TotalFileSize,
				Files:      int(m.Stats.TotalFileCount),
				Errors:     int(m.Stats.ErrorCount),
				Incomplete: m.IncompleteReason,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start > out[j].Start })
	return out, nil
}

// ListDirectory lists rel (slash-separated, relative to the snapshot root).
func (e *Engine) ListDirectory(ctx context.Context, r Repo, snapshotID, rel string) ([]DirEntry, error) {
	rep, err := e.open(ctx, r)
	if err != nil {
		return nil, err
	}
	defer rep.Close(ctx)

	entry, err := e.snapshotEntry(ctx, rep, snapshotID, rel)
	if err != nil {
		return nil, err
	}
	dir, ok := entry.(kfs.Directory)
	if !ok {
		return nil, fmt.Errorf("%q is not a directory", rel)
	}
	entries, err := kfs.GetAllEntries(ctx, dir)
	if err != nil {
		return nil, err
	}
	out := make([]DirEntry, 0, len(entries))
	for _, ent := range entries {
		out = append(out, DirEntry{
			Name:    ent.Name(),
			Dir:     ent.IsDir(),
			Size:    ent.Size(),
			ModUnix: ent.ModTime().Unix(),
			Mode:    uint32(ent.Mode()),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// Restore writes rel from the snapshot into target on this host. Without
// overwrite the target must be missing or an empty directory, so a restore
// never half-completes over existing data.
func (e *Engine) Restore(ctx context.Context, r Repo, snapshotID, rel, target string, overwrite bool) (RestoreStats, error) {
	target, err := guard.RestoreTarget(target)
	if err != nil {
		return RestoreStats{}, err
	}
	if !overwrite {
		if empty, err := emptyOrMissing(target); err != nil {
			return RestoreStats{}, err
		} else if !empty {
			return RestoreStats{}, fmt.Errorf("restore target %s is not empty; pick an empty directory or enable overwrite", target)
		}
	}
	rep, err := e.open(ctx, r)
	if err != nil {
		return RestoreStats{}, err
	}
	defer rep.Close(ctx)

	entry, err := e.snapshotEntry(ctx, rep, snapshotID, rel)
	if err != nil {
		return RestoreStats{}, err
	}
	return e.restoreTo(ctx, rep, entry, target, overwrite)
}

func (e *Engine) restoreTo(ctx context.Context, rep repo.Repository, entry kfs.Entry, target string, overwrite bool) (RestoreStats, error) {
	if ro, err := readOnly(target); err != nil {
		return RestoreStats{}, fmt.Errorf("restore target %s: %w", target, err)
	} else if ro {
		return RestoreStats{}, fmt.Errorf("%s is mounted read-only in the agent; restore to another directory instead", target)
	}
	out := &restore.FilesystemOutput{
		TargetPath:           target,
		OverwriteDirectories: true,
		OverwriteFiles:       overwrite,
		OverwriteSymlinks:    overwrite,
		// With the capabilities, a failed chown/chmod is a real error. Without
		// them, ownership is skipped on purpose and reported below.
		SkipOwners:             !e.canChown,
		IgnorePermissionErrors: !e.canChown,
		WriteFilesAtomically:   true,
		WriteSparseFiles:       true,
	}
	if err := out.Init(ctx); err != nil {
		return RestoreStats{}, err
	}
	st, err := restore.Entry(ctx, rep, out, entry, restore.Options{
		// 0 would mean a shallow restore (placeholders instead of files).
		RestoreDirEntryAtDepth: math.MaxInt32,
	})
	if err != nil {
		return RestoreStats{}, err
	}
	rs := RestoreStats{
		Bytes:   st.RestoredTotalFileSize,
		Files:   int(st.RestoredFileCount),
		Dirs:    int(st.RestoredDirCount),
		Skipped: int(st.SkippedCount),
	}
	if !e.canChown {
		rs.Warnings = append(rs.Warnings, fmt.Sprintf(
			"file ownership was not restored: the agent runs as uid %d without CAP_CHOWN/CAP_FOWNER, so restored files belong to it; run the agent as root", e.uid))
	}
	return rs, nil
}

func (e *Engine) snapshotEntry(ctx context.Context, rep repo.Repository, snapshotID, rel string) (kfs.Entry, error) {
	man, err := snapshot.LoadSnapshot(ctx, rep, manifest.ID(snapshotID))
	if err != nil {
		return nil, fmt.Errorf("load snapshot %s: %w", snapshotID, err)
	}
	if !e.anyHost && man.Source.Host != e.hostname {
		return nil, fmt.Errorf("snapshot %s belongs to host %s", snapshotID, man.Source.Host)
	}
	entry, err := snapshotfs.SnapshotRoot(rep, man)
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(strings.Trim(rel, "/"), "/") {
		if part == "" {
			continue
		}
		if part == "." || part == ".." {
			return nil, fmt.Errorf("invalid path %q", rel)
		}
		dir, ok := entry.(kfs.Directory)
		if !ok {
			return nil, fmt.Errorf("%q is not a directory", rel)
		}
		if entry, err = dir.Child(ctx, part); err != nil {
			return nil, fmt.Errorf("%q: %w", rel, err)
		}
	}
	return entry, nil
}

// open connects on first use (writing a Kopia config under the engine dir,
// keyed by repository ID and config fingerprint) and opens the repository.
func (e *Engine) open(ctx context.Context, r Repo) (repo.Repository, error) {
	if err := r.Config.Validate(); err != nil {
		return nil, err
	}
	dir := filepath.Join(e.dir, r.ID+"-"+r.Config.Fingerprint())
	cfgFile := filepath.Join(dir, "repository.config")
	if _, err := os.Stat(cfgFile); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		st, err := storage(ctx, r.Config, false)
		if err != nil {
			return nil, err
		}
		err = repo.Connect(ctx, cfgFile, st, r.Password, &repo.ConnectOptions{
			ClientOptions:  repo.ClientOptions{Hostname: e.hostname, Username: Username, ReadOnly: e.readOnly},
			CachingOptions: content.CachingOptions{CacheDirectory: filepath.Join(dir, "cache")},
		})
		st.Close(ctx)
		if err != nil {
			return nil, fmt.Errorf("connect to repository: %w", err)
		}
	} else if err != nil {
		return nil, err
	}
	rep, err := repo.Open(ctx, cfgFile, r.Password, &repo.Options{})
	if err != nil {
		return nil, fmt.Errorf("open repository: %w", err)
	}
	return rep, nil
}

func storage(ctx context.Context, c repocfg.Config, create bool) (blob.Storage, error) {
	switch c.Kind {
	case repocfg.KindSFTP:
		s := c.SFTP
		return sftp.New(ctx, &sftp.Options{
			Path:           s.Path,
			Host:           s.Host,
			Port:           s.Port,
			Username:       s.User,
			Password:       s.Password,
			Keyfile:        s.KeyFile,
			KnownHostsData: s.KnownHosts,
		}, create)
	case repocfg.KindS3:
		s := c.S3
		return s3.New(ctx, &s3.Options{
			BucketName:      s.Bucket,
			Prefix:          s.Prefix,
			Endpoint:        s.Endpoint,
			Region:          s.Region,
			AccessKeyID:     s.AccessKeyID,
			SecretAccessKey: s.SecretAccessKey,
			DoNotUseTLS:     s.Insecure,
		}, create)
	case repocfg.KindFilesystem:
		if create {
			if err := os.MkdirAll(c.Filesystem.Path, 0o700); err != nil {
				return nil, err
			}
		}
		return filesystem.New(ctx, &filesystem.Options{Path: c.Filesystem.Path}, create)
	}
	return nil, fmt.Errorf("unknown repository kind %q", c.Kind)
}

func emptyOrMissing(dir string) (bool, error) {
	f, err := os.Open(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close() //nolint:errcheck // read-only directory handle
	names, err := f.Readdirnames(1)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("restore target %s: %w", dir, err)
	}
	return len(names) == 0, nil
}

func optInt(n int) *policy.OptionalInt {
	v := policy.OptionalInt(n)
	return &v
}
