//go:build linux

package guard

import (
	"path/filepath"
	"testing"
)

func TestReadOnly(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{dir, filepath.Join(dir, "not", "created", "yet")} {
		if ro, err := ReadOnly(p); err != nil || ro {
			t.Errorf("ReadOnly(%s) = %v, %v; want false", p, ro, err)
		}
	}
	// Unprivileged containers (where the tests run) mount sysfs read-only.
	if ro, err := ReadOnly("/sys/kernel"); err == nil && !ro {
		t.Log("/sys is writable here; skipping the read-only positive case")
	} else if err != nil {
		t.Fatal(err)
	}
}
