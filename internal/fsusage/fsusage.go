// Package fsusage measures how big a host path is: the filesystem it lives on
// (cheap) and, optionally, the files under it (a bounded walk).
package fsusage

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Limits bound a walk so one huge tree can't hang the UI.
type Limits struct {
	MaxFiles int           // stop after this many entries
	MaxTime  time.Duration // stop after this long
}

// DefaultLimits suit an interactive page load.
var DefaultLimits = Limits{MaxFiles: 2_000_000, MaxTime: 20 * time.Second}

// Usage is the measurement of one path.
type Usage struct {
	FSTotal, FSUsed, FSAvail int64 // filesystem holding the path
	Bytes, Files             int64 // under the path, same filesystem only
	Walked, Truncated        bool
}

// Measure returns the filesystem totals for p and, if walk is set, the size of
// the tree under it. The walk does not follow symlinks or cross onto another
// filesystem, and sets Truncated if it stops at a limit or when ctx ends.
func Measure(ctx context.Context, p string, walk bool, lim Limits) (Usage, error) {
	var u Usage
	var err error
	if u.FSTotal, u.FSUsed, u.FSAvail, err = statfs(p); err != nil {
		return u, err
	}
	if !walk {
		return u, nil
	}
	fi, err := os.Lstat(p)
	if err != nil {
		return u, err
	}
	u.Walked = true
	if !fi.IsDir() {
		u.Bytes, u.Files = fi.Size(), 1
		return u, nil
	}
	root, _ := deviceOf(fi)
	ctx, cancel := context.WithTimeout(ctx, lim.MaxTime)
	defer cancel()
	_ = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Unreadable entries are skipped, not fatal: the total is still useful.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if ctx.Err() != nil || u.Files >= int64(lim.MaxFiles) {
			u.Truncated = true
			return fs.SkipAll
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if d.IsDir() && path != p {
			if dev, ok := deviceOf(info); ok && dev != root {
				return fs.SkipDir
			}
		}
		if info.Mode().IsRegular() {
			u.Bytes += info.Size()
		}
		u.Files++
		return nil
	})
	return u, nil
}
