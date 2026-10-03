package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	kfs "github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/fs/localfs"
	"github.com/kopia/kopia/fs/virtualfs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot"

	"github.com/voidgrid/voidgrid-backup/internal/docker"
	"github.com/voidgrid/voidgrid-backup/internal/guard"
)

// Quiesce modes for the stack's application containers while files are read.
const (
	QuiesceNone  = "none"
	QuiescePause = "pause"
	QuiesceStop  = "stop"
)

// Names inside a stack snapshot.
const (
	stackDirName      = "stack"
	mountsDirName     = "mounts"
	dumpsDirName      = "dumps"
	stackManifestName = "voidgrid-backup.json"
	// dumpsRestoreDir holds dumps restored to the original location, inside
	// the stack directory, ready for ImportDump or manual use.
	dumpsRestoreDir = ".voidgrid-backup-dumps"
)

var projectPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

// StackSpec says what to back up for one compose project.
type StackSpec struct {
	Project    string
	WorkingDir string
	Include    []string          // host paths of mounts to back up
	Exclude    []string          // host paths of mounts to leave out
	Excludes   []string          // gitignore-style rules applied to everything
	Dumps      map[string]string // service -> dump kind
	Quiesce    string
}

// StackManifest is stored in every stack snapshot so it can be restored
// to where it came from.
type StackManifest struct {
	Version    int               `json:"version"`
	Project    string            `json:"project"`
	WorkingDir string            `json:"working_dir"`
	Mounts     map[string]string `json:"mounts"` // entry under mounts/ -> host path
	Dumps      map[string]string `json:"dumps"`  // service -> dump kind
}

// StackSourcePath is the Kopia source path for a project's snapshots.
func StackSourcePath(project string) string { return "/stacks/" + project }

// BackupStack snapshots a compose project: its directory, the chosen
// mounts, and database dumps streamed from the database containers.
func (e *Engine) BackupStack(ctx context.Context, r Repo, dk *docker.Client, spec StackSpec, keep Retention) (PathResult, error) {
	rep, err := e.open(ctx, r)
	if err != nil {
		return PathResult{}, err
	}
	defer rep.Close(ctx)
	res := e.backupStack(ctx, rep, dk, spec, keep)
	if err := e.maintain(ctx, rep); err != nil {
		res.Warnings = append(res.Warnings, "repository maintenance: "+err.Error())
	}
	return res, nil
}

