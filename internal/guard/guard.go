// Package guard validates paths before anything is backed up or restored.
package guard

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// systemDirs are never valid sources or restore targets.
var systemDirs = []string{"/proc", "/sys", "/dev", "/run"}

// SourcePath cleans p and refuses the filesystem root, relative paths and
// kernel/runtime filesystems.
func SourcePath(p string) (string, error) {
	return check(p, "backup source")
}

// RestoreTarget applies the same rules to a restore destination.
func RestoreTarget(p string) (string, error) {
	return check(p, "restore target")
}

func check(p, what string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", fmt.Errorf("%s is empty", what)
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("%s %q must be an absolute path", what, p)
	}
	p = filepath.Clean(p)
	if p == "/" {
		return "", errors.New("the filesystem root (/) is never allowed as a " + what)
	}
	for _, d := range systemDirs {
		if Within(p, d) {
			return "", fmt.Errorf("%s %q is under %s", what, p, d)
		}
	}
	return p, nil
}

// Within reports whether p is dir or inside it. Both must be clean.
func Within(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, dir+"/")
}
