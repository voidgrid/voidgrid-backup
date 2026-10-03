package server

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
	"github.com/voidgrid/voidgrid-backup/internal/guard"
	"github.com/voidgrid/voidgrid-backup/internal/notify"
	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
	"github.com/voidgrid/voidgrid-backup/internal/repocfg"
)

// Operation limits. Backups and restores are detached from the HTTP request
// that started them.
const (
	initTimeout   = 2 * time.Minute
	browseTimeout = 2 * time.Minute
	runTimeout    = 24 * time.Hour
)

// Check defaults. A full byte-for-byte verify of a large repository is
// expensive to run on demand, so this checks all snapshot metadata and blob
// existence (always done) plus a spot check of file contents, and test
// restores small sources in full.
const (
	checkVerifyPercent       = 10
	checkTestRestoreMaxBytes = 500 << 20 // 500 MiB
)

var (
	ErrRunning        = errors.New("this job is already running")
	ErrNotInitialized = errors.New("repository has not been initialized")
)

// running tracks jobs with a backup or restore in flight.
type running struct {
	mu   sync.Mutex
	jobs map[string]bool
}

func (r *running) start(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.jobs == nil {
		r.jobs = map[string]bool{}
	}
	if r.jobs[id] {
		return false
	}
	r.jobs[id] = true
	return true
}

func (r *running) done(id string) {
	r.mu.Lock()
	delete(r.jobs, id)
	r.mu.Unlock()
}

func (r *running) is(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.jobs[id]
}

// IsRunning reports whether a backup or restore of the job is in flight.
func (c *Controller) IsRunning(jobID string) bool { return c.inflight.is(jobID) }

// AddRepository records a repository and has agentID create it (or connect
// to an existing one). With no password a random one is generated and
// returned: it is the only key to the data and must be recorded.
func (c *Controller) AddRepository(ctx context.Context, name string, cfg repocfg.Config, password, agentID string) (catalog.Repository, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return catalog.Repository{}, "", errors.New("name is required")
	}
	if err := cfg.Validate(); err != nil {
		return catalog.Repository{}, "", err
	}
	if _, err := c.Catalog.RepositoryByName(ctx, name); err == nil {
		return catalog.Repository{}, "", errors.New("a repository with that name already exists")
	} else if !errors.Is(err, catalog.ErrNotFound) {
		return catalog.Repository{}, "", err
	}
	agent, err := c.Catalog.Agent(ctx, agentID)
	if err != nil {
		return catalog.Repository{}, "", fmt.Errorf("agent: %w", err)
	}
	generated := ""
	if password == "" {
		if password, err = newPassword(); err != nil {
			return catalog.Repository{}, "", err
		}
		generated = password
	}
	id, err := newAgentID()
	if err != nil {
		return catalog.Repository{}, "", err
	}
	r := catalog.Repository{ID: id, Name: name, Config: cfg, Password: password, CreatedAt: time.Now()}
	if err := c.Catalog.AddRepository(ctx, r); err != nil {
		return catalog.Repository{}, "", err
	}

	resp, err := withAgent(c, agent, initTimeout, func(ctx context.Context, cl agentpb.AgentClient) (*agentpb.InitRepositoryResponse, error) {
		return cl.InitRepository(ctx, &agentpb.InitRepositoryRequest{Repository: toProtoRepo(r)})
	})
	if err != nil {
		if derr := c.Catalog.DeleteRepository(ctx, id); derr != nil {
			slog.Warn("remove failed repository record", "repo", id, "err", derr)
		}
		return catalog.Repository{}, "", fmt.Errorf("initialize on %s: %w", agent.Name, err)
	}
	if resp.GetKnownHosts() != "" && r.Config.SFTP != nil {
		r.Config.SFTP.KnownHosts = resp.GetKnownHosts()
	}
	r.InitializedAt = time.Now()
	if err := c.Catalog.MarkRepositoryInitialized(ctx, id, r.Config, r.InitializedAt); err != nil {
		return catalog.Repository{}, "", err
	}
	return r, generated, nil
}

