// Package caps reports which Linux capabilities the agent has, so missing
// permissions show up in the UI instead of as silent partial backups or
// restores with the wrong ownership.
package caps

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Capability bits (linux/capability.h).
const (
	Chown         = 0
	DACOverride   = 1
	DACReadSearch = 2
	FOwner        = 3
	FSetID        = 4
)

// Needed lists what the agent needs for full backups and faithful restores.
var Needed = []struct {
	Bit  uint
	Name string
	Why  string
}{
	{DACReadSearch, "CAP_DAC_READ_SEARCH", "read files and directories it does not own"},
	{DACOverride, "CAP_DAC_OVERRIDE", "write restored files into directories it does not own"},
	{Chown, "CAP_CHOWN", "restore file ownership"},
	{FOwner, "CAP_FOWNER", "restore modes and times on files it does not own"},
	{FSetID, "CAP_FSETID", "keep setuid/setgid bits on restore"},
}

type Report struct {
	UID     int
	Eff     uint64
	Missing []string // "CAP_X: why"
}

func (r Report) Has(bit uint) bool { return r.Eff&(1<<bit) != 0 }

// Current reads the effective capability set of this process.
func Current() (Report, error) {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return Report{UID: os.Geteuid()}, err
	}
	defer f.Close() //nolint:errcheck // read-only file
	r := Report{UID: os.Geteuid()}
	found := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "CapEff:"); ok {
			if r.Eff, err = strconv.ParseUint(strings.TrimSpace(v), 16, 64); err != nil {
				return r, fmt.Errorf("CapEff: %w", err)
			}
			found = true
		}
	}
	if !found {
		return r, fmt.Errorf("no CapEff in /proc/self/status")
	}
	for _, n := range Needed {
		if !r.Has(n.Bit) {
			r.Missing = append(r.Missing, n.Name+": cannot "+n.Why)
		}
	}
	return r, nil
}
