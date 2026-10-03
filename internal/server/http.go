package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
	"github.com/voidgrid/voidgrid-backup/internal/guard"
	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
	"github.com/voidgrid/voidgrid-backup/internal/repocfg"
	"github.com/voidgrid/voidgrid-backup/internal/version"
	"github.com/voidgrid/voidgrid-backup/web"
)

// NewHandler serves the UI and JSON API, behind a mandatory sign-in
// (Controller.Auth; see package webauth) and the setup wizard that comes
// before it. Cross-origin POSTs are also refused so a browser can't be
// tricked into changing anything even while signed in.
func NewHandler(c *Controller) http.Handler {
	if c.Auth() == nil {
		panic("server: NewHandler requires Controller.Auth (see Controller.ReloadAuth); signing in is not optional")
	}
	u := &ui{c: c, tmpl: template.Must(template.New("").Funcs(template.FuncMap{
		"ago":      ago,
		"when":     func(t time.Time) string { return whenStr(t) },
		"bytes":    HumanBytes,
		"short":    shortID,
		"join":     strings.Join,
		"child":    func(dir, name string) string { return strings.TrimPrefix(path.Join(dir, name), "/") },
		"q":        url.QueryEscape,
		"unixTime": func(s int64) time.Time { return time.Unix(s, 0) },
		"list":     func(s ...string) []string { return s },
	}).ParseFS(web.FS, "templates/*.html"))}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := c.Catalog.Ping(ctx); err != nil {
			http.Error(w, "catalog: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok") //nolint:errcheck // status already sent; a failed write means the client went away
	})
	mux.HandleFunc("GET /{$}", u.agents)
	mux.HandleFunc("POST /agents/registrations/{id}/approve", u.approveRegistration)
	mux.HandleFunc("POST /agents/registrations/{id}/reject", u.rejectRegistration)
	mux.HandleFunc("POST /agents/registrations/{id}/forget", u.forgetRegistration)
	mux.HandleFunc("POST /agents/token/rotate", u.rotateToken)
	mux.HandleFunc("POST /agents/{id}/rename", u.renameAgent)
	mux.HandleFunc("GET /repositories", u.repositories)
	mux.HandleFunc("POST /repositories", u.addRepository)
	mux.HandleFunc("POST /repositories/{id}/stats", u.refreshRepoStats)
	mux.HandleFunc("GET /notifications", u.notifications)
	mux.HandleFunc("POST /notifications", u.saveNotifications)
	mux.HandleFunc("POST /notifications/test", u.testNotifications)
	mux.HandleFunc("GET /logs", u.logs)
	mux.HandleFunc("GET /jobs", u.jobs)
	mux.HandleFunc("POST /jobs", u.addJob)
	mux.HandleFunc("GET /jobs/{id}", u.job)
	mux.HandleFunc("POST /jobs/{id}/run", u.runJob)
	mux.HandleFunc("POST /jobs/{id}/check", u.checkJob)
	mux.HandleFunc("POST /jobs/{id}/toggle", u.toggleJob)
	mux.HandleFunc("POST /jobs/{id}/delete", u.deleteJob)
	mux.HandleFunc("POST /jobs/{id}/snapshots/refresh", u.refreshSnapshots)
	mux.HandleFunc("GET /jobs/{id}/edit", u.editJob)
	mux.HandleFunc("POST /jobs/{id}/edit", u.saveJob)
	mux.HandleFunc("GET /jobs/{id}/snapshots/{sid}", u.browse)
	mux.HandleFunc("POST /jobs/{id}/snapshots/{sid}/restore", u.restore)
	mux.HandleFunc("POST /jobs/{id}/snapshots/{sid}/restore-stack", u.restoreStack)
	mux.HandleFunc("POST /jobs/{id}/snapshots/{sid}/import", u.importDump)
	mux.HandleFunc("GET /agents/{id}/stacks", u.stacks)
	mux.HandleFunc("POST /jobs/stack", u.addStackJob)
	mux.HandleFunc("GET /agents/{id}/vms", u.vms)
	mux.HandleFunc("POST /jobs/vm", u.addVMJob)
	mux.HandleFunc("POST /jobs/{id}/snapshots/{sid}/restore-vm", u.restoreVM)
	mux.HandleFunc("GET /api/agents/{id}/usage", u.apiUsage)
	mux.HandleFunc("GET /api/agents", u.apiAgents)
	mux.HandleFunc("GET /api/repositories", u.apiRepositories)
	mux.HandleFunc("GET /api/jobs", u.apiJobs)
	mux.HandleFunc("GET /api/jobs/{id}/runs", u.apiRuns)
	// Registered unconditionally and dispatched through whichever
	// Authenticator is current (c.Auth(), not a fixed instance captured
	// here), so turning OIDC on or off later -- the setup wizard, or the
	// authentication settings page -- never needs a new route added to
	// take effect; StartOIDC/CompleteOIDC 404 on their own when OIDC isn't
	// currently configured.
	mux.HandleFunc("GET /auth/start", func(w http.ResponseWriter, r *http.Request) { c.Auth().StartOIDC(w, r) })
	mux.HandleFunc("GET /auth/callback", func(w http.ResponseWriter, r *http.Request) { c.Auth().CompleteOIDC(w, r) })
	mux.HandleFunc("POST /auth/logout", func(w http.ResponseWriter, r *http.Request) { c.Auth().Logout(w, r) })
	mux.HandleFunc("GET /auth/login", u.signIn)
	mux.HandleFunc("GET /auth/recovery", u.recoveryLogin)
	mux.HandleFunc("POST /auth/recovery", u.recoveryLoginSubmit)
	mux.HandleFunc("GET /authentication", u.authSettings)
	mux.HandleFunc("POST /authentication", u.saveAuthSettings)
	mux.HandleFunc("GET /setup", u.setup)
	mux.HandleFunc("POST /setup", u.generateSetupCodes)

	corsProtected := http.NewCrossOriginProtection().Handler(mux)
	// c.Auth().Middleware is recomputed per request (not wrapped once at
	// startup) for the same reason: it needs to reflect the Authenticator
	// currently installed, which can change after startup.
	withAuth := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.Auth().Middleware(corsProtected).ServeHTTP(w, r)
	})
	return u.requireSetup(withAuth)
}