// JobInput is what the UI or API submits to create a job.
type JobInput struct {
	Name, AgentID, RepositoryID string
	Paths, Excludes             []string
	Stack                       *catalog.StackConfig // set for a stack job instead of Paths
	VM                          *catalog.VMConfig    // set for a VM job instead of Paths
	Schedule                    string
	Keep                        catalog.Retention
	Enabled                     bool
}

// validateJobBody checks and fills in what creating and editing a job share:
// what it backs up (paths, a stack or a VM), the schedule and the retention.
// It sets j.Kind from the shape of in.
func validateJobBody(in JobInput, j *catalog.Job) error {
	var err error
	switch {
	case in.Stack != nil && in.VM != nil:
		return errors.New("a job backs up a stack or a VM, not both")
	case in.Stack != nil:
		j.Kind = catalog.JobStack
		if j.Stack, err = validStack(*in.Stack); err != nil {
			return err
		}
	case in.VM != nil:
		j.Kind = catalog.JobVM
		if j.VM, err = validVM(*in.VM); err != nil {
			return err
		}
	default:
		j.Kind = catalog.JobPaths
		for _, p := range nonEmpty(in.Paths) {
			clean, err := guard.SourcePath(p)
			if err != nil {
				return err
			}
			j.Paths = append(j.Paths, clean)
		}
		if len(j.Paths) == 0 {
			return errors.New("at least one path is required")
		}
	}
	if j.Schedule != "" {
		if _, err := cron.ParseStandard(j.Schedule); err != nil {
			return fmt.Errorf("schedule: %w", err)
		}
	}
	k := j.Keep
	if k.Latest < 0 || k.Hourly < 0 || k.Daily < 0 || k.Weekly < 0 || k.Monthly < 0 || k.Annual < 0 {
		return errors.New("retention counts cannot be negative")
	}
	if k.Latest+k.Hourly+k.Daily+k.Weekly+k.Monthly+k.Annual == 0 {
		return errors.New("retention keeps nothing; set at least one count")
	}
	return nil
}

func (c *Controller) AddJob(ctx context.Context, in JobInput) (catalog.Job, error) {
	j := catalog.Job{
		Name:         strings.TrimSpace(in.Name),
		AgentID:      in.AgentID,
		RepositoryID: in.RepositoryID,
		Excludes:     nonEmpty(in.Excludes),
		Schedule:     strings.TrimSpace(in.Schedule),
		Keep:         in.Keep,
		Enabled:      in.Enabled,
		CreatedAt:    time.Now(),
	}
	if j.Name == "" {
		return catalog.Job{}, errors.New("name is required")
	}
	if _, err := c.Catalog.JobByName(ctx, j.Name); err == nil {
		return catalog.Job{}, errors.New("a job with that name already exists")
	} else if !errors.Is(err, catalog.ErrNotFound) {
		return catalog.Job{}, err
	}
	if _, err := c.Catalog.Agent(ctx, j.AgentID); err != nil {
		return catalog.Job{}, fmt.Errorf("agent: %w", err)
	}
	repo, err := c.Catalog.Repository(ctx, j.RepositoryID)
	if err != nil {
		return catalog.Job{}, fmt.Errorf("repository: %w", err)
	}
	if repo.InitializedAt.IsZero() {
		return catalog.Job{}, ErrNotInitialized
	}
	if err := validateJobBody(in, &j); err != nil {
		return catalog.Job{}, err
	}
	if j.ID, err = newAgentID(); err != nil {
		return catalog.Job{}, err
	}
	return j, c.Catalog.AddJob(ctx, j)
}

