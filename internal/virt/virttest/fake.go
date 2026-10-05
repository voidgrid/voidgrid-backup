// Package virttest provides a fake Hypervisor. Snapshots create real overlay
// files next to the disks and commits remove them, like libvirt does.
package virttest

import (
	"context"
	"os"
	"strings"
	"sync"

	"github.com/voidgrid/voidgrid-backup/internal/virt"
)

type Fake struct {
	Mu          sync.Mutex
	Dom         *virt.Domain // nil: no such domain
	DomainXML   string
	QuiesceErr  error // returned by a quiescing snapshot (no guest agent)
	CommitErr   error
	KeepOverlay bool // a commit leaves the overlay file behind
	Calls       []string
	Overlays    []string
	OnOverlay   bool // disks are currently on overlays
	Defined     string
}

func (f *Fake) Domains(ctx context.Context) ([]virt.Domain, error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if f.Dom == nil {
		return nil, nil
	}
	return []virt.Domain{*f.Dom}, nil
}

func (f *Fake) Domain(_ context.Context, name string) (virt.Domain, error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if f.Dom == nil || name != f.Dom.Name {
		return virt.Domain{}, virt.ErrNoDomain
	}
	return *f.Dom, nil
}

func (f *Fake) XML(context.Context, string, bool) (string, error) { return f.DomainXML, nil }

func (f *Fake) SnapshotDisks(_ context.Context, _, snapXML string, quiesce bool) error {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if quiesce {
		f.Calls = append(f.Calls, "snapshot-quiesce")
		if f.QuiesceErr != nil {
			return f.QuiesceErr
		}
	} else {
		f.Calls = append(f.Calls, "snapshot")
	}
	for _, part := range strings.Split(snapXML, `file="`)[1:] {
		p := part[:strings.Index(part, `"`)]
		if err := os.WriteFile(p, []byte("overlay"), 0o644); err != nil {
			return err
		}
		f.Overlays = append(f.Overlays, p)
	}
	f.OnOverlay = true
	return nil
}

func (f *Fake) BlockCommit(_ context.Context, _, disk string) error {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.Calls = append(f.Calls, "commit "+disk)
	if f.CommitErr != nil {
		return f.CommitErr
	}
	if !f.KeepOverlay {
		for _, p := range f.Overlays {
			os.Remove(p)
		}
	}
	f.OnOverlay = false
	return nil
}

func (f *Fake) Define(_ context.Context, x string) error {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.Defined = x
	return nil
}