type ui struct {
	c    *Controller
	tmpl *template.Template
}

// base is embedded in every page.
type base struct {
	Title   string
	Nav     string
	Version string
	Error   string
	Notice  string
	User    string // signed-in identity; "" when login is off or not signed in
}

func (u *ui) render(w http.ResponseWriter, code int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	if err := u.tmpl.ExecuteTemplate(w, name, data); err != nil {
		slog.Error("render", "page", name, "err", err)
	}
}

func (u *ui) newBase(title, nav string, r *http.Request) base {
	b := base{Title: title, Nav: nav, Version: version.Version, Notice: r.URL.Query().Get("notice")}
	b.User, _ = u.c.Auth().CurrentUser(r)
	return b
}

func redirectNotice(w http.ResponseWriter, r *http.Request, to, notice string) {
	if notice != "" {
		to += "?notice=" + url.QueryEscape(notice)
	}
	http.Redirect(w, r, to, http.StatusSeeOther) //nolint:gosec // G710: callers pass local paths only
}

// Agents

type agentsPage struct {
	base
	Agents  []catalog.Agent
	Pending []catalog.Registration
	Token   string
}

func (u *ui) agents(w http.ResponseWriter, r *http.Request) {
	u.renderAgents(w, r, http.StatusOK, agentsPage{base: u.newBase("Agents", "agents", r)})
}