// UpdateJob changes what an existing job backs up and how. Name, excludes,
// schedule, retention and enabled can change for any kind; paths for a path
// job, mounts/dumps/quiesce for a stack job, disks/quiesce for a VM job.
// Its kind, agent, repository, stack project and working dir, and VM name are
// fixed: they are what the job's snapshots are of, and in must have the shape
// of the job's kind. A running job is refused, since the edit would apply
// only from its next run while the current one records against the old config.
func (c *Controller) UpdateJob(ctx context.Context, id string, in JobInput) (catalog.Job, error) {
	if !c.inflight.start(id) {
		return catalog.Job{}, ErrRunning
	}
	defer c.inflight.done(id)
	old, err := c.Catalog.Job(ctx, id)
	if err != nil {
		return catalog.Job{}, err
	}
	j := old
	j.Name = strings.TrimSpace(in.Name)
	j.Excludes = nonEmpty(in.Excludes)
	j.Schedule = strings.TrimSpace(in.Schedule)
	j.Keep = in.Keep
	j.Enabled = in.Enabled
	j.Paths, j.Stack, j.VM = nil, nil, nil
	if j.Name == "" {
		return catalog.Job{}, errors.New("name is required")
	}
	if other, err := c.Catalog.JobByName(ctx, j.Name); err == nil && other.ID != id {
		return catalog.Job{}, errors.New("a job with that name already exists")
	} else if err != nil && !errors.Is(err, catalog.ErrNotFound) {
		return catalog.Job{}, err
	}
	switch {
	case in.Stack != nil && old.Stack != nil:
		in.Stack.Project, in.Stack.WorkingDir = old.Stack.Project, old.Stack.WorkingDir
	case in.VM != nil && old.VM != nil:
		in.VM.Name = old.VM.Name
	}
	if err := validateJobBody(in, &j); err != nil {
		return catalog.Job{}, err
	}
	if j.Kind != old.Kind {
		return catalog.Job{}, fmt.Errorf("this is a %s job; its kind can't change", old.Kind)
	}
	return j, c.Catalog.UpdateJob(ctx, j)
}

// RunJob backs up a job now and records the run. It blocks until the backup
// finishes; callers that must not block run it in a goroutine.
func (c *Controller) RunJob(ctx context.Context, jobID, trigger string) (catalog.Run, error) {
	if !c.inflight.start(jobID) {
		return catalog.Run{}, ErrRunning
	}
	defer c.inflight.done(jobID)

	job, repo, agent, err := c.jobContext(ctx, jobID)
	if err != nil {
		return catalog.Run{}, err
	}
	run := catalog.Run{JobID: jobID, Kind: "backup", Trigger: trigger, StartedAt: time.Now()}
	if run.ID, err = c.Catalog.StartRun(ctx, jobID, run.Kind, trigger, run.StartedAt); err != nil {
		return catalog.Run{}, err
	}

	req := &agentpb.BackupRequest{
		Repository: toProtoRepo(repo),
		Paths:      job.Paths,
		Excludes:   job.Excludes,
		Retention: &agentpb.Retention{
			KeepLatest: int32(job.Keep.Latest), KeepHourly: int32(job.Keep.Hourly),
			KeepDaily: int32(job.Keep.Daily), KeepWeekly: int32(job.Keep.Weekly),
			KeepMonthly: int32(job.Keep.Monthly), KeepAnnual: int32(job.Keep.Annual),
		},
	}
	if s := job.Stack; s != nil {
		req.Paths = nil
		req.Stack = &agentpb.StackSpec{Project: s.Project, WorkingDir: s.WorkingDir,
			Include: s.Include, Exclude: s.Exclude, Dumps: s.Dumps, Quiesce: s.Quiesce}
	}
	if v := job.VM; v != nil {
		req.Paths = nil
		req.Vm = &agentpb.VMSpec{Name: v.Name, Disks: v.Disks, Quiesce: v.Quiesce}
	}
	resp, err := withAgent(c, agent, runTimeout, func(ctx context.Context, cl agentpb.AgentClient) (*agentpb.BackupResponse, error) {
		return cl.Backup(ctx, req)
	})
	run.FinishedAt = time.Now()
	if err != nil {
		run.Status, run.Error = catalog.RunFailed, err.Error()
	} else {
		summarizeBackup(&run, resp.GetResults())
		c.recordSnapshots(context.WithoutCancel(ctx), job, resp.GetResults())
	}
	if ferr := c.Catalog.FinishRun(context.WithoutCancel(ctx), run); ferr != nil {
		slog.Error("record run", "job", job.Name, "err", ferr)
	}
	slog.Info("backup finished", "job", job.Name, "status", run.Status, "summary", run.Summary, "err", run.Error)
	c.notifyOnFailure(context.WithoutCancel(ctx), job.Name, run)
	return run, nil
}

