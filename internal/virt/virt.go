// Package virt is the libvirt side of VM backups: list domains and their
// disks, take external disk-only snapshots, and merge them back.
package virt

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/digitalocean/go-libvirt"
	"github.com/digitalocean/go-libvirt/socket/dialers"
)

var ErrNoDomain = errors.New("no such domain")

// Domain states as reported here.
const (
	StateRunning = "running"
	StatePaused  = "paused"
	StateShutoff = "shutoff"
	StateOther   = "other"
)

type Domain struct {
	Name  string
	UUID  string
	State string
	Disks []Disk
}

// Active reports whether the domain has a running QEMU process.
func (d Domain) Active() bool { return d.State == StateRunning || d.State == StatePaused }

type Disk struct {
	Target   string // vda, sdb, ...
	Device   string // disk, cdrom, ...
	Type     string // file, block, network, volume
	Source   string // host path for type=file
	Format   string // qcow2, raw, ...
	ReadOnly bool
	Backing  bool // has a backing chain below Source
}

// Backupable reports whether the disk can be snapshotted and read as a file.
func (d Disk) Backupable() bool {
	return d.Device == "disk" && d.Type == "file" && d.Source != "" && !d.ReadOnly
}

// Hypervisor is what the backup engine needs from libvirt.
type Hypervisor interface {
	Domains(ctx context.Context) ([]Domain, error)
	Domain(ctx context.Context, name string) (Domain, error)
	// XML returns the domain definition; inactive gives the persistent config.
	XML(ctx context.Context, name string, inactive bool) (string, error)
	// SnapshotDisks creates an external disk-only snapshot described by
	// snapshotXML, freezing guest filesystems through the guest agent when
	// quiesce is set.
	SnapshotDisks(ctx context.Context, name, snapshotXML string, quiesce bool) error
	// BlockCommit merges the active overlay of disk back into its base and
	// pivots the domain onto the base again. It waits until done.
	BlockCommit(ctx context.Context, name, disk string) error
	Define(ctx context.Context, domainXML string) error
}

// ParseDisks reads the disks from a domain's XML.
func ParseDisks(domainXML string) ([]Disk, error) {
	var d struct {
		Disks []struct {
			Type   string `xml:"type,attr"`
			Device string `xml:"device,attr"`
			Driver struct {
				Type string `xml:"type,attr"`
			} `xml:"driver"`
			Source struct {
				File string `xml:"file,attr"`
				Dev  string `xml:"dev,attr"`
			} `xml:"source"`
			Target struct {
				Dev string `xml:"dev,attr"`
			} `xml:"target"`
			ReadOnly     *struct{} `xml:"readonly"`
			BackingStore *struct {
				Type string `xml:"type,attr"`
			} `xml:"backingStore"`
		} `xml:"devices>disk"`
	}
	if err := xml.Unmarshal([]byte(domainXML), &d); err != nil {
		return nil, fmt.Errorf("domain XML: %w", err)
	}
	out := make([]Disk, 0, len(d.Disks))
	for _, x := range d.Disks {
		disk := Disk{
			Target:   x.Target.Dev,
			Device:   x.Device,
			Type:     x.Type,
			Source:   x.Source.File,
			Format:   x.Driver.Type,
			ReadOnly: x.ReadOnly != nil,
			Backing:  x.BackingStore != nil && x.BackingStore.Type != "",
		}
		if disk.Device == "" {
			disk.Device = "disk"
		}
		if disk.Source == "" {
			disk.Source = x.Source.Dev
		}
		out = append(out, disk)
	}
	return out, nil
}

// SnapshotXML describes an external disk-only snapshot: each disk in
// overlays gets a new overlay file, every other disk is left alone.
func SnapshotXML(name string, all []Disk, overlays map[string]string) string {
	var b strings.Builder
	b.WriteString("<domainsnapshot><name>")
	xml.EscapeText(&b, []byte(name)) //nolint:errcheck // writes to a buffer, which never fails
	b.WriteString("</name><description>voidgrid-backup</description><disks>")
	for _, d := range all {
		b.WriteString(`<disk name="`)
		xml.EscapeText(&b, []byte(d.Target)) //nolint:errcheck // writes to a buffer, which never fails
		if ov, ok := overlays[d.Target]; ok {
			b.WriteString(`" snapshot="external"><driver type="qcow2"/><source file="`)
			xml.EscapeText(&b, []byte(ov)) //nolint:errcheck // writes to a buffer, which never fails
			b.WriteString(`"/></disk>`)
		} else {
			b.WriteString(`" snapshot="no"/>`)
		}
	}
	b.WriteString("</disks></domainsnapshot>")
	return b.String()
}

// Libvirt talks to libvirtd/virtqemud over its Unix socket. Each call opens
// its own connection so a restarted daemon never leaves the agent stuck.
type Libvirt struct {
	Socket string
	URI    string
	mu     sync.Mutex
}

func (v *Libvirt) conn() (*libvirt.Libvirt, error) {
	l := libvirt.NewWithDialer(dialers.NewLocal(dialers.WithSocket(v.Socket), dialers.WithLocalTimeout(5*time.Second)))
	if err := l.ConnectToURI(libvirt.ConnectURI(v.URI)); err != nil {
		return nil, fmt.Errorf("libvirt %s via %s: %w", v.URI, v.Socket, err)
	}
	return l, nil
}

