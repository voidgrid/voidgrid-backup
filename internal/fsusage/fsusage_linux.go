package fsusage

import (
	"io/fs"
	"syscall"

	"golang.org/x/sys/unix"
)

func statfs(p string) (total, used, avail int64, err error) {
	var st unix.Statfs_t
	if err = unix.Statfs(p, &st); err != nil {
		return 0, 0, 0, err
	}
	bs := int64(st.Bsize)
	total = int64(st.Blocks) * bs
	avail = int64(st.Bavail) * bs
	used = total - int64(st.Bfree)*bs
	return total, used, avail, nil
}

func deviceOf(fi fs.FileInfo) (uint64, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Dev), true
}
