package guard

import "testing"

func TestSourcePath(t *testing.T) {
	for in, want := range map[string]string{
		"/srv/data":        "/srv/data",
		"/srv/data/":       "/srv/data",
		" /a/b/../c ":      "/a/c",
		"/processes":       "/processes",
		"/devices/storage": "/devices/storage",
	} {
		got, err := SourcePath(in)
		if err != nil || got != want {
			t.Errorf("SourcePath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "/", "//", "/a/..", "relative", "./x", "/proc", "/proc/1", "/sys/fs", "/dev", "/run/docker.sock"} {
		if got, err := SourcePath(bad); err == nil {
			t.Errorf("SourcePath(%q) = %q, want an error", bad, got)
		}
	}
	if _, err := RestoreTarget("/"); err == nil {
		t.Error("RestoreTarget(/) allowed")
	}
}