func (u *ui) renderAgents(w http.ResponseWriter, r *http.Request, code int, p agentsPage) {
	ctx := r.Context()
	agents, err := u.c.Catalog.ListAgents(ctx)
	if err == nil {
		p.Pending, err = u.c.Catalog.ListRegistrations(ctx)
	}
	if err == nil {
		p.Token, err = u.c.RegistrationToken(ctx)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	p.Agents = agents
	u.render(w, code, "agents", p)
}

// agentsFailed re-renders the Agents page with err shown.
func (u *ui) agentsFailed(w http.ResponseWriter, r *http.Request, err error) {
	p := agentsPage{base: u.newBase("Agents", "agents", r)}
	if errors.Is(err, catalog.ErrNotFound) {
		err = errors.New("not found (it may have been decided or removed already)")
	}
	p.Error = err.Error()
	u.renderAgents(w, r, http.StatusBadRequest, p)
}

func (u *ui) approveRegistration(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	ag, err := u.c.ApproveRegistration(ctx, r.PathValue("id"), r.FormValue("name"), r.FormValue("address"), r.FormValue("replace"))
	if err != nil {
		u.agentsFailed(w, r, err)
		return
	}
	redirectNotice(w, r, "/", "Approved "+ag.Name+". The agent collects its certificate within a few seconds.")
}

func (u *ui) rejectRegistration(w http.ResponseWriter, r *http.Request) {
	if err := u.c.RejectRegistration(r.Context(), r.PathValue("id")); err != nil {
		u.agentsFailed(w, r, err)
		return
	}
	redirectNotice(w, r, "/", "Rejected.")
}

func (u *ui) forgetRegistration(w http.ResponseWriter, r *http.Request) {
	if err := u.c.ForgetRegistration(r.Context(), r.PathValue("id")); err != nil {
		u.agentsFailed(w, r, err)
		return
	}
	redirectNotice(w, r, "/", "Forgotten. The agent can register again.")
}

func (u *ui) rotateToken(w http.ResponseWriter, r *http.Request) {
	if _, err := u.c.RotateRegistrationToken(r.Context()); err != nil {
		u.agentsFailed(w, r, err)
		return
	}
	redirectNotice(w, r, "/", "Token rotated. Update VB_TOKEN on agents that have not registered yet.")
}

func (u *ui) renameAgent(w http.ResponseWriter, r *http.Request) {
	if err := u.c.RenameAgent(r.Context(), r.PathValue("id"), r.FormValue("name")); err != nil {
		u.agentsFailed(w, r, err)
		return
	}
	redirectNotice(w, r, "/", "Renamed.")
}

// Repositories

type repoRow struct {
	catalog.Repository
	Location  string
	Jobs      int
	Stats     *RepoStats // last measurement of the space used in storage, nil if never measured
	Measuring bool       // a measurement is running now
}

type repositoriesPage struct {
	base
	Measuring bool // any repository is being measured: the page keeps itself current
	Repos     []repoRow
	Agents    []catalog.Agent
	Form      url.Values
	Generated string // shown exactly once, right after creation
	Created   string
}

func (u *ui) repositories(w http.ResponseWriter, r *http.Request) {
	u.renderRepositories(w, r, http.StatusOK, repositoriesPage{base: u.newBase("Repositories", "repos", r)})
}

func (u *ui) renderRepositories(w http.ResponseWriter, r *http.Request, code int, p repositoriesPage) {
	ctx := r.Context()
	repos, err := u.c.Catalog.ListRepositories(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jobs, err := u.c.Catalog.ListJobs(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, rp := range repos {
		row := repoRow{Repository: rp, Location: rp.Config.Location()}
		if st, ok, _ := u.c.RepoStats(ctx, rp.ID); ok {
			row.Stats = &st
		}
		if row.Measuring = u.c.IsRunning(repoMeasureKey(rp.ID)); row.Measuring {
			p.Measuring = true
		}
		for _, j := range jobs {
			if j.RepositoryID == rp.ID {
				row.Jobs++
			}
		}
		p.Repos = append(p.Repos, row)
	}
	if p.Agents, err = u.c.Catalog.ListAgents(ctx); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if p.Form == nil {
		p.Form = url.Values{"kind": {repocfg.KindSFTP}, "sftp_port": {"23"}, "ecc": {"on"}}
	}
	u.render(w, code, "repositories", p)
}

// refreshRepoStats measures the repository's storage in the background: listing
// every blob can take minutes on a big S3 or SFTP repository.
func (u *ui) refreshRepoStats(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rp, err := u.c.Catalog.Repository(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !u.c.inflight.start(repoMeasureKey(id)) {
		redirectNotice(w, r, "/repositories", "Already measuring "+rp.Name+".")
		return
	}
	go func() {
		defer u.c.inflight.done(repoMeasureKey(id))
		if _, err := u.c.RefreshRepoStats(context.Background(), id); err != nil {
			slog.Error("repository stats", "repository", id, "err", err)
		}
	}()
	redirectNotice(w, r, "/repositories", "Measuring "+rp.Name+".")
}

func (u *ui) addRepository(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f := r.PostForm
	cfg, err := repoConfigFromForm(f)
	var generated string
	var created catalog.Repository
	if err == nil {
		ctx, cancel := context.WithTimeout(r.Context(), initTimeout+10*time.Second)
		defer cancel()
		created, generated, err = u.c.AddRepository(ctx, f.Get("name"), cfg, f.Get("password"), f.Get("agent"))
	}
	p := repositoriesPage{base: u.newBase("Repositories", "repos", r)}
	if err != nil {
		// Secrets are not echoed back.
		for _, k := range []string{"password", "sftp_password", "s3_secret"} {
			f.Del(k)
		}
		p.Form, p.Error = f, err.Error()
		u.renderRepositories(w, r, http.StatusBadRequest, p)
		return
	}
	if generated == "" {
		redirectNotice(w, r, "/repositories", "Created repository "+created.Name+".")
		return
	}
	p.Generated, p.Created = generated, created.Name
	u.renderRepositories(w, r, http.StatusOK, p)
}

func repoConfigFromForm(f url.Values) (repocfg.Config, error) {
	cfg := repocfg.Config{Kind: f.Get("kind"), ECC: f.Get("ecc") != ""}
	switch cfg.Kind {
	case repocfg.KindSFTP:
		port, err := strconv.Atoi(strings.TrimSpace(f.Get("sftp_port")))
		if err != nil {
			return cfg, errors.New("sftp port must be a number")
		}
		cfg.SFTP = &repocfg.SFTP{
			Host:       strings.TrimSpace(f.Get("sftp_host")),
			Port:       port,
			User:       strings.TrimSpace(f.Get("sftp_user")),
			Path:       strings.TrimSpace(f.Get("sftp_path")),
			KeyFile:    strings.TrimSpace(f.Get("sftp_key_file")),
			Password:   f.Get("sftp_password"),
			KnownHosts: strings.TrimSpace(f.Get("sftp_known_hosts")),
		}
		if cfg.SFTP.KnownHosts != "" {
			cfg.SFTP.KnownHosts += "\n"
		}
	case repocfg.KindS3:
		cfg.S3 = &repocfg.S3{
			Endpoint:        strings.TrimSpace(f.Get("s3_endpoint")),
			Bucket:          strings.TrimSpace(f.Get("s3_bucket")),
			Prefix:          strings.TrimSpace(f.Get("s3_prefix")),
			Region:          strings.TrimSpace(f.Get("s3_region")),
			AccessKeyID:     strings.TrimSpace(f.Get("s3_access_key")),
			SecretAccessKey: f.Get("s3_secret"),
			Insecure:        f.Get("s3_insecure") != "",
		}
	case repocfg.KindFilesystem:
		cfg.Filesystem = &repocfg.Filesystem{Path: strings.TrimSpace(f.Get("fs_path"))}
	}
	return cfg, cfg.Validate()
}

// Jobs

type jobRow struct {
	catalog.Job
	Agent, Repo string
	Last        *catalog.Run
	Next        time.Time
	Running     bool
}

type jobsPage struct {
	base
	Jobs   []jobRow
	Agents []catalog.Agent
	Repos  []catalog.Repository
	Form   url.Values
}

func (u *ui) jobs(w http.ResponseWriter, r *http.Request) {
	u.renderJobs(w, r, http.StatusOK, jobsPage{base: u.newBase("Jobs", "jobs", r)})
}

func (u *ui) renderJobs(w http.ResponseWriter, r *http.Request, code int, p jobsPage) {
	ctx := r.Context()
	jobs, err := u.c.Catalog.ListJobs(ctx)
	if err == nil {
		p.Agents, err = u.c.Catalog.ListAgents(ctx)
	}
	if err == nil {
		p.Repos, err = u.c.Catalog.ListRepositories(ctx)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, j := range jobs {
		p.Jobs = append(p.Jobs, u.jobRow(ctx, j, p.Agents, p.Repos))
	}
	if p.Form == nil {
		k := catalog.DefaultRetention
		p.Form = url.Values{
			"schedule": {"0 3 * * *"}, "enabled": {"on"},
			"keep_latest": {strconv.Itoa(k.Latest)}, "keep_hourly": {strconv.Itoa(k.Hourly)},
			"keep_daily": {strconv.Itoa(k.Daily)}, "keep_weekly": {strconv.Itoa(k.Weekly)},
			"keep_monthly": {strconv.Itoa(k.Monthly)}, "keep_annual": {strconv.Itoa(k.Annual)},
		}
	}
	u.render(w, code, "jobs", p)
}

func (u *ui) jobRow(ctx context.Context, j catalog.Job, agents []catalog.Agent, repos []catalog.Repository) jobRow {
	row := jobRow{Job: j, Running: u.c.IsRunning(j.ID)}
	for _, a := range agents {
		if a.ID == j.AgentID {
			row.Agent = a.Name
		}
	}
	for _, rp := range repos {
		if rp.ID == j.RepositoryID {
			row.Repo = rp.Name
		}
	}
	if last, err := u.c.Catalog.LastRun(ctx, j.ID, "backup"); err == nil {
		row.Last = &last
	}
	if j.Enabled && j.Schedule != "" {
		row.Next, _ = NextRun(ctx, u.c.Catalog, j)
	}
	return row
}

func (u *ui) addJob(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f := r.PostForm
	in := JobInput{
		Name:         f.Get("name"),
		AgentID:      f.Get("agent"),
		RepositoryID: f.Get("repository"),
		Paths:        strings.Split(f.Get("paths"), "\n"),
		Excludes:     strings.Split(f.Get("excludes"), "\n"),
		Schedule:     f.Get("schedule"),
		Enabled:      f.Get("enabled") != "",
	}
	var err error
	in.Keep, err = keepFromForm(f)
	var job catalog.Job
	if err == nil {
		job, err = u.c.AddJob(r.Context(), in)
	}
	if err != nil {
		p := jobsPage{base: u.newBase("Jobs", "jobs", r), Form: f}
		p.Error = err.Error()
		u.renderJobs(w, r, http.StatusBadRequest, p)
		return
	}
	redirectNotice(w, r, "/jobs/"+job.ID, "Created job "+job.Name+".")
}

type jobPage struct {
	base
	Job        jobRow
	RepoName   string
	Runs       []catalog.Run
	Snapshots  []Snapshot
	SnapError  string
	SnapAsOf   time.Time // when the snapshot list was last made to match the repository
	SnapSynced bool
	SnapBusy   bool // a refresh from the repository is running
}

func (u *ui) job(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	j, err := u.c.Catalog.Job(ctx, r.PathValue("id"))
	if errors.Is(err, catalog.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	agents, _ := u.c.Catalog.ListAgents(ctx)
	repos, _ := u.c.Catalog.ListRepositories(ctx)
	p := jobPage{base: u.newBase(j.Name, "jobs", r), Job: u.jobRow(ctx, j, agents, repos)}
	if p.Runs, err = u.c.Catalog.ListRuns(ctx, j.ID, 25); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if p.Snapshots, err = u.c.Snapshots(ctx, j.ID); err != nil {
		p.SnapError = err.Error()
	}
	p.SnapAsOf, p.SnapSynced = u.c.SnapshotsSynced(ctx, j.ID)
	// A job whose list has never been checked against the repository (every
	// job from before the list was kept here) gets one background refresh.
	if !p.SnapSynced {
		u.startSnapshotRefresh(j.ID)
	}
	p.SnapBusy = u.c.IsRunning(snapshotRefreshKey(j.ID))
	u.render(w, http.StatusOK, "job", p)
}

func repoMeasureKey(repoID string) string { return "stats:" + repoID }

func snapshotRefreshKey(jobID string) string { return "snapshots:" + jobID }

// startSnapshotRefresh refreshes a job's snapshot list from the repository in
// the background, unless one is already running.
func (u *ui) startSnapshotRefresh(jobID string) {
	if !u.c.inflight.start(snapshotRefreshKey(jobID)) {
		return
	}
	go func() {
		defer u.c.inflight.done(snapshotRefreshKey(jobID))
		if err := u.c.RefreshSnapshots(context.Background(), jobID); err != nil {
			slog.Error("refresh snapshots", "job", jobID, "err", err)
		}
	}()
}

func (u *ui) refreshSnapshots(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := u.c.Catalog.Job(r.Context(), id); err != nil {
		http.NotFound(w, r)
		return
	}
	u.startSnapshotRefresh(id)
	redirectNotice(w, r, "/jobs/"+id, "Refreshing the snapshot list from the repository.")
}

func (u *ui) runJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := u.c.Catalog.Job(r.Context(), id); err != nil {
		http.NotFound(w, r)
		return
	}
	if u.c.IsRunning(id) {
		redirectNotice(w, r, "/jobs/"+id, ErrRunning.Error())
		return
	}
	go func() {
		if _, err := u.c.RunJob(context.Background(), id, "manual"); err != nil && !errors.Is(err, ErrRunning) {
			slog.Error("manual backup", "job", id, "err", err)
		}
	}()
	redirectNotice(w, r, "/jobs/"+id, "Backup started.")
}

func (u *ui) checkJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := u.c.Catalog.Job(r.Context(), id); err != nil {
		http.NotFound(w, r)
		return
	}
	if u.c.IsRunning(id) {
		redirectNotice(w, r, "/jobs/"+id, ErrRunning.Error())
		return
	}
	go func() {
		if _, err := u.c.RunCheck(context.Background(), id); err != nil && !errors.Is(err, ErrRunning) {
			slog.Error("manual check", "job", id, "err", err)
		}
	}()
	redirectNotice(w, r, "/jobs/"+id, "Check started.")
}

func (u *ui) toggleJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	j, err := u.c.Catalog.Job(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := u.c.Catalog.SetJobEnabled(r.Context(), id, !j.Enabled); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	state := "enabled"
	if j.Enabled {
		state = "disabled"
	}
	redirectNotice(w, r, "/jobs/"+id, "Schedule "+state+".")
}

func (u *ui) deleteJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	j, err := u.c.Catalog.Job(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if r.FormValue("confirm") != j.Name {
		redirectNotice(w, r, "/jobs/"+id, "Type the job name to confirm deletion.")
		return
	}
	if u.c.IsRunning(id) {
		redirectNotice(w, r, "/jobs/"+id, "Wait for the running backup to finish before deleting.")
		return
	}
	if err := u.c.Catalog.DeleteJob(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	redirectNotice(w, r, "/jobs", "Deleted job "+j.Name+". Its snapshots are still in the repository.")
}

// Browse and restore

type crumb struct{ Name, Path string }

type browsePage struct {
	base
	Job        catalog.Job
	SnapshotID string
	Path       string
	Crumbs     []crumb
	Entries    []*agentpb.DirEntry
	Target     string
	Dumps      []string // stack snapshot root only: services with a dump
}

func (u *ui) browse(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	j, err := u.c.Catalog.Job(ctx, r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	sid := r.PathValue("sid")
	rel := strings.Trim(path.Clean("/"+r.URL.Query().Get("path")), "/")
	p := browsePage{base: u.newBase(j.Name+" snapshot "+shortID(sid), "jobs", r), Job: j, SnapshotID: sid, Path: rel,
		Target: "/restore/" + j.Name + "-" + shortID(sid)}
	acc := ""
	for _, part := range strings.Split(rel, "/") {
		if part != "" {
			acc = strings.TrimPrefix(acc+"/"+part, "/")
			p.Crumbs = append(p.Crumbs, crumb{Name: part, Path: acc})
		}
	}
	if p.Entries, err = u.c.Browse(ctx, j.ID, sid, rel); err != nil {
		p.Error = err.Error()
	}
	if j.Stack != nil && rel == "" {
		dumps, _ := u.c.Browse(ctx, j.ID, sid, "dumps")
		for _, d := range dumps {
			if name := d.GetName(); !d.GetDir() && d.GetSize() > 0 {
				p.Dumps = append(p.Dumps, strings.TrimSuffix(name, path.Ext(name)))
			}
		}
	}
	u.render(w, http.StatusOK, "browse", p)
}

func (u *ui) restore(w http.ResponseWriter, r *http.Request) {
	id, sid := r.PathValue("id"), r.PathValue("sid")
	if _, err := u.c.Catalog.Job(r.Context(), id); err != nil {
		http.NotFound(w, r)
		return
	}
	rel := strings.Trim(path.Clean("/"+r.FormValue("path")), "/")
	target, overwrite := strings.TrimSpace(r.FormValue("target")), r.FormValue("overwrite") != ""
	back := "/jobs/" + id + "/snapshots/" + sid + "?path=" + url.QueryEscape(rel)
	if _, err := guard.RestoreTarget(target); err != nil {
		http.Redirect(w, r, back+"&notice="+url.QueryEscape(err.Error()), http.StatusSeeOther) //nolint:gosec // G710: local path built from route segments
		return
	}
	if u.c.IsRunning(id) {
		http.Redirect(w, r, back+"&notice="+url.QueryEscape(ErrRunning.Error()), http.StatusSeeOther) //nolint:gosec // G710: local path built from route segments
		return
	}
	go func() {
		if _, err := u.c.Restore(context.Background(), id, sid, rel, target, overwrite); err != nil && !errors.Is(err, ErrRunning) {
			slog.Error("restore", "job", id, "err", err)
		}
	}()
	redirectNotice(w, r, "/jobs/"+id, "Restore to "+target+" started. The result appears under Runs.")
}

// JSON API

type repoView struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	Location      string         `json:"location"`
	Config        repocfg.Config `json:"config"`
	CreatedAt     time.Time      `json:"created_at"`
	InitializedAt time.Time      `json:"initialized_at"`
	Stats         *RepoStats     `json:"stats,omitempty"`
}

func (u *ui) apiAgents(w http.ResponseWriter, r *http.Request) {
	agents, err := u.c.Catalog.ListAgents(r.Context())
	writeJSON(w, agents, err)
}

func (u *ui) apiRepositories(w http.ResponseWriter, r *http.Request) {
	repos, err := u.c.Catalog.ListRepositories(r.Context())
	out := make([]repoView, 0, len(repos))
	for _, rp := range repos {
		v := repoView{ID: rp.ID, Name: rp.Name, Location: rp.Config.Location(),
			Config: rp.Config.Redacted(), CreatedAt: rp.CreatedAt, InitializedAt: rp.InitializedAt}
		if st, ok, _ := u.c.RepoStats(r.Context(), rp.ID); ok {
			v.Stats = &st
		}
		out = append(out, v)
	}
	writeJSON(w, out, err)
}

func (u *ui) apiJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := u.c.Catalog.ListJobs(r.Context())
	writeJSON(w, jobs, err)
}

func (u *ui) apiRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := u.c.Catalog.ListRuns(r.Context(), r.PathValue("id"), 100)
	writeJSON(w, runs, err)
}

func writeJSON[T any](w http.ResponseWriter, v []T, err error) {
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if v == nil {
		v = []T{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v) //nolint:errcheck // status already sent; a failed write means the client went away
}

func whenStr(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t).Round(time.Second)
	switch {
	case d < 0:
		return "in " + (-d).Round(time.Minute).String()
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return whenStr(t)
}
