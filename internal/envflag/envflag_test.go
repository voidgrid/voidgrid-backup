package envflag

import (
	"flag"
	"testing"
	"time"
)

// The environment sets a flag's value, not its default: -help prints the
// defaults, and a secret from the environment must not be in it.
func TestEnvironmentSetsTheValueNotTheDefault(t *testing.T) {
	t.Setenv("VB_TEST_SECRET", "hunter2")
	t.Setenv("VB_TEST_POLL", "45s")
	t.Setenv("VB_TEST_ON", "true")
	t.Setenv("VB_TEST_N", "7")
	s := String("test-secret", "VB_TEST_SECRET", "", "a secret")
	d := Duration("test-poll", "VB_TEST_POLL", 30*time.Second, "how often")
	b := Bool("test-on", "VB_TEST_ON", false, "on or off")
	n := Int("test-n", "VB_TEST_N", 3, "how many")
	unset := String("test-unset", "VB_TEST_UNSET", "fallback", "not in the environment")
	if *s != "hunter2" || *d != 45*time.Second || !*b || *n != 7 || *unset != "fallback" {
		t.Fatalf("values: %q %v %v %d %q", *s, *d, *b, *n, *unset)
	}
	for name, def := range map[string]string{"test-secret": "", "test-poll": "30s", "test-on": "false", "test-n": "3"} {
		if got := flag.Lookup(name).DefValue; got != def {
			t.Errorf("-%s: default shown as %q, want %q", name, got, def)
		}
	}
	// A flag on the command line still wins.
	if err := flag.CommandLine.Parse([]string{"-test-secret", "from-the-command-line"}); err != nil || *s != "from-the-command-line" {
		t.Fatalf("%q %v", *s, err)
	}
}
