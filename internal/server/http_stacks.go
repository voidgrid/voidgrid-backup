package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
	"github.com/voidgrid/voidgrid-backup/internal/guard"
	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
)

// systemPrefixes are mount sources left unticked by default: host files a
// stack reads (timezone, sockets) rather than data it owns. Restoring them
// in place would overwrite the host's own files.
var systemPrefixes = []string{"/etc", "/var/run", "/run", "/usr", "/lib", "/boot", "/sys", "/proc", "/dev"}

type mountOpt struct {
	Source   string
	Services string
	Default  bool
	ReadOnly bool // mounted :ro in the agent: back up only, restore elsewhere
	Inside   bool // lives inside the compose directory, so the stack directory already holds it
}

type dumpOpt struct {
	Service, Suggested, Default string
}

type stackView struct {
	Stack    *agentpb.Stack
	Mounts   []mountOpt
	Dumps    []dumpOpt
	SQLite   []string
	Quiesce  string
	Existing string // name of a job already backing up this project on this agent
}

type stacksPage struct {
	base
	Agent  catalog.Agent
	Stacks []stackView
	Repos  []catalog.Repository
	Keep   catalog.Retention
}

func (u *ui) stacks(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	agent, err := u.c.Catalog.Agent(ctx, r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	p := stacksPage{base: u.newBase(agent.Name+" stacks", "agents", r), Agent: agent, Keep: catalog.DefaultRetention}
	if p.Repos, err = u.c.Catalog.ListRepositories(ctx); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jobs, _ := u.c.Catalog.ListJobs(ctx)
	stacks, err := u.c.Stacks(ctx, agent.ID)
	if err != nil {
		p.Error = "Could not list stacks: " + err.Error()
	}
	for _, st := range stacks {
		v := buildStackView(st)
		for _, j := range jobs {
			if j.AgentID == agent.ID && j.Stack != nil && j.Stack.Project == st.GetProject() {
				v.Existing = j.Name
			}
		}
		p.Stacks = append(p.Stacks, v)
	}
	u.render(w, http.StatusOK, "stacks", p)
}

func buildStackView(st *agentpb.Stack) stackView {
	v := stackView{Stack: st, Quiesce: "none"}
	dumped := map[string]bool{}
	for _, svc := range st.GetServices() {
		if k := svc.GetDumpKind(); k != "" {
			def := k
			if k == "redis" {
				def = "none" // usually a cache; opt in if it holds real data
			}
			v.Dumps = append(v.Dumps, dumpOpt{Service: svc.GetName(), Suggested: k, Default: def})
			dumped[svc.GetName()] = def != "none"
		}
		v.SQLite = append(v.SQLite, svc.GetSqliteFiles()...)
	}
	if len(v.SQLite) > 0 {
		v.Quiesce = "pause"
	}
	bySource := map[string]*mountOpt{}
	for _, svc := range st.GetServices() {
		for _, m := range svc.GetMounts() {
			mo := bySource[m.GetSource()]
			if mo == nil {
				mo = &mountOpt{Source: m.GetSource(), Default: defaultInclude(m.GetSource()), ReadOnly: m.GetAgentReadOnly(),
					Inside: st.GetWorkingDir() != "" && guard.Within(m.GetSource(), st.GetWorkingDir())}
				bySource[m.GetSource()] = mo
			}
			if mo.Services != "" {
				mo.Services += ", "
			}
			mo.Services += svc.GetName()
			if dumped[svc.GetName()] {
				mo.Default = false // the dump is the consistent copy
			}
		}
	}
	for _, mo := range bySource {
		v.Mounts = append(v.Mounts, *mo)
	}
	sort.Slice(v.Mounts, func(i, j int) bool { return v.Mounts[i].Source < v.Mounts[j].Source })
	return v
}

func defaultInclude(source string) bool {
	if _, err := guard.SourcePath(source); err != nil {
		return false
	}
	for _, p := range systemPrefixes {
		if guard.Within(source, p) {
			return false
		}
	}
	return !strings.HasSuffix(source, ".sock")
}

func (u *ui) addStackJob(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f := r.PostForm
	agentID := f.Get("agent")
	sc := stackConfigFromForm(f)
	sc.Project, sc.WorkingDir = f.Get("project"), f.Get("working_dir")
	in := JobInput{
		Name: f.Get("name"), AgentID: agentID, RepositoryID: f.Get("repository"),
		Stack: sc, Excludes: strings.Split(f.Get("excludes"), "\n"),
		Schedule: f.Get("schedule"), Enabled: f.Get("enabled") != "",
	}
	var err error
	if in.Keep, err = keepFromForm(f); err == nil {
		var job catalog.Job
		if job, err = u.c.AddJob(r.Context(), in); err == nil {
			redirectNotice(w, r, "/jobs/"+job.ID, "Created stack job "+job.Name+".")
			return
		}
	}
	redirectNotice(w, r, "/agents/"+agentID+"/stacks", "Could not create the job for "+sc.Project+": "+err.Error())
}

func keepFromForm(f url.Values) (catalog.Retention, error) {
	var k catalog.Retention
	for _, x := range []struct {
		field string
		dst   *int
	}{
		{"keep_latest", &k.Latest}, {"keep_hourly", &k.Hourly}, {"keep_daily", &k.Daily},
		{"keep_weekly", &k.Weekly}, {"keep_monthly", &k.Monthly}, {"keep_annual", &k.Annual},
	} {
		n, err := strconv.Atoi(strings.TrimSpace(f.Get(x.field)))
		if err != nil {
			return k, errors.New(strings.ReplaceAll(x.field, "_", " ") + " must be a number")
		}
		*x.dst = n
	}
	return k, nil
}

func (u *ui) restoreStack(w http.ResponseWriter, r *http.Request) {
	id, sid := r.PathValue("id"), r.PathValue("sid")
	j, err := u.c.Catalog.Job(r.Context(), id)
	if err != nil || j.Stack == nil {
		http.NotFound(w, r)
		return
	}
	back := "/jobs/" + id + "/snapshots/" + sid
	target := strings.TrimSpace(r.FormValue("target_root"))
	if target == "" && r.FormValue("confirm") != j.Stack.Project {
		redirectNotice(w, r, back, "Type the project name to confirm restoring over the live stack.")
		return
	}
	if target != "" {
		if _, err := guard.RestoreTarget(target); err != nil {
			redirectNotice(w, r, back, err.Error())
			return
		}
	}
	if u.c.IsRunning(id) {
		redirectNotice(w, r, back, ErrRunning.Error())
		return
	}
	stop := r.FormValue("stop") != ""
	go func() {
		if _, err := u.c.RestoreStack(context.Background(), id, sid, target, stop); err != nil && !errors.Is(err, ErrRunning) {
			slog.Error("restore stack", "job", id, "err", err)
		}
	}()
	redirectNotice(w, r, "/jobs/"+id, "Stack restore started. The result appears under Runs.")
}

func (u *ui) importDump(w http.ResponseWriter, r *http.Request) {
	id, sid, svc := r.PathValue("id"), r.PathValue("sid"), r.FormValue("service")
	j, err := u.c.Catalog.Job(r.Context(), id)
	if err != nil || j.Stack == nil {
		http.NotFound(w, r)
		return
	}
	if u.c.IsRunning(id) {
		redirectNotice(w, r, "/jobs/"+id+"/snapshots/"+sid, ErrRunning.Error())
		return
	}
	go func() {
		if _, err := u.c.ImportDump(context.Background(), id, sid, svc); err != nil && !errors.Is(err, ErrRunning) {
			slog.Error("import dump", "job", id, "err", err)
		}
	}()
	redirectNotice(w, r, "/jobs/"+id, "Importing the "+svc+" dump. The result appears under Runs.")
}
