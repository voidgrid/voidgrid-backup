//go:build !linux

package fsusage

import (
	"errors"
	"io/fs"
)

// The agent only runs on Linux; these keep the package building elsewhere.
func statfs(string) (int64, int64, int64, error) {
	return 0, 0, 0, errors.New("filesystem usage is only available on Linux")
}

func deviceOf(fs.FileInfo) (uint64, bool) { return 0, false }
