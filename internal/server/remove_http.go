package server

import (
	"errors"
	"net/http"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
)

func (u *ui) removeRepository(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rp, err := u.c.Catalog.Repository(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if r.FormValue("confirm") != rp.Name {
		redirectNotice(w, r, "/repositories", "Type the repository name to confirm removing it.")
		return
	}
	if err := u.c.RemoveRepository(r.Context(), id); err != nil {
		redirectNotice(w, r, "/repositories", "Not removed: "+err.Error())
		return
	}
	redirectNotice(w, r, "/repositories", "Removed "+rp.Name+" from the server. Its data in storage was not touched.")
}

func (u *ui) wipeRepository(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rp, err := u.c.Catalog.Repository(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if r.FormValue("confirm") != rp.Name {
		redirectNotice(w, r, "/repositories", "Type the repository name to confirm wiping it.")
		return
	}
	if r.FormValue("agent") == "" {
		redirectNotice(w, r, "/repositories", "Pick the agent that will delete the data.")
		return
	}
	if err := u.c.StartWipe(r.Context(), id, r.FormValue("agent")); err != nil {
		redirectNotice(w, r, "/repositories", "Not wiped: "+err.Error())
		return
	}
	redirectNotice(w, r, "/repositories", "Wiping "+rp.Name+" in the background. It is removed from the list when the data is gone.")
}

func (u *ui) deleteSnapshot(w http.ResponseWriter, r *http.Request) {
	id, sid := r.PathValue("id"), r.PathValue("sid")
	if _, err := u.c.Catalog.Job(r.Context(), id); err != nil {
		http.NotFound(w, r)
		return
	}
	if r.FormValue("confirm") != shortID(sid) {
		redirectNotice(w, r, "/jobs/"+id, "Type the snapshot ID shown in the box to confirm deleting it.")
		return
	}
	if err := u.c.DeleteSnapshot(r.Context(), id, sid); err != nil {
		if errors.Is(err, catalog.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		redirectNotice(w, r, "/jobs/"+id, "Not deleted: "+err.Error())
		return
	}
	redirectNotice(w, r, "/jobs/"+id, "Deleted snapshot "+shortID(sid)+". Its space is freed by a later maintenance run.")
}