func (v *Libvirt) with(fn func(*libvirt.Libvirt) error) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	l, err := v.conn()
	if err != nil {
		return err
	}
	defer l.Disconnect()
	return fn(l)
}

func (v *Libvirt) Domains(ctx context.Context) ([]Domain, error) {
	var out []Domain
	err := v.with(func(l *libvirt.Libvirt) error {
		doms, _, err := l.ConnectListAllDomains(1, libvirt.ConnectListDomainsActive|libvirt.ConnectListDomainsInactive)
		if err != nil {
			return err
		}
		for _, d := range doms {
			dom, err := describe(l, d)
			if err != nil {
				return err
			}
			out = append(out, dom)
		}
		return nil
	})
	return out, err
}

func (v *Libvirt) Domain(ctx context.Context, name string) (Domain, error) {
	var out Domain
	err := v.with(func(l *libvirt.Libvirt) error {
		d, err := lookup(l, name)
		if err != nil {
			return err
		}
		out, err = describe(l, d)
		return err
	})
	return out, err
}

func (v *Libvirt) XML(ctx context.Context, name string, inactive bool) (string, error) {
	var out string
	err := v.with(func(l *libvirt.Libvirt) error {
		d, err := lookup(l, name)
		if err != nil {
			return err
		}
		var flags libvirt.DomainXMLFlags
		if inactive {
			flags = libvirt.DomainXMLInactive
		}
		out, err = l.DomainGetXMLDesc(d, flags)
		return err
	})
	return out, err
}

func (v *Libvirt) SnapshotDisks(ctx context.Context, name, snapshotXML string, quiesce bool) error {
	return v.with(func(l *libvirt.Libvirt) error {
		d, err := lookup(l, name)
		if err != nil {
			return err
		}
		flags := libvirt.DomainSnapshotCreateDiskOnly | libvirt.DomainSnapshotCreateAtomic | libvirt.DomainSnapshotCreateNoMetadata
		if quiesce {
			flags |= libvirt.DomainSnapshotCreateQuiesce
		}
		_, err = l.DomainSnapshotCreateXML(d, snapshotXML, uint32(flags))
		return err
	})
}

func (v *Libvirt) BlockCommit(ctx context.Context, name, disk string) error {
	return v.with(func(l *libvirt.Libvirt) error {
		d, err := lookup(l, name)
		if err != nil {
			return err
		}
		if err := l.DomainBlockCommit(d, disk, nil, nil, 0, libvirt.DomainBlockCommitActive|libvirt.DomainBlockCommitDelete); err != nil {
			return fmt.Errorf("blockcommit %s: %w", disk, err)
		}
		// Wait for the commit to reach the ready phase, then pivot.
		for {
			found, _, _, cur, end, err := l.DomainGetBlockJobInfo(d, disk, 0)
			if err != nil {
				return fmt.Errorf("blockcommit %s: %w", disk, err)
			}
			if found == 0 {
				return fmt.Errorf("blockcommit %s: job disappeared before pivot", disk)
			}
			if end > 0 && cur == end {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
		}
		if err := l.DomainBlockJobAbort(d, disk, libvirt.DomainBlockJobAbortPivot); err != nil {
			return fmt.Errorf("pivot %s: %w", disk, err)
		}
		for i := 0; i < 150; i++ {
			found, _, _, _, _, err := l.DomainGetBlockJobInfo(d, disk, 0)
			if err != nil || found == 0 {
				return err
			}
			time.Sleep(200 * time.Millisecond)
		}
		return fmt.Errorf("pivot %s: job did not finish", disk)
	})
}

func (v *Libvirt) Define(ctx context.Context, domainXML string) error {
	return v.with(func(l *libvirt.Libvirt) error {
		_, err := l.DomainDefineXML(domainXML)
		return err
	})
}

func lookup(l *libvirt.Libvirt, name string) (libvirt.Domain, error) {
	d, err := l.DomainLookupByName(name)
	if err != nil {
		var le libvirt.Error
		if errors.As(err, &le) && le.Code == uint32(libvirt.ErrNoDomain) {
			return d, ErrNoDomain
		}
		return d, err
	}
	return d, nil
}

func describe(l *libvirt.Libvirt, d libvirt.Domain) (Domain, error) {
	st, _, err := l.DomainGetState(d, 0)
	if err != nil {
		return Domain{}, err
	}
	x, err := l.DomainGetXMLDesc(d, 0)
	if err != nil {
		return Domain{}, err
	}
	disks, err := ParseDisks(x)
	if err != nil {
		return Domain{}, err
	}
	state := StateOther
	switch libvirt.DomainState(st) {
	case libvirt.DomainRunning:
		state = StateRunning
	case libvirt.DomainPaused:
		state = StatePaused
	case libvirt.DomainShutoff:
		state = StateShutoff
	}
	u := d.UUID
	return Domain{
		Name:  d.Name,
		UUID:  fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16]),
		State: state,
		Disks: disks,
	}, nil
}