func (e *Engine) backupStack(ctx context.Context, rep repo.Repository, dk *docker.Client, spec StackSpec, keep Retention) PathResult {
	res := PathResult{Path: StackSourcePath(spec.Project)}
	fail := func(err error) PathResult { res.Err = err; return res }

	if !projectPattern.MatchString(spec.Project) {
		return fail(fmt.Errorf("invalid project name %q", spec.Project))
	}
	workdir, err := guard.SourcePath(spec.WorkingDir)
	if err != nil {
		return fail(err)
	}
	switch spec.Quiesce {
	case "", QuiesceNone, QuiescePause, QuiesceStop:
	default:
		return fail(fmt.Errorf("unknown quiesce mode %q", spec.Quiesce))
	}
	stack, found, err := dk.FindStack(ctx, spec.Project)
	if err != nil {
		return fail(err)
	}
	if !found && (len(spec.Dumps) > 0 || spec.Quiesce == QuiescePause || spec.Quiesce == QuiesceStop) {
		res.Warnings = append(res.Warnings, "no containers found for project "+spec.Project+"; backing up files only")
	}

	man := StackManifest{Version: 1, Project: spec.Project, WorkingDir: workdir,
		Mounts: map[string]string{}, Dumps: map[string]string{}}
	rules := append([]string(nil), spec.Excludes...)

	stackDir, err := localfs.Directory(workdir)
	if err != nil {
		return fail(err)
	}
	entries := []kfs.Entry{renamedDir{stackDir, stackDirName}}
	if rel, ok := relInside(e.dir, workdir); ok {
		rules = append(rules, "/"+stackDirName+"/"+rel+"/") // never our own Kopia cache
	}
	for _, p := range spec.Exclude {
		p, err := guard.SourcePath(p)
		if err != nil {
			return fail(err)
		}
		if rel, ok := relInside(p, workdir); ok {
			rules = append(rules, "/"+stackDirName+"/"+rel+dirSuffix(p))
		}
	}

	var mountEntries []kfs.Entry
	for _, p := range spec.Include {
		p, err := guard.SourcePath(p)
		if err != nil {
			return fail(err)
		}
		if guard.Within(p, workdir) {
			continue // already inside stack/
		}
		name := mountName(p)
		ent, err := localfs.NewEntry(p)
		if err != nil {
			return fail(fmt.Errorf("mount %s: %w", p, err))
		}
		switch v := ent.(type) {
		case kfs.Directory:
			mountEntries = append(mountEntries, renamedDir{v, name})
		case kfs.File:
			mountEntries = append(mountEntries, renamedFile{v, name})
		default:
			return fail(fmt.Errorf("mount %s is neither a file nor a directory", p))
		}
		man.Mounts[name] = p
		if rel, ok := relInside(e.dir, p); ok {
			rules = append(rules, "/"+mountsDirName+"/"+name+"/"+rel+"/")
		}
	}
	if len(mountEntries) > 0 {
		entries = append(entries, virtualfs.NewStaticDirectory(mountsDirName, mountEntries))
	}

	var dumpErrs errorList
	var dumpEntries []kfs.Entry
	for _, svcName := range sortedKeys(spec.Dumps) {
		kind := spec.Dumps[svcName]
		cmd, ext, ok := docker.DumpCommand(kind)
		if !ok {
			return fail(fmt.Errorf("service %s: unknown dump kind %q", svcName, kind))
		}
		svc, ok := findService(stack, svcName)
		if !ok || svc.State != "running" {
			res.Warnings = append(res.Warnings, fmt.Sprintf("dump of %s skipped: container is not running", svcName))
			continue
		}
		man.Dumps[svcName] = kind
		id, name := svc.ContainerID, svcName
		dumpEntries = append(dumpEntries, lazyStream{
			StreamingFile: virtualfs.StreamingFileFromReader(svcName+ext, io.NopCloser(strings.NewReader(""))),
			open: func(ctx context.Context) (io.ReadCloser, error) {
				rc, err := dk.Exec(ctx, id, cmd, nil, nil)
				if err != nil {
					dumpErrs.add(fmt.Errorf("dump of %s: %w", name, err))
					return nil, err
				}
				return &recordErrors{ReadCloser: rc, label: "dump of " + name, errs: &dumpErrs}, nil
			},
		})
	}
	if len(dumpEntries) > 0 {
		entries = append(entries, virtualfs.NewStaticDirectory(dumpsDirName, dumpEntries))
	}

	mj, _ := json.MarshalIndent(man, "", "  ")
	entries = append(entries, virtualfs.StreamingFileFromReader(stackManifestName, io.NopCloser(bytes.NewReader(mj))))
	root := virtualfs.NewStaticDirectory(spec.Project, entries)

	quiesce := func(upload func() error) error { return upload() }
	if found && (spec.Quiesce == QuiescePause || spec.Quiesce == QuiesceStop) {
		quiesce = func(upload func() error) error {
			return withQuiesced(ctx, dk, stack, spec.Dumps, spec.Quiesce, upload)
		}
	}

	si := snapshot.SourceInfo{Host: e.hostname, UserName: Username, Path: res.Path}
	res = e.takeSnapshot(ctx, rep, si, root, rules, keep, quiesce, res)
	// Failed dumps are reported below with their own wording.
	res.Warnings = slices.DeleteFunc(res.Warnings, func(w string) bool {
		return strings.HasPrefix(w, "could not read "+dumpsDirName+"/")
	})
	for _, err := range dumpErrs.list() {
		res.Warnings = append(res.Warnings, err.Error())
	}
	return res
}

