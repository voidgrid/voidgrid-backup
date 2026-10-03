package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
	"github.com/voidgrid/voidgrid-backup/internal/guard"
	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
)

type vmView struct {
	VM       *agentpb.VM
	Existing string // job already backing up this VM on this agent
}

type vmsPage struct {
	base
	Agent catalog.Agent
	VMs   []vmView
	Repos []catalog.Repository
	Keep  catalog.Retention
}

func (u *ui) vms(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	agent, err := u.c.Catalog.Agent(ctx, r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	p := vmsPage{base: u.newBase(agent.Name+" VMs", "agents", r), Agent: agent, Keep: catalog.DefaultRetention}
	if p.Repos, err = u.c.Catalog.ListRepositories(ctx); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jobs, _ := u.c.Catalog.ListJobs(ctx)
	vms, err := u.c.VMs(ctx, agent.ID)
	if err != nil {
		p.Error = "Could not list VMs: " + err.Error()
	}
	for _, vm := range vms {
		v := vmView{VM: vm}
		for _, j := range jobs {
			if j.AgentID == agent.ID && j.VM != nil && j.VM.Name == vm.GetName() {
				v.Existing = j.Name
			}
		}
		p.VMs = append(p.VMs, v)
	}
	u.render(w, http.StatusOK, "vms", p)
}

func (u *ui) addVMJob(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f := r.PostForm
	agentID := f.Get("agent")
	vc := catalog.VMConfig{Name: f.Get("vm"), Disks: f["disk"], Quiesce: f.Get("quiesce") != ""}
	in := JobInput{Name: f.Get("name"), AgentID: agentID, RepositoryID: f.Get("repository"),
		VM: &vc, Schedule: f.Get("schedule"), Enabled: f.Get("enabled") != ""}
	var err error
	if len(vc.Disks) == 0 {
		err = errors.New("select at least one disk")
	}
	if err == nil {
		in.Keep, err = keepFromForm(f)
	}
	if err == nil {
		var job catalog.Job
		if job, err = u.c.AddJob(r.Context(), in); err == nil {
			redirectNotice(w, r, "/jobs/"+job.ID, "Created VM job "+job.Name+".")
			return
		}
	}
	redirectNotice(w, r, "/agents/"+agentID+"/vms", "Could not create the job for "+vc.Name+": "+err.Error())
}

func (u *ui) restoreVM(w http.ResponseWriter, r *http.Request) {
	id, sid := r.PathValue("id"), r.PathValue("sid")
	j, err := u.c.Catalog.Job(r.Context(), id)
	if err != nil || j.VM == nil {
		http.NotFound(w, r)
		return
	}
	back := "/jobs/" + id + "/snapshots/" + sid
	target := strings.TrimSpace(r.FormValue("target_dir"))
	if target == "" && r.FormValue("confirm") != j.VM.Name {
		redirectNotice(w, r, back, "Type the VM name to confirm overwriting its disks.")
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
	define := r.FormValue("define") != ""
	go func() {
		if _, err := u.c.RestoreVM(context.Background(), id, sid, target, define); err != nil && !errors.Is(err, ErrRunning) {
			slog.Error("restore VM", "job", id, "err", err)
		}
	}()
	redirectNotice(w, r, "/jobs/"+id, "VM restore started. The result appears under Runs.")
}