// notifyOnFailure sends run through whatever notification channels are
// configured, but only for a scheduled run that didn't fully succeed: a
// manual run is one the operator is already watching in the UI, and a
// success needs no attention. A missing or disabled configuration is a
// silent no-op, not an error.
func (c *Controller) notifyOnFailure(ctx context.Context, jobName string, run catalog.Run) {
	if run.Trigger != "schedule" || run.Status == catalog.RunSuccess {
		return
	}
	cfg, err := c.NotifyConfig(ctx)
	if err != nil {
		slog.Error("load notification settings", "err", err)
		return
	}
	if !cfg.Enabled() {
		return
	}
	ev := notify.Event{Job: jobName, Kind: run.Kind, Status: run.Status, Summary: run.Summary, Error: run.Error, JobID: run.JobID}
	for _, r := range notify.Send(ctx, cfg, ev) {
		if r.Err != nil {
			slog.Error("notify", "job", jobName, "channel", r.Channel, "err", r.Err)
		} else {
			slog.Info("notify", "job", jobName, "channel", r.Channel)
		}
	}
}

func summarizeBackup(run *catalog.Run, results []*agentpb.PathResult) {
	var failed, pruned, fileErrs int
	var errs []string
	for _, r := range results {
		if r.GetError() != "" {
			failed++
			errs = append(errs, r.GetPath()+": "+r.GetError())
			continue
		}
		run.Bytes += r.GetBytes()
		run.Files += int(r.GetFiles())
		pruned += int(r.GetPruned())
		if r.GetErrors() > 0 {
			fileErrs += int(r.GetErrors())
			errs = append(errs, fmt.Sprintf("%s: %d entries could not be read", r.GetPath(), r.GetErrors()))
		}
		for _, w := range r.GetWarnings() {
			fileErrs++
			errs = append(errs, r.GetPath()+": "+w)
		}
	}
	switch {
	case failed == len(results):
		run.Status = catalog.RunFailed
	case failed > 0 || fileErrs > 0:
		run.Status = catalog.RunPartial
	default:
		run.Status = catalog.RunSuccess
	}
	run.Summary = fmt.Sprintf("%d of %d paths, %d files, %s, %d old snapshots pruned",
		len(results)-failed, len(results), run.Files, HumanBytes(run.Bytes), pruned)
	run.Error = strings.Join(errs, "\n")
}

// RunCheck verifies the job's repository: snapshot metadata, blob existence,
// a spot check of file contents, and (with ECC on) repairs bitrot found
// along the way. It blocks until the check finishes and records it as a run
// of the job, sharing the job's inflight lock with backups and restores.
func (c *Controller) RunCheck(ctx context.Context, jobID string) (catalog.Run, error) {
	if !c.inflight.start(jobID) {
		return catalog.Run{}, ErrRunning
	}
	defer c.inflight.done(jobID)

	_, repo, agent, err := c.jobContext(ctx, jobID)
	if err != nil {
		return catalog.Run{}, err
	}
	run := catalog.Run{JobID: jobID, Kind: "check", Trigger: "manual", StartedAt: time.Now()}
	if run.ID, err = c.Catalog.StartRun(ctx, jobID, run.Kind, run.Trigger, run.StartedAt); err != nil {
		return catalog.Run{}, err
	}
	resp, err := withAgent(c, agent, runTimeout, func(ctx context.Context, cl agentpb.AgentClient) (*agentpb.CheckResponse, error) {
		return cl.Check(ctx, &agentpb.CheckRequest{
			Repository:          toProtoRepo(repo),
			VerifyPercent:       checkVerifyPercent,
			TestRestoreMaxBytes: checkTestRestoreMaxBytes,
		})
	})
	run.FinishedAt = time.Now()
	if err != nil {
		run.Status, run.Error = catalog.RunFailed, err.Error()
	} else {
		summarizeCheck(&run, resp)
	}
	if ferr := c.Catalog.FinishRun(context.WithoutCancel(ctx), run); ferr != nil {
		slog.Error("record run", "job", jobID, "err", ferr)
	}
	slog.Info("check finished", "job", jobID, "status", run.Status, "summary", run.Summary, "err", run.Error)
	return run, nil
}