// withQuiesced pauses or stops the stack's running application containers
// (not the ones being dumped, and never this agent), runs fn, and always
// resumes what it touched.
func withQuiesced(ctx context.Context, dk *docker.Client, st docker.Stack, dumps map[string]string, mode string, fn func() error) error {
	var touched []string
	resume := func() error {
		var errs []error
		for i := len(touched) - 1; i >= 0; i-- {
			var err error
			if mode == QuiescePause {
				err = dk.Unpause(context.WithoutCancel(ctx), touched[i])
			} else {
				err = dk.Start(context.WithoutCancel(ctx), touched[i])
			}
			if err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}
	for _, svc := range st.Services {
		if _, dumped := dumps[svc.Name]; dumped || svc.State != "running" || isSelf(svc) {
			continue
		}
		var err error
		if mode == QuiescePause {
			err = dk.Pause(ctx, svc.ContainerID)
		} else {
			err = dk.Stop(ctx, svc.ContainerID)
		}
		if err != nil {
			return errors.Join(fmt.Errorf("%s %s: %w", mode, svc.Name, err), resume())
		}
		touched = append(touched, svc.ContainerID)
	}
	err := fn()
	if rerr := resume(); rerr != nil {
		return errors.Join(err, fmt.Errorf("resuming containers: %w", rerr))
	}
	return err
}

// isSelf keeps the agent from pausing or stopping itself when it is part
// of the stack being backed up.
func isSelf(svc docker.Service) bool {
	return strings.Contains(svc.Image, "voidgrid-backup")
}

// RestoreStack restores a stack snapshot. With targetRoot "" everything goes
// back to its original location (stack dir, mounts; dumps into the stack
// dir's .voidgrid-backup-dumps), optionally stopping the stack meanwhile.
// Otherwise the snapshot tree is written under targetRoot, which must be
// empty, and no containers are touched.
func (e *Engine) RestoreStack(ctx context.Context, r Repo, dk *docker.Client, snapshotID, targetRoot string, stopStack bool) (RestoreStats, error) {
	rep, err := e.open(ctx, r)
	if err != nil {
		return RestoreStats{}, err
	}
	defer rep.Close(ctx)

	root, err := e.snapshotEntry(ctx, rep, snapshotID, "")
	if err != nil {
		return RestoreStats{}, err
	}
	man, err := readStackManifest(ctx, root)
	if err != nil {
		return RestoreStats{}, err
	}

	if targetRoot != "" {
		target, err := guard.RestoreTarget(targetRoot)
		if err != nil {
			return RestoreStats{}, err
		}
		if empty, err := emptyOrMissing(target); err != nil {
			return RestoreStats{}, err
		} else if !empty {
			return RestoreStats{}, fmt.Errorf("restore target %s is not empty", target)
		}
		return e.restoreTo(ctx, rep, root, target, false)
	}

	// Original locations: validate every destination before touching anything.
	workdir, err := guard.RestoreTarget(man.WorkingDir)
	if err != nil {
		return RestoreStats{}, err
	}
	type job struct {
		rel, target string
	}
	jobs := []job{{stackDirName, workdir}}
	for _, name := range sortedKeys(man.Mounts) {
		t, err := guard.RestoreTarget(man.Mounts[name])
		if err != nil {
			return RestoreStats{}, err
		}
		jobs = append(jobs, job{mountsDirName + "/" + name, t})
	}
	if len(man.Dumps) > 0 {
		jobs = append(jobs, job{dumpsDirName, filepath.Join(workdir, dumpsRestoreDir)})
	}

	run := func() (RestoreStats, error) {
		var total RestoreStats
		for _, j := range jobs {
			ent, err := childEntry(ctx, root, j.rel)
			if err != nil {
				return total, err
			}
			st, err := e.restoreTo(ctx, rep, ent, j.target, true)
			total.add(st)
			if err != nil {
				return total, fmt.Errorf("restore %s to %s: %w", j.rel, j.target, err)
			}
		}
		return total, nil
	}
	if !stopStack {
		return run()
	}
	st, found, err := dk.FindStack(ctx, man.Project)
	if err != nil {
		return RestoreStats{}, err
	}
	if !found {
		return run()
	}
	var stats RestoreStats
	err = withQuiesced(ctx, dk, st, nil, QuiesceStop, func() error {
		var err error
		stats, err = run()
		return err
	})
	return stats, err
}

// ImportDump loads one service's dump from a stack snapshot into that
// service's running container. It returns the tail of the import output.
func (e *Engine) ImportDump(ctx context.Context, r Repo, dk *docker.Client, snapshotID, service string) (string, error) {
	rep, err := e.open(ctx, r)
	if err != nil {
		return "", err
	}
	defer rep.Close(ctx)

	root, err := e.snapshotEntry(ctx, rep, snapshotID, "")
	if err != nil {
		return "", err
	}
	man, err := readStackManifest(ctx, root)
	if err != nil {
		return "", err
	}
	kind, ok := man.Dumps[service]
	if !ok {
		return "", fmt.Errorf("snapshot has no dump for service %s", service)
	}
	_, ext, _ := docker.DumpCommand(kind)
	cmd, ok := docker.ImportCommand(kind)
	if !ok {
		return "", fmt.Errorf("%s dumps can't be imported into a running server: restore the snapshot, stop %s, "+
			"copy %s/%s%s over its data file (dump.rdb) and start it again", kind, service, dumpsRestoreDir, service, ext)
	}
	st, found, err := dk.FindStack(ctx, man.Project)
	if err != nil {
		return "", err
	}
	svc, ok := findService(st, service)
	if !found || !ok || svc.State != "running" {
		return "", fmt.Errorf("service %s of %s is not running", service, man.Project)
	}

	ent, err := childEntry(ctx, root, dumpsDirName+"/"+service+ext)
	if err != nil {
		return "", err
	}
	f, ok := ent.(kfs.File)
	if !ok {
		return "", fmt.Errorf("dump %s is not a file", ent.Name())
	}
	in, err := f.Open(ctx)
	if err != nil {
		return "", err
	}
	defer in.Close()
	rc, err := dk.Exec(ctx, svc.ContainerID, cmd, nil, in)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	var out tail
	_, err = io.Copy(&out, rc)
	return out.String(), err
}

func readStackManifest(ctx context.Context, root kfs.Entry) (StackManifest, error) {
	var man StackManifest
	ent, err := childEntry(ctx, root, stackManifestName)
	if err != nil {
		return man, fmt.Errorf("not a stack snapshot: %w", err)
	}
	f, ok := ent.(kfs.File)
	if !ok {
		return man, errors.New("not a stack snapshot")
	}
	rd, err := f.Open(ctx)
	if err != nil {
		return man, err
	}
	defer rd.Close()
	if err := json.NewDecoder(io.LimitReader(rd, 1<<20)).Decode(&man); err != nil {
		return man, fmt.Errorf("stack manifest: %w", err)
	}
	if !projectPattern.MatchString(man.Project) {
		return man, fmt.Errorf("stack manifest has invalid project %q", man.Project)
	}
	return man, nil
}

func childEntry(ctx context.Context, root kfs.Entry, rel string) (kfs.Entry, error) {
	ent := root
	for _, part := range strings.Split(rel, "/") {
		dir, ok := ent.(kfs.Directory)
		if !ok {
			return nil, fmt.Errorf("%s: not a directory", rel)
		}
		var err error
		if ent, err = dir.Child(ctx, part); err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
	}
	return ent, nil
}

func findService(st docker.Stack, name string) (docker.Service, bool) {
	for _, s := range st.Services {
		if s.Name == name {
			return s, true
		}
	}
	return docker.Service{}, false
}

// mountName flattens a host path into one directory name under mounts/.
func mountName(p string) string {
	return strings.ReplaceAll(strings.TrimPrefix(p, "/"), "/", "__")
}

// relInside returns p relative to dir when p is strictly inside dir.
func relInside(p, dir string) (string, bool) {
	if p == dir || !guard.Within(p, dir) {
		return "", false
	}
	return filepath.ToSlash(strings.TrimPrefix(p, dir+"/")), true
}

func dirSuffix(p string) string {
	if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
		return ""
	}
	return "/"
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// renamedDir and renamedFile present a local entry under another name.
type renamedDir struct {
	kfs.Directory
	name string
}

func (r renamedDir) Name() string { return r.name }

type renamedFile struct {
	kfs.File
	name string
}

func (r renamedFile) Name() string { return r.name }

// lazyStream starts its source only when Kopia reads it, so a database dump
// isn't held open while unrelated files upload.
type lazyStream struct {
	kfs.StreamingFile
	open func(context.Context) (io.ReadCloser, error)
}

func (l lazyStream) GetReader(ctx context.Context) (io.ReadCloser, error) { return l.open(ctx) }

// recordErrors notes a failing stream (e.g. a dump command's non-zero exit)
// so the run can report it, and still passes the error on to Kopia.
type recordErrors struct {
	io.ReadCloser
	label string
	errs  *errorList
	once  sync.Once
}

func (r *recordErrors) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		r.once.Do(func() { r.errs.add(fmt.Errorf("%s: %w", r.label, err)) })
	}
	return n, err
}

type errorList struct {
	mu   sync.Mutex
	errs []error
}

func (l *errorList) add(err error) {
	l.mu.Lock()
	l.errs = append(l.errs, err)
	l.mu.Unlock()
}

func (l *errorList) list() []error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]error(nil), l.errs...)
}

// tail keeps the last 4 KiB written.
type tail struct{ b []byte }

func (t *tail) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 4096 {
		t.b = t.b[len(t.b)-4096:]
	}
	return len(p), nil
}

func (t *tail) String() string { return strings.TrimSpace(string(t.b)) }

func (s *RestoreStats) add(o RestoreStats) {
	s.Bytes += o.Bytes
	s.Files += o.Files
	s.Dirs += o.Dirs
	s.Skipped += o.Skipped
	for _, w := range o.Warnings {
		if !slices.Contains(s.Warnings, w) {
			s.Warnings = append(s.Warnings, w)
		}
	}
}
