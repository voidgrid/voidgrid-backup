package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/format"
	"github.com/kopia/kopia/repo/maintenance"
	"github.com/kopia/kopia/repo/manifest"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/snapshotmaintenance"
)

// DeleteSnapshot forgets one snapshot. Only this agent's own snapshots can be
// deleted. The data it referenced stays in storage until a later full
// maintenance cycle garbage-collects it.
func (e *Engine) DeleteSnapshot(ctx context.Context, r Repo, snapshotID string) error {
	rep, err := e.open(ctx, r)
	if err != nil {
		return err
	}
	defer rep.Close(ctx)

	id := manifest.ID(snapshotID)
	man, err := snapshot.LoadSnapshot(ctx, rep, id)
	if err != nil {
		return fmt.Errorf("load snapshot %s: %w", snapshotID, err)
	}
	if man.Source.Host != e.hostname || man.Source.UserName != Username {
		return fmt.Errorf("snapshot %s belongs to %s@%s, not to this agent", snapshotID, man.Source.UserName, man.Source.Host)
	}
	return repo.WriteSession(ctx, rep, repo.WriteSessionOptions{Purpose: "voidgrid-backup:delete-snapshot"},
		func(ctx context.Context, w repo.RepositoryWriter) error {
			return w.DeleteManifest(ctx, id)
		})
}

// Wipe deletes every blob of the repository at r's storage location and this
// agent's local cache of it. It needs no repository password. It refuses when
// no repository is found there (a wrong path would otherwise delete
// nothing, or something else), and deletes the repository's format blob last,
// so a wipe that fails halfway can be run again. Returns what was deleted.
// Only Kopia's own blob files are listed, so unrelated files survive, and
// the empty directories of a file-based store remain.
func (e *Engine) Wipe(ctx context.Context, r Repo) (blobs, bytes int64, err error) {
	if err := r.Config.Validate(); err != nil {
		return 0, 0, err
	}
	st, err := storage(ctx, r.Config, false)
	if err != nil {
		return 0, 0, err
	}
	defer st.Close(ctx)

	if _, err := st.GetMetadata(ctx, format.KopiaRepositoryBlobID); errors.Is(err, blob.ErrBlobNotFound) {
		return 0, 0, errors.New("no repository was found at this location; nothing was deleted")
	} else if err != nil {
		return 0, 0, fmt.Errorf("look for the repository: %w", err)
	}

	var all []blob.Metadata
	if err := st.ListBlobs(ctx, "", func(bm blob.Metadata) error {
		all = append(all, bm)
		return ctx.Err()
	}); err != nil {
		return 0, 0, fmt.Errorf("list blobs: %w", err)
	}
	sort.SliceStable(all, func(i, j int) bool {
		return all[j].BlobID == format.KopiaRepositoryBlobID && all[i].BlobID != format.KopiaRepositoryBlobID
	})
	for i, bm := range all {
		if err := ctx.Err(); err != nil {
			return blobs, bytes, err
		}
		if err := st.DeleteBlob(ctx, bm.BlobID); err != nil && !errors.Is(err, blob.ErrBlobNotFound) {
			return blobs, bytes, fmt.Errorf("delete %s after %d of %d blobs: %w", bm.BlobID, i, len(all), err)
		}
		blobs++
		bytes += bm.Length
		if blobs%1000 == 0 {
			slog.Info("repository wipe progress", "repo", r.ID, "deleted", blobs, "of", len(all))
		}
	}

	// Drop this agent's cached connection(s) to it.
	stale, _ := filepath.Glob(filepath.Join(e.dir, r.ID+"-*"))
	for _, d := range stale {
		if err := os.RemoveAll(d); err != nil {
			slog.Warn("remove local repository cache", "dir", d, "err", err)
		}
	}
	return blobs, bytes, nil
}

// Maintain runs a full maintenance cycle now. Kopia lets only the repository's
// maintenance owner do it: when this agent is not the owner nothing runs and
// the owner ("user@host", the host being the owning agent's ID) is returned.
// A full cycle does not free recently deleted data at once: Kopia's safety
// delays still apply.
func (e *Engine) Maintain(ctx context.Context, r Repo) (ran bool, owner string, err error) {
	rep, err := e.open(ctx, r)
	if err != nil {
		return false, "", err
	}
	defer rep.Close(ctx)
	dr, ok := rep.(repo.DirectRepository)
	if !ok {
		return false, "", errors.New("repository does not support maintenance")
	}
	if err := claimMaintenance(ctx, rep); err != nil {
		return false, "", err
	}
	err = repo.DirectWriteSession(ctx, dr, repo.WriteSessionOptions{Purpose: "voidgrid-backup:maintenance"},
		func(ctx context.Context, dw repo.DirectRepositoryWriter) error {
			return snapshotmaintenance.Run(ctx, dw, maintenance.ModeFull, false, maintenance.SafetyFull)
		})
	var notOwner maintenance.NotOwnedError
	if errors.As(err, &notOwner) {
		return false, notOwner.Owner, nil
	}
	return err == nil, "", err
}