func summarizeCheck(run *catalog.Run, resp *agentpb.CheckResponse) {
	run.Bytes, run.Files = resp.GetBytesRead(), int(resp.GetFilesRead())
	var errs []string
	errs = append(errs, resp.GetErrors()...)
	if resp.GetTestRestoreError() != "" {
		errs = append(errs, "test restore of "+resp.GetTestRestore()+": "+resp.GetTestRestoreError())
	}
	switch {
	case len(errs) > 0:
		run.Status = catalog.RunPartial
	default:
		run.Status = catalog.RunSuccess
	}
	run.Summary = fmt.Sprintf("%d snapshots, %d objects checked, %d files read, %s",
		resp.GetSnapshots(), resp.GetObjectsChecked(), resp.GetFilesRead(), HumanBytes(resp.GetBytesRead()))
	if resp.GetTestRestore() != "" && resp.GetTestRestoreError() == "" {
		run.Summary += "; test restored " + resp.GetTestRestore()
	}
	run.Error = strings.Join(errs, "\n")
}

// listRemoteSnapshots asks the job's agent to list the job's snapshots from the
// repository itself, which opens a connection to its storage.
func (c *Controller) listRemoteSnapshots(ctx context.Context, jobID string) ([]Snapshot, error) {
	job, repo, agent, err := c.jobContext(ctx, jobID)
	if err != nil {
		return nil, err
	}
	paths := job.Paths
	if job.Stack != nil {
		paths = []string{"/stacks/" + job.Stack.Project}
	}
	if job.VM != nil {
		paths = []string{"/vms/" + job.VM.Name}
	}
	resp, err := withAgent(c, agent, browseTimeout, func(ctx context.Context, cl agentpb.AgentClient) (*agentpb.ListSnapshotsResponse, error) {
		return cl.ListSnapshots(ctx, &agentpb.ListSnapshotsRequest{Repository: toProtoRepo(repo), Paths: paths})
	})
	if err != nil {
		return nil, err
	}
	out := make([]Snapshot, 0, len(resp.GetSnapshots()))
	for _, s := range resp.GetSnapshots() {
		out = append(out, Snapshot{
			ID: s.GetId(), Path: s.GetPath(),
			Start: time.Unix(s.GetStartUnix(), 0), End: time.Unix(s.GetEndUnix(), 0),
			Bytes: s.GetBytes(), Files: int(s.GetFiles()), Errors: int(s.GetErrors()), Incomplete: s.GetIncomplete(),
		})
	}
	return out, nil
}

func (c *Controller) Browse(ctx context.Context, jobID, snapshotID, path string) ([]*agentpb.DirEntry, error) {
	_, repo, agent, err := c.jobContext(ctx, jobID)
	if err != nil {
		return nil, err
	}
	return c.browse.do(browseKey{repo.ID, snapshotID, path}, func() ([]*agentpb.DirEntry, error) {
		release, err := c.gate.acquire(ctx, repo.ID)
		if err != nil {
			return nil, err
		}
		defer release()
		resp, err := withAgent(c, agent, browseTimeout, func(ctx context.Context, cl agentpb.AgentClient) (*agentpb.ListDirectoryResponse, error) {
			return cl.ListDirectory(ctx, &agentpb.ListDirectoryRequest{Repository: toProtoRepo(repo), SnapshotId: snapshotID, Path: path})
		})
		if err != nil {
			return nil, err
		}
		return resp.GetEntries(), nil
	})
}

