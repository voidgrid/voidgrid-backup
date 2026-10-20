package server

import (
	"io"

	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path"
	"slices"
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
		"ago":       ago,
		"took":      took,
		"tookRun":   tookRun,
		"tookMaint": tookMaint,
		"mul24":     func(n int) int { return n*24 + 4 },
		"sub1":      func(n int) int { return n - 1 },
		"sortHeads": sortHeads,
		"when":      func(t time.Time) string { return whenStr(t) },
		"future":    func(t time.Time) bool { return t.After(time.Now()) },
		"bytes":     HumanBytes,
		"short":     shortID,
		"join":      strings.Join,
		"json": func(v any) string {
			b, _ := json.Marshal(v)
			return string(b)
		},
		"child":    func(dir, name string) string { return strings.TrimPrefix(path.Join(dir, name), "/") },
		"unixTime": func(s int64) time.Time { return time.Unix(s, 0) },
		"list":     func(s ...string) []string { return s },
		"todText":  todText,
	}).ParseFS(web.FS, "templates/*.html"))}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := c.Catalog.Ping(ctx); err != nil {
			http.Error(w, "catalog: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		if _, err := io.WriteString(w, "ok\n"); err != nil {
			slog.Debug("healthz reply not delivered", "err", err) // the client went away
		}
	})
	mux.HandleFunc("GET /{$}", u.status)
	mux.HandleFunc("GET /agents", u.agents)
	mux.HandleFunc("POST /agents/registrations/{id}/approve", u.approveRegistration)
	mux.HandleFunc("POST /agents/registrations/{id}/reject", u.rejectRegistration)
	mux.HandleFunc("POST /agents/registrations/{id}/forget", u.forgetRegistration)
	mux.HandleFunc("POST /agents/token/rotate", u.rotateToken)
	mux.HandleFunc("POST /agents/{id}/rename", u.renameAgent)
	mux.HandleFunc("GET /repositories", u.repositories)
	mux.HandleFunc("POST /repositories", u.addRepository)
	mux.HandleFunc("GET /repositories/{id}/edit", u.editRepositoryForm)
	mux.HandleFunc("POST /repositories/{id}/edit", u.editRepository)
	mux.HandleFunc("POST /agents/{id}/delete", u.deleteAgent)
	mux.HandleFunc("POST /repositories/{id}/reach", u.reachRepository)
	mux.HandleFunc("POST /repositories/{id}/stats", u.refreshRepoStats)
	mux.HandleFunc("POST /repositories/{id}/maintain", u.maintainRepository)
	mux.HandleFunc("POST /repositories/{id}/maintainer", u.setMaintainer)
	mux.HandleFunc("POST /repositories/{id}/maint-schedule", u.setMaintSchedule)
	mux.HandleFunc("POST /settings/sftp-cap", u.setSFTPCap)
	mux.HandleFunc("POST /repositories/{id}/takeover", u.takeOverMaintenance)
	mux.HandleFunc("POST /repositories/{id}/agents", u.assignAgent)
	mux.HandleFunc("POST /repositories/{id}/agents/{agent}/remove", u.unassignAgent)
	mux.HandleFunc("POST /repositories/{id}/agents/{agent}/default", u.defaultRepository)
	mux.HandleFunc("POST /repositories/{id}/remove", u.removeRepository)
	mux.HandleFunc("POST /repositories/{id}/wipe", u.wipeRepository)
	mux.HandleFunc("GET /notifications", u.notifications)
	mux.HandleFunc("POST /notifications", u.saveNotifications)
	mux.HandleFunc("POST /notifications/test", u.testNotifications)
	mux.HandleFunc("GET /logs", u.logs)
	mux.HandleFunc("GET /help", u.helpIndex)
	mux.HandleFunc("GET /help/{name}", u.helpDoc)
	mux.HandleFunc("GET /jobs", u.jobs)
	mux.HandleFunc("GET /jobs/convert-utc", u.convertForm)
	mux.HandleFunc("POST /jobs/convert-utc", u.convertApply)
	mux.HandleFunc("GET /schedule", u.schedule)
	mux.HandleFunc("POST /jobs", u.addJob)
	mux.HandleFunc("GET /jobs/{id}", u.job)
	mux.HandleFunc("POST /jobs/{id}/run", u.runJob)
	mux.HandleFunc("POST /jobs/{id}/cancel", u.cancelRun)
	mux.HandleFunc("POST /jobs/{id}/schedule", u.setSchedule)
	mux.HandleFunc("POST /jobs/{id}/schedule/move", u.moveSchedule)
	mux.HandleFunc("POST /jobs/{id}/check", u.checkJob)
	mux.HandleFunc("POST /jobs/{id}/toggle", u.toggleJob)
	mux.HandleFunc("POST /jobs/{id}/delete", u.deleteJob)
	mux.HandleFunc("POST /jobs/{id}/snapshots/refresh", u.refreshSnapshots)
	mux.HandleFunc("GET /jobs/{id}/edit", u.editJob)
	mux.HandleFunc("POST /jobs/{id}/edit", u.saveJob)
	mux.HandleFunc("GET /jobs/{id}/snapshots/{sid}", u.browse)
	mux.HandleFunc("POST /jobs/{id}/snapshots/{sid}/delete", u.deleteSnapshot)
	mux.HandleFunc("POST /jobs/{id}/snapshots/{sid}/restore", u.restore)
	mux.HandleFunc("POST /jobs/{id}/snapshots/{sid}/restore-stack", u.restoreStack)
	mux.HandleFunc("POST /jobs/{id}/snapshots/{sid}/import", u.importDump)
	mux.HandleFunc("GET /agents/{id}/stacks", u.stacks)
	mux.HandleFunc("POST /jobs/stack", u.addStackJob)
	mux.HandleFunc("GET /agents/{id}/vms", u.vms)
	mux.HandleFunc("POST /jobs/vm", u.addVMJob)
	mux.HandleFunc("POST /jobs/{id}/snapshots/{sid}/restore-vm", u.restoreVM)
	mux.HandleFunc("GET /api/agents/{id}/usage", u.apiUsage)
	mux.HandleFunc("GET /api/suggest-schedule", u.apiSuggestSchedule)
	mux.HandleFunc("POST /schedule/plan/apply", u.applySchedulePlan)
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
	TZ      string // the zone of every time and cron schedule on the page, e.g. "CDT (UTC-05:00)"
}

