package guard

import (
	"errors"
	"io/fs"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// ReadOnly reports whether p (or, if p doesn't exist yet, its nearest
// existing parent) is on a filesystem mounted read-only in this process's
// view, e.g. a bind mount given to the agent with :ro.
func ReadOnly(p string) (bool, error) {
	p = filepath.Clean(p)
	for {
		var st unix.Statfs_t
		err := unix.Statfs(p, &st)
		if err == nil {
			return st.Flags&unix.ST_RDONLY != 0, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
		parent := filepath.Dir(p)
		if parent == p {
			return false, err
		}
		p = parent
	}
}
