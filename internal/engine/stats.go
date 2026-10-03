package engine

import (
	"context"
	"fmt"

	"github.com/kopia/kopia/repo/blob"

	"github.com/voidgrid/voidgrid-backup/internal/fsusage"
	"github.com/voidgrid/voidgrid-backup/internal/repocfg"
)

// RepoStats is the space a repository occupies in its storage.
type RepoStats struct {
	StoredBytes, Blobs int64
	// Only for filesystem repositories; 0 otherwise.
	FSTotal, FSAvail int64
}

// RepoStats totals the size of every blob in the repository's storage. It
// lists the whole storage, so on S3 and SFTP the cost grows with blob count.
func (e *Engine) RepoStats(ctx context.Context, r Repo) (RepoStats, error) {
	var out RepoStats
	if err := r.Config.Validate(); err != nil {
		return out, err
	}
	st, err := storage(ctx, r.Config, false)
	if err != nil {
		return out, err
	}
	defer st.Close(ctx)
	err = st.ListBlobs(ctx, "", func(bm blob.Metadata) error {
		out.StoredBytes += bm.Length
		out.Blobs++
		return nil
	})
	if err != nil {
		return out, fmt.Errorf("list blobs: %w", err)
	}
	if r.Config.Kind == repocfg.KindFilesystem {
		if u, err := fsusage.Measure(ctx, r.Config.Filesystem.Path, false, fsusage.Limits{}); err == nil {
			out.FSTotal, out.FSAvail = u.FSTotal, u.FSAvail
		}
	}
	return out, nil
}
