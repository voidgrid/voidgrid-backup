package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
)

// statusLine is one row of the Status page: what it is about, one short
// sentence, how worried to be, and where to go to look closer.
type statusLine struct {
	Label string
	Text  string
	Level string // "ok", "warn", "bad" or "muted"
	Href  string
	Ack   string // when set, a button that posts here to acknowledge the problem
}

type statusPage struct {
	base
	Lines   []statusLine
	Summary string
	Level   string
}

// worse is the more worrying of two levels.
func worse(a, b string) string {
	rank := map[string]int{"muted": 0, "ok": 1, "warn": 2, "bad": 3}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// plural is "1 job" or "2 jobs".
func plural(n int, one string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", one)
	}
	return fmt.Sprintf("%d %ss", n, one)
}

// some names up to three things and counts the rest: "a, b, c and 2 more".
func some(names []string) string {
	if len(names) <= 3 {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:3], ", ") + fmt.Sprintf(" and %d more", len(names)-3)
}

// status is the home page: one line each for agents, jobs, repositories and the
// next backup, each a link, so the state of the whole system is visible at a
// glance. It reads only the catalog and never contacts an agent or a repository.
func (u *ui) status(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	agents, err := u.c.Catalog.ListAgents(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	regs, _ := u.c.Catalog.ListRegistrations(ctx)
	jobs, err := u.c.Catalog.ListJobs(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	repos, err := u.c.Catalog.ListRepositories(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Agents.
	var down []string
	for _, a := range agents {
		if a.LastError != "" {
			down = append(down, a.Name)
		}
	}
	waiting := 0
	for _, reg := range regs {
		if reg.Status == catalog.RegPending {
			waiting++
		}
	}
	al := statusLine{Label: "Agents", Href: "/agents", Level: "ok"}
	switch {
	case len(agents) == 0:
		al.Text, al.Level = "none yet", "muted"
	case len(down) > 0:
		al.Text, al.Level = fmt.Sprintf("%d of %d reachable · %s unreachable", len(agents)-len(down), len(agents), some(down)), "bad"
	default:
		al.Text = fmt.Sprintf("%d of %d reachable", len(agents), len(agents))
	}
	if waiting > 0 {
		al.Text += fmt.Sprintf(" · %d waiting for approval", waiting)
		al.Level = worse(al.Level, "warn")
	}

	// Jobs.
	rows := make([]jobRow, 0, len(jobs))
	for _, j := range jobs {
		rows = append(rows, u.jobRow(ctx, j, agents, repos))
	}
	var okN, partialN, failedN, neverN, runningN int
	var attn []statusLine // restores, checks and imports that did not succeed and nobody has acknowledged
	var problems []string
	var next time.Time
	var nextName string
	for _, row := range rows {
		if row.Running {
			runningN++
		}
		switch {
		case row.Last == nil:
			neverN++
		case row.Last.Status == catalog.RunFailed:
			failedN++
			problems = append(problems, row.Name+" failed "+ago(row.Last.StartedAt))
		case row.Last.Status == catalog.RunPartial:
			partialN++
			problems = append(problems, row.Name+" partial "+ago(row.Last.StartedAt))
		default:
			okN++
		}
		// A restore, check or import that did not succeed is a problem too, though
		// the job's last backup was fine. It shows until a later run of that kind
		// succeeds or someone acknowledges it.
		for _, o := range u.attention(ctx, row.ID) {
			lvl := "warn"
			if o.Status == catalog.RunFailed {
				lvl = "bad"
			}
			attn = append(attn, statusLine{Label: strings.ToUpper(o.Kind[:1]) + o.Kind[1:], Level: lvl,
				Text: fmt.Sprintf("%s of %s %s %s", o.Kind, row.Name, o.Status, ago(o.StartedAt)),
				Href: "/jobs/" + row.ID, Ack: fmt.Sprintf("/jobs/%s/runs/%d/ack", row.ID, o.ID)})
		}
		if row.Enabled && !row.Next.IsZero() && (next.IsZero() || row.Next.Before(next)) {
			next, nextName = row.Next, row.Name
		}
	}
	jl := statusLine{Label: "Jobs", Href: "/jobs", Level: "ok"}
	switch {
	case len(rows) == 0:
		jl.Text, jl.Level = "none yet", "muted"
	default:
		parts := []string{plural(len(rows), "job")}
		if failedN == 0 && partialN == 0 && neverN == 0 {
			parts = append(parts, "last backups all ok")
		} else {
			parts = append(parts, fmt.Sprintf("%d ok", okN))
			if partialN > 0 {
				parts = append(parts, fmt.Sprintf("%d partial", partialN))
			}
			if failedN > 0 {
				parts = append(parts, fmt.Sprintf("%d failed", failedN))
			}
			if neverN > 0 {
				parts = append(parts, fmt.Sprintf("%d never run", neverN))
			}
		}
		if runningN > 0 {
			parts = append(parts, fmt.Sprintf("%d running now", runningN))
		}
		jl.Text = strings.Join(parts, " · ")
		if len(problems) > 0 {
			jl.Text += " (" + some(problems) + ")"
		}
		switch {
		case failedN > 0:
			jl.Level = "bad"
		case partialN > 0:
			jl.Level = "warn"
		}
	}

	// Repositories.
	var maintFailed, notInitialized []string
	for _, rp := range repos {
		if rp.InitializedAt.IsZero() {
			notInitialized = append(notInitialized, rp.Name)
			continue
		}
		if m, ok := u.c.RepoMaintenance(ctx, rp.ID); ok && m.Status == "failed" {
			maintFailed = append(maintFailed, rp.Name)
		}
	}
	rl := statusLine{Label: "Repositories", Href: "/repositories", Level: "ok"}
	switch {
	case len(repos) == 0:
		rl.Text, rl.Level = "none yet", "muted"
	default:
		rl.Text = plural(len(repos), "repository")
		if len(maintFailed) == 0 && len(notInitialized) == 0 {
			rl.Text += " · maintenance ok"
		}
		if len(maintFailed) > 0 {
			rl.Text += " · maintenance failed on " + some(maintFailed)
			rl.Level = "bad"
		}
		if len(notInitialized) > 0 {
			rl.Text += " · not set up: " + some(notInitialized)
			rl.Level = worse(rl.Level, "warn")
		}
	}

	// The next scheduled backup.
	nl := statusLine{Label: "Next backup", Href: "/schedule", Level: "muted"}
	if next.IsZero() {
		nl.Text = "nothing scheduled"
	} else {
		nl.Text, nl.Level = whenStr(next)+" · "+nextName, "ok"
	}

	p := statusPage{base: u.newBase("Status", "home", r), Lines: append(append([]statusLine{al, jl}, attn...), rl, nl)}
	p.Level = "ok"
	attention := 0
	for _, l := range p.Lines {
		p.Level = worse(p.Level, l.Level)
		if l.Level == "bad" || l.Level == "warn" {
			attention++
		}
	}
	switch attention {
	case 0:
		p.Summary = "all good"
	case 1:
		p.Summary = "1 thing needs attention"
	default:
		p.Summary = fmt.Sprintf("%d things need attention", attention)
	}
	u.render(w, http.StatusOK, "status", p)
}

// attention is the latest restore, check and import of a job that did not
// succeed and that nobody has acknowledged. A later success of the same kind
// replaces it.
func (u *ui) attention(ctx context.Context, jobID string) []catalog.Run {
	var out []catalog.Run
	for _, kind := range []string{"restore", "check", "import"} {
		run, err := u.c.Catalog.LastRun(ctx, jobID, kind)
		if err != nil || !run.AckedAt.IsZero() {
			continue
		}
		if run.Status == catalog.RunFailed || run.Status == catalog.RunPartial {
			out = append(out, run)
		}
	}
	return out
}