func (u *ui) render(w http.ResponseWriter, code int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	if err := u.tmpl.ExecuteTemplate(w, name, data); err != nil {
		slog.Error("render", "page", name, "err", err)
	}
}

func (u *ui) newBase(title, nav string, r *http.Request) base {
	b := base{Title: title, Nav: nav, Version: version.Version, Notice: r.URL.Query().Get("notice"), TZ: zoneLabel(time.Now())}
	b.User, _ = u.c.Auth().CurrentUser(r)
	return b
}

// redirectNotice sends the browser to a path on this server, with notice (if
// any) shown there. to is checked first, so no input can make this an
// open redirect to another site.
func redirectNotice(w http.ResponseWriter, r *http.Request, to, notice string) {
	to = localPath(to)
	if notice != "" {
		sep := "?"
		if strings.Contains(to, "?") {
			sep = "&"
		}
		to += sep + "notice=" + url.QueryEscape(notice)
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// localPath returns p as a path on this server, or "/" when p is anything
// else: it has a scheme or a host, or its path doesn't start with a single "/".
// The result is rebuilt from the parsed path and query with every part
// escaped, so it can only ever be a path and query on this server.
func localPath(p string) string {
	u, err := url.Parse(p)
	if err != nil || u.Scheme != "" || u.Host != "" || !strings.HasPrefix(u.Path, "/") || strings.HasPrefix(u.Path, "//") {
		return "/"
	}
	var b strings.Builder
	for _, seg := range strings.Split(u.Path[1:], "/") {
		b.WriteString("/" + url.PathEscape(seg))
	}
	q := u.Query()
	sep := "?"
	for _, k := range slices.Sorted(maps.Keys(q)) {
		for _, v := range q[k] {
			b.WriteString(sep + url.QueryEscape(k) + "=" + url.QueryEscape(v))
			sep = "&"
		}
	}
	return b.String()
}

// Agents

type agentsPage struct {
	base
	Agents  []catalog.Agent
	Pending []catalog.Registration
	Token   string
	// Replaces maps a pending registration's ID to the agent it looks like a
	// rebuild of (same hostname), for the approve form to preselect.
	Replaces map[string]*replaceCandidate
	// Blocked is set when a delete was refused: what stands in its way, with
	// HandTo the other agents maintenance can be handed to.
	Blocked *AgentInUseError
	HandTo  []catalog.Agent
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
	if p.Blocked != nil {
		for _, a := range agents {
			if a.ID != p.Blocked.Agent.ID {
				p.HandTo = append(p.HandTo, a)
			}
		}
	}
	p.Replaces = map[string]*replaceCandidate{}
	now := time.Now()
	for _, reg := range p.Pending {
		if reg.Status == catalog.RegPending {
			p.Replaces[reg.ID] = replaceFor(reg, agents, now)
		}
	}
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

// deleteAgent removes an agent after its name is typed. A refusal shows what
// is in the way, with a form to hand its maintenance over.
func (u *ui) deleteAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ag, err := u.c.Catalog.Agent(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if r.FormValue("confirm") != ag.Name {
		redirectNotice(w, r, "/agents", "Type the agent name to confirm deletion.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	warnings, err := u.c.DeleteAgent(ctx, id)
	if blocked, ok := isAgentInUse(err); ok {
		p := agentsPage{base: u.newBase("Agents", "agents", r), Blocked: &blocked}
		p.Error = blocked.Error()
		u.renderAgents(w, r, http.StatusConflict, p)
		return
	}
	if err != nil {
		u.agentsFailed(w, r, err)
		return
	}
	msg := "Deleted " + ag.Name + ". Its snapshots stay in their repositories."
	if len(warnings) > 0 {
		msg += " Warning: " + strings.Join(warnings, "; ") + "."
	}
	redirectNotice(w, r, "/agents", msg)
}

func (u *ui) approveRegistration(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	ag, err := u.c.ApproveRegistration(ctx, r.PathValue("id"), r.FormValue("name"), r.FormValue("address"), r.FormValue("replace"))
	if err != nil {
		u.agentsFailed(w, r, err)
		return
	}
	redirectNotice(w, r, "/agents", "Approved "+ag.Name+". The agent collects its certificate within a few seconds.")
}

func (u *ui) rejectRegistration(w http.ResponseWriter, r *http.Request) {
	if err := u.c.RejectRegistration(r.Context(), r.PathValue("id")); err != nil {
		u.agentsFailed(w, r, err)
		return
	}
	redirectNotice(w, r, "/agents", "Rejected.")
}

func (u *ui) forgetRegistration(w http.ResponseWriter, r *http.Request) {
	if err := u.c.ForgetRegistration(r.Context(), r.PathValue("id")); err != nil {
		u.agentsFailed(w, r, err)
		return
	}
	redirectNotice(w, r, "/agents", "Forgotten. The agent can register again.")
}

func (u *ui) rotateToken(w http.ResponseWriter, r *http.Request) {
	if _, err := u.c.RotateRegistrationToken(r.Context()); err != nil {
		u.agentsFailed(w, r, err)
		return
	}
	redirectNotice(w, r, "/agents", "Token rotated. Update VB_TOKEN on agents that have not registered yet.")
}

func (u *ui) renameAgent(w http.ResponseWriter, r *http.Request) {
	if err := u.c.RenameAgent(r.Context(), r.PathValue("id"), r.FormValue("name")); err != nil {
		u.agentsFailed(w, r, err)
		return
	}
	redirectNotice(w, r, "/agents", "Renamed.")
}

// Repositories

type repoRow struct {
	catalog.Repository
	Location    string
	Jobs        int
	Stats       *RepoStats // last measurement of the space used in storage, nil if never measured
	Measuring   bool       // a measurement is running now
	Wiping      bool       // its data is being deleted now
	Maint       *RepoMaintenance
	Maintaining bool                     // a maintenance cycle is running now
	MaintRuns   []catalog.MaintenanceRun // its last cycles, newest first
	WipeError   string                   // why the last wipe failed
	MaintBy     string                   // "server" or "agent": who runs maintenance
	MaintSched  MaintSchedule            // when scheduled maintenance runs
	Assigned    []assignedAgent
	Unassigned  []catalog.Agent // agents that could still be assigned
	ServerOK    bool            // the server can open it itself, so it can stand in for an agent
	ServerNote  string          // whether the server can open it, from the last check
	ServerWarn  bool            // ServerNote is a warning: the server cannot
}

// assignedAgent is an agent that may use a repository.
type assignedAgent struct {
	catalog.Agent
	Jobs    int  // its jobs on the repository; the assignment can't be removed while any
	Default bool // new jobs on this agent start with this repository
}

type repositoriesPage struct {
	base
	Measuring bool // any repository is being measured: the page keeps itself current
	Repos     []repoRow
	Agents    []catalog.Agent
	Form      url.Values
	Generated string // shown exactly once, right after creation
	Created   string
	// ServerEngine: the server has its own engine, so it can create a repository.
	ServerEngine bool
	SFTPCap      SFTPCap // the limit on simultaneous connections to one SFTP host
}

func (u *ui) repositories(w http.ResponseWriter, r *http.Request) {
	u.renderRepositories(w, r, http.StatusOK, repositoriesPage{base: u.newBase("Repositories", "repos", r)})
}

func (u *ui) renderRepositories(w http.ResponseWriter, r *http.Request, code int, p repositoriesPage) {
	ctx := r.Context()
	p.ServerEngine = u.c.Engine != nil
	p.SFTPCap = u.c.SFTPCap(ctx)
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
	agents, err := u.c.Catalog.ListAgents(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, rp := range repos {
		row := repoRow{Repository: rp, Location: rp.Config.Location(), ServerOK: u.c.ServerCanOpen(rp) == nil}
		if st, ok, _ := u.c.RepoStats(ctx, rp.ID); ok {
			row.Stats = &st
		}
		if row.Measuring = u.c.IsRunning(repoMeasureKey(rp.ID)); row.Measuring {
			p.Measuring = true
		}
		if row.Wiping = u.c.IsRunning(repoWipeKey(rp.ID)); row.Wiping {
			p.Measuring = true
		}
		row.WipeError = u.c.WipeError(ctx, rp.ID)
		row.ServerNote, row.ServerWarn = u.c.ServerReachNote(ctx, rp)
		row.MaintBy = u.c.MaintainedBy(ctx, rp.ID)
		row.MaintSched = u.c.MaintSchedule(ctx, rp.ID)
		if m, ok := u.c.RepoMaintenance(ctx, rp.ID); ok {
			row.Maint = &m
		}
		row.MaintRuns, _ = u.c.Catalog.ListMaintenanceRuns(ctx, rp.ID, 10)
		if row.Maintaining = u.c.IsRunning(repoMaintRunKey(rp.ID)); row.Maintaining {
			p.Measuring = true
		}
		perAgent := map[string]int{}
		for _, j := range jobs {
			if j.RepositoryID == rp.ID {
				row.Jobs++
				perAgent[j.AgentID]++
			}
		}
		ids, _ := u.c.Catalog.RepositoryAgents(ctx, rp.ID)
		defaults, _ := u.c.Catalog.DefaultRepositories(ctx)
		for _, a := range agents {
			if slices.Contains(ids, a.ID) {
				row.Assigned = append(row.Assigned, assignedAgent{Agent: a, Jobs: perAgent[a.ID], Default: defaults[a.ID] == rp.ID})
			} else {
				row.Unassigned = append(row.Unassigned, a)
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
	ctx := context.WithoutCancel(r.Context()) // the measurement outlives the request
	go func() {
		defer u.c.inflight.done(repoMeasureKey(id))
		if _, err := u.c.RefreshRepoStats(ctx, id); err != nil {
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
	note := ""
	if f.Get("agent") == ServerHost {
		note = " The server created it and maintains it. No agent is assigned yet: assign one under Manage, Assign an agent, before adding a job."
	}
	if generated == "" {
		redirectNotice(w, r, "/repositories", "Created repository "+created.Name+"."+note)
		return
	}
	p.Generated, p.Created = generated, created.Name
	p.Notice = strings.TrimSpace(note)
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
		cfg.Filesystem = &repocfg.Filesystem{Path: strings.TrimSpace(f.Get("fs_path")), ServerPath: strings.TrimSpace(f.Get("fs_server_path"))}
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
	Progress    *RunProgress  // where its upload is, while it runs and its agent reports one
	Clashes     []string      // other jobs it would run alongside in the next 24 h
	Dur         time.Duration // how long a run is expected to take; see jobDuration
	Longest     time.Duration // the longest of the runs Dur was taken from
	DurGuess    bool          // Dur is a default: the job has no finished backup yet
}

type jobsPage struct {
	base
	Jobs []jobRow
	// AnyRunning makes the page refresh faster, so progress moves.
	AnyRunning bool
	Agents     []catalog.Agent
	Repos      []catalog.Repository
	// RepoAgents maps a repository ID to the space-separated IDs of the agents
	// that can take a job on it ("" when it has no assignments: any agent).
	// Defaults maps an agent ID to its default repository ID. The add-job form
	// uses them to offer and preselect the chosen agent's repositories.
	RepoAgents map[string]string
	Defaults   map[string]string
	Form       url.Values
	Sort       string // active sort column, "" for the stored order
	Desc       bool
	// ConvertOffer: schedules saved before beta.14 may still hold UTC hours.
	ConvertOffer bool
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
	p.RepoAgents = map[string]string{}
	for _, rp := range p.Repos {
		ids, _ := u.c.Catalog.RepositoryAgents(ctx, rp.ID)
		p.RepoAgents[rp.ID] = strings.Join(ids, " ")
	}
	p.Defaults, _ = u.c.Catalog.DefaultRepositories(ctx)
	clashes := findClashes(p.Jobs, time.Now())
	for i := range p.Jobs {
		p.Jobs[i].Clashes = clashes[p.Jobs[i].ID]
	}
	if done, err := u.c.SchedulesConverted(ctx); err == nil && !done {
		rows, err := u.c.ConversionPreview(ctx, time.Now())
		p.ConvertOffer = err == nil && len(rows) > 0
	}
	for _, j := range p.Jobs {
		p.AnyRunning = p.AnyRunning || j.Running
	}
	p.Sort = sortJobs(p.Jobs, r.URL.Query().Get("sort"), r.URL.Query().Get("dir") == "desc")
	p.Desc = p.Sort != "" && r.URL.Query().Get("dir") == "desc"
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
	if p, ok := u.c.JobProgress(j.ID); ok {
		row.Progress = &p
	}
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
	row.Dur, row.Longest, row.DurGuess = u.c.jobDuration(ctx, j.ID)
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
	redirectNotice(w, r, "/jobs/"+job.ID, "Created job "+job.Name+"."+u.c.ApplyMaintainer(r.Context(), job, f.Get("maintainer")))
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
	// SnapVia is "the server" when it made the last snapshot list because the
	// job's agent could not reach the repository.
	SnapVia string
	// AgentDown and ServerOK decide whether a snapshot delete offers to run on
	// the server: only when the job's agent is unreachable and the server can
	// open the repository.
	AgentDown, ServerOK bool
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
	p.SnapVia = u.c.SnapshotsVia(ctx, j.ID)
	if ag, aerr := u.c.Catalog.Agent(ctx, j.AgentID); aerr != nil || ag.LastError != "" {
		p.AgentDown = true
	}
	if rp, rerr := u.c.Catalog.Repository(ctx, j.RepositoryID); rerr == nil {
		p.ServerOK = u.c.ServerCanOpen(rp) == nil
	}
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

// listBack is where a button on the Jobs list returns to: the list in the sort
// it was in (the list's forms send from=list, sort and dir), or fallback for a
// post from anywhere else.
func listBack(r *http.Request, fallback string) string {
	if r.FormValue("from") != "list" {
		return fallback
	}
	for _, c := range jobCols {
		if c.key == r.FormValue("sort") {
			return "/jobs?" + url.Values{"sort": {c.key}, "dir": {r.FormValue("dir")}}.Encode()
		}
	}
	return "/jobs"
}

// cancelRun stops the backup, check or restore running for a job. It ends as a
// failed run and notifies like any other.
func (u *ui) cancelRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job, err := u.c.Catalog.Job(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	back := listBack(r, "/jobs/"+id)
	who, _ := u.c.Auth().CurrentUser(r)
	if err := u.c.CancelRun(id, who); err != nil {
		redirectNotice(w, r, back, "Not cancelled: "+err.Error())
		return
	}
	redirectNotice(w, r, back, "Cancelling "+job.Name+". The run ends as failed, with the reason, and is announced like any other.")
}

// setSchedule changes only the schedule of a job, from the Jobs list.
func (u *ui) setSchedule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	back := listBack(r, "/jobs/"+id)
	if err := u.c.SetSchedule(r.Context(), id, r.FormValue("schedule")); err != nil {
		if errors.Is(err, catalog.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		redirectNotice(w, r, back, "Schedule not saved: "+err.Error())
		return
	}
	redirectNotice(w, r, back, "Schedule saved. It applies from the next scheduled start.")
}

func (u *ui) runJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job, err := u.c.Catalog.Job(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	back := listBack(r, "/jobs/"+id)
	if u.c.IsRunning(id) {
		redirectNotice(w, r, back, ErrRunning.Error())
		return
	}
	if r.FormValue("maintain") == "1" {
		notice := "Backup started; maintenance follows it."
		if u.c.IsRunning(repoMaintRunKey(job.RepositoryID)) {
			notice = "Backup started. Maintenance of its repository is already running, so it isn't started again."
		}
		ctx := context.WithoutCancel(r.Context()) // the backup outlives the request
		go func() {
			if _, err := u.c.RunJobThenMaintain(ctx, id, "manual"); err != nil && !errors.Is(err, ErrRunning) {
				slog.Error("manual backup with maintenance", "job", id, "err", err)
			}
		}()
		redirectNotice(w, r, back, notice)
		return
	}
	ctx := context.WithoutCancel(r.Context()) // the backup outlives the request
	go func() {
		if _, err := u.c.RunJob(ctx, id, "manual"); err != nil && !errors.Is(err, ErrRunning) {
			slog.Error("manual backup", "job", id, "err", err)
		}
	}()
	redirectNotice(w, r, back, "Backup started.")
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
	ctx := context.WithoutCancel(r.Context()) // the check outlives the request
	go func() {
		if _, err := u.c.RunCheck(ctx, id); err != nil && !errors.Is(err, ErrRunning) {
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
	n, err := u.c.DeleteJobOn(r.Context(), id, r.FormValue("snapshots") == "1", r.FormValue("on"))
	switch {
	case errors.Is(err, ErrRunning):
		redirectNotice(w, r, "/jobs/"+id, "Wait for the running backup to finish before deleting.")
		return
	case err != nil:
		redirectNotice(w, r, "/jobs/"+id, "Not deleted: "+err.Error())
		return
	case r.FormValue("snapshots") == "1":
		redirectNotice(w, r, "/jobs", fmt.Sprintf("Deleted job %s and %d snapshot(s). Their space is freed by a later maintenance run.", j.Name, n))
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
	Agents     []catalog.Agent
	On         string // agent ID the page browses and restores on; "" is the job's own
	ServerWhy  string // why the server cannot be chosen under "Restore on"; "" when it can
	// RestoreNote warns that files restored on the chosen host will not get
	// their original owners (it does not run as root); "" when they will.
	RestoreNote string
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
	p.Agents, _ = u.c.Catalog.ListAgents(ctx)
	if repo, err := u.c.Catalog.Repository(ctx, j.RepositoryID); err != nil {
		p.ServerWhy = err.Error()
	} else if err := u.c.ServerCanOpen(repo); err != nil {
		p.ServerWhy = err.Error()
	}
	if on := r.URL.Query().Get("on"); on != j.AgentID {
		p.On = on
	}
	host := p.On
	if host == "" {
		host = j.AgentID
	}
	p.RestoreNote = u.c.restoreHostNote(ctx, host)
	if p.Entries, err = u.c.Browse(ctx, j.ID, p.On, sid, rel); err != nil {
		p.Error = err.Error()
	}
	if j.Stack != nil && rel == "" {
		dumps, _ := u.c.Browse(ctx, j.ID, p.On, sid, "dumps")
		for _, d := range dumps {
			if name := d.GetName(); !d.GetDir() && d.GetSize() > 0 {
				p.Dumps = append(p.Dumps, strings.TrimSuffix(name, path.Ext(name)))
			}
		}
	}
	u.render(w, http.StatusOK, "browse", p)
}

// onQuery is the query-string tail that keeps the page on the agent picked
// with "Restore on".
func onQuery(on string) string {
	if on == "" {
		return ""
	}
	return "&on=" + url.QueryEscape(on)
}

func (u *ui) restore(w http.ResponseWriter, r *http.Request) {
	id, sid := r.PathValue("id"), r.PathValue("sid")
	if _, err := u.c.Catalog.Job(r.Context(), id); err != nil {
		http.NotFound(w, r)
		return
	}
	rel := strings.Trim(path.Clean("/"+r.FormValue("path")), "/")
	target, overwrite := strings.TrimSpace(r.FormValue("target")), r.FormValue("overwrite") != ""
	on := r.FormValue("on")
	back := "/jobs/" + id + "/snapshots/" + sid + "?path=" + url.QueryEscape(rel) + onQuery(on)
	if _, err := guard.RestoreTarget(target); err != nil {
		redirectNotice(w, r, back, err.Error())
		return
	}
	if u.c.IsRunning(id) {
		redirectNotice(w, r, back, ErrRunning.Error())
		return
	}
	ctx := context.WithoutCancel(r.Context()) // the restore outlives the request
	go func() {
		if _, err := u.c.Restore(ctx, id, on, sid, rel, target, overwrite); err != nil && !errors.Is(err, ErrRunning) {
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
	replyJSON(w, v)
}

// replyJSON sends v as the JSON body of a 200 reply.
func replyJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Debug("reply not delivered", "err", err) // the client went away
	}
}

// zoneLabel names the server's time zone as of now, the zone every displayed
// time and cron schedule is in: "UTC", or "CDT (UTC-05:00)", with the zone's
// name in front when TZ sets one ("America/Chicago · CDT (UTC-05:00)").
func zoneLabel(now time.Time) string {
	abbr, off := now.Local().Zone()
	label := abbr
	if off != 0 {
		sign := "+"
		if off < 0 {
			sign, off = "-", -off
		}
		label = fmt.Sprintf("%s (UTC%s%02d:%02d)", abbr, sign, off/3600, off%3600/60)
	}
	if name := os.Getenv("TZ"); name != "" && name != abbr {
		label = name + " · " + label
	}
	return label
}

func whenStr(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}

// took formats how long a run lasted, or "-" while it has no finish time.
func took(start, end time.Time) string {
	if start.IsZero() || end.IsZero() || end.Before(start) {
		return "-"
	}
	d := end.Sub(start).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
}

// tookRun is took for a run, less the time it waited for its agent.
func tookRun(r catalog.Run) string {
	if r.FinishedAt.IsZero() {
		return "-"
	}
	return took(r.StartedAt, r.StartedAt.Add(r.Took()))
}

// tookMaint is took for a maintenance cycle, less the time it waited for its agent.
func tookMaint(m catalog.MaintenanceRun) string {
	return took(m.StartedAt, m.StartedAt.Add(m.Took()))
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