// Restore restores path from a snapshot into target on the job's agent and
// records it as a run of the job.
func (c *Controller) Restore(ctx context.Context, jobID, snapshotID, path, target string, overwrite bool) (catalog.Run, error) {
	if _, err := guard.RestoreTarget(target); err != nil {
		return catalog.Run{}, err
	}
	if !c.inflight.start(jobID) {
		return catalog.Run{}, ErrRunning
	}
	defer c.inflight.done(jobID)

	_, repo, agent, err := c.jobContext(ctx, jobID)
	if err != nil {
		return catalog.Run{}, err
	}
	run := catalog.Run{JobID: jobID, Kind: "restore", Trigger: "manual", StartedAt: time.Now()}
	if run.ID, err = c.Catalog.StartRun(ctx, jobID, run.Kind, run.Trigger, run.StartedAt); err != nil {
		return catalog.Run{}, err
	}
	resp, err := withAgent(c, agent, runTimeout, func(ctx context.Context, cl agentpb.AgentClient) (*agentpb.RestoreResponse, error) {
		return cl.Restore(ctx, &agentpb.RestoreRequest{
			Repository: toProtoRepo(repo), SnapshotId: snapshotID, Path: path, Target: target, Overwrite: overwrite,
		})
	})
	run.FinishedAt = time.Now()
	what := "/" + strings.Trim(path, "/")
	if err != nil {
		run.Status, run.Error = catalog.RunFailed, err.Error()
		run.Summary = fmt.Sprintf("restore %s of %s to %s", what, shortID(snapshotID), target)
	} else {
		run.Status = catalog.RunSuccess
		if w := resp.GetWarnings(); len(w) > 0 {
			run.Status, run.Error = catalog.RunPartial, strings.Join(w, "\n")
		}
		run.Bytes, run.Files = resp.GetBytes(), int(resp.GetFiles())
		run.Summary = fmt.Sprintf("restored %s of %s to %s: %d files, %s", what, shortID(snapshotID), target, run.Files, HumanBytes(run.Bytes))
	}
	if ferr := c.Catalog.FinishRun(context.WithoutCancel(ctx), run); ferr != nil {
		slog.Error("record run", "job", jobID, "err", ferr)
	}
	return run, nil
}

func (c *Controller) jobContext(ctx context.Context, jobID string) (catalog.Job, catalog.Repository, catalog.Agent, error) {
	job, err := c.Catalog.Job(ctx, jobID)
	if err != nil {
		return catalog.Job{}, catalog.Repository{}, catalog.Agent{}, fmt.Errorf("job: %w", err)
	}
	repo, err := c.Catalog.Repository(ctx, job.RepositoryID)
	if err != nil {
		return catalog.Job{}, catalog.Repository{}, catalog.Agent{}, fmt.Errorf("repository: %w", err)
	}
	if repo.InitializedAt.IsZero() {
		return catalog.Job{}, catalog.Repository{}, catalog.Agent{}, ErrNotInitialized
	}
	agent, err := c.Catalog.Agent(ctx, job.AgentID)
	if err != nil {
		return catalog.Job{}, catalog.Repository{}, catalog.Agent{}, fmt.Errorf("agent: %w", err)
	}
	return job, repo, agent, nil
}

// withAgent dials the agent and runs call with its own deadline, detached
// from ctx's cancellation so a closed browser tab can't abort a backup.
func withAgent[T any](c *Controller, a catalog.Agent, timeout time.Duration, call func(context.Context, agentpb.AgentClient) (T, error)) (T, error) {
	var zero T
	conn, err := c.dialAgent(a)
	if err != nil {
		return zero, err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return call(ctx, agentpb.NewAgentClient(conn))
}

func toProtoRepo(r catalog.Repository) *agentpb.Repository {
	cfg, _ := json.Marshal(r.Config) // a repocfg.Config always marshals
	return &agentpb.Repository{Id: r.ID, ConfigJson: cfg, Password: r.Password}
}

func newPassword() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	s := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
	// Groups of 4 are easier to copy onto paper.
	var parts []string
	for len(s) > 4 {
		parts, s = append(parts, s[:4]), s[4:]
	}
	return strings.Join(append(parts, s), "-"), nil
}

func nonEmpty(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// HumanBytes formats a byte count with binary units.
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
