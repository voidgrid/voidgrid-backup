package server

import (
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
	"github.com/voidgrid/voidgrid-backup/internal/guard"
)

type editMount struct {
	Source, Services string
	Checked          bool
	Missing          bool // in the job but not found on the agent right now
	ReadOnly         bool
	Inside           bool // inside the compose directory
}

type editDump struct {
	Service, Detected, Selected string
}

type editDisk struct {
	Target, Source string
	Checked        bool
	Missing        bool
}

type editPage struct {
	base
	Job    catalog.Job
	Mounts []editMount
	Dumps  []editDump
	Disks  []editDisk
	// Live is false when the agent couldn't be asked what exists now, so the
	// form shows only what the job already has.
	Live bool
}

// editJob shows the edit form. What the agent has right now (mounts, dumps,
// disks) is merged with what the job already lists, so nothing the job backs
// up is dropped just because the agent can't see it at the moment.
func (u *ui) editJob(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	j, err := u.c.Catalog.Job(ctx, r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	p := editPage{base: u.newBase("Edit "+j.Name, "jobs", r), Job: j, Live: true}
	switch {
	case j.Stack != nil:
		u.editStackView(r, &p)
	case j.VM != nil:
		u.editVMView(r, &p)
	}
	u.render(w, http.StatusOK, "jobedit", p)
}

func (u *ui) editStackView(r *http.Request, p *editPage) {
	st := p.Job.Stack
	stacks, err := u.c.Stacks(r.Context(), p.Job.AgentID)
	if err != nil {
		p.Live, p.Error = false, "Could not reach the agent ("+err.Error()+"), so only what the job already has is shown."
	}
	seen := map[string]bool{}
	dumpSeen := map[string]bool{}
	for _, s := range stacks {
		if s.GetProject() != st.Project {
			continue
		}
		v := buildStackView(s)
		for _, m := range v.Mounts {
			seen[m.Source] = true
			checked := m.Default
			if slices.Contains(st.Include, m.Source) {
				checked = true
			} else if slices.Contains(st.Exclude, m.Source) {
				checked = false
			}
			p.Mounts = append(p.Mounts, editMount{Source: m.Source, Services: m.Services, Checked: checked, ReadOnly: m.ReadOnly, Inside: m.Inside})
		}
		for _, d := range v.Dumps {
			dumpSeen[d.Service] = true
			sel := st.Dumps[d.Service]
			if sel == "" {
				sel = "none"
			}
			p.Dumps = append(p.Dumps, editDump{Service: d.Service, Detected: d.Suggested, Selected: sel})
		}
	}
	for _, src := range st.Include {
		if !seen[src] {
			seen[src] = true
			p.Mounts = append(p.Mounts, editMount{Source: src, Checked: true, Missing: true, Inside: guard.Within(src, st.WorkingDir)})
		}
	}
	for _, src := range st.Exclude {
		if !seen[src] {
			seen[src] = true
			p.Mounts = append(p.Mounts, editMount{Source: src, Missing: true, Inside: guard.Within(src, st.WorkingDir)})
		}
	}
	for svc, kind := range st.Dumps {
		if !dumpSeen[svc] {
			p.Dumps = append(p.Dumps, editDump{Service: svc, Selected: kind})
		}
	}
}

func (u *ui) editVMView(r *http.Request, p *editPage) {
	vc := p.Job.VM
	vms, err := u.c.VMs(r.Context(), p.Job.AgentID)
	if err != nil {
		p.Live, p.Error = false, "Could not reach the agent ("+err.Error()+"), so only what the job already has is shown."
	}
	seen := map[string]bool{}
	for _, vm := range vms {
		if vm.GetName() != vc.Name {
			continue
		}
		for _, d := range vm.GetDisks() {
			if !d.GetBackupable() {
				continue
			}
			seen[d.GetTarget()] = true
			// An empty saved list means every backupable disk.
			checked := len(vc.Disks) == 0 || slices.Contains(vc.Disks, d.GetTarget())
			p.Disks = append(p.Disks, editDisk{Target: d.GetTarget(), Source: d.GetSource(), Checked: checked})
		}
	}
	for _, t := range vc.Disks {
		if !seen[t] {
			p.Disks = append(p.Disks, editDisk{Target: t, Checked: true, Missing: true})
		}
	}
}

// saveJob applies the edit form. The form is read according to the job's own
// kind, so a crafted post can't turn a path job into a stack job.
func (u *ui) saveJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	j, err := u.c.Catalog.Job(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f := r.PostForm
	back := "/jobs/" + id + "/edit"
	in := JobInput{Name: f.Get("name"), Schedule: f.Get("schedule"), Enabled: f.Get("enabled") != "",
		Excludes: strings.Split(f.Get("excludes"), "\n")}
	if in.Keep, err = keepFromForm(f); err != nil {
		redirectNotice(w, r, back, "Not saved: "+err.Error())
		return
	}
	switch {
	case j.Stack != nil:
		in.Stack = stackConfigFromForm(f)
	case j.VM != nil:
		in.VM = &catalog.VMConfig{Disks: f["disk"], Quiesce: f.Get("quiesce") != ""}
		if len(in.VM.Disks) == 0 {
			redirectNotice(w, r, back, "Not saved: select at least one disk.")
			return
		}
	default:
		in.Paths = strings.Split(f.Get("paths"), "\n")
	}
	if _, err := u.c.UpdateJob(r.Context(), id, in); err != nil {
		redirectNotice(w, r, back, "Not saved: "+err.Error())
		return
	}
	redirectNotice(w, r, "/jobs/"+id, "Saved. The change applies from the next backup.")
}

// stackConfigFromForm reads the mount, dump and quiesce fields shared by the
// create and edit forms. Every "mount" field not ticked in "include" is left
// out on purpose, which is what lets a later edit tell it from a new mount.
func stackConfigFromForm(f url.Values) *catalog.StackConfig {
	included := map[string]bool{}
	for _, s := range f["include"] {
		included[s] = true
	}
	sc := &catalog.StackConfig{Dumps: map[string]string{}, Quiesce: f.Get("quiesce")}
	for _, m := range f["mount"] {
		if included[m] {
			sc.Include = append(sc.Include, m)
		} else {
			sc.Exclude = append(sc.Exclude, m)
		}
	}
	for k, v := range f {
		if svc, ok := strings.CutPrefix(k, "dump:"); ok && len(v) > 0 {
			sc.Dumps[svc] = v[0]
		}
	}
	return sc
}
