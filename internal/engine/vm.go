package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"time"

	kfs "github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/fs/localfs"
	"github.com/kopia/kopia/fs/virtualfs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot"

	"github.com/voidgrid/voidgrid-backup/internal/guard"
	"github.com/voidgrid/voidgrid-backup/internal/virt"
)

// Names inside a VM snapshot.
const (
	vmDisksDir     = "disks"
	vmXMLName      = "domain.xml"
	vmManifestName = "voidgrid-backup-vm.json"
)

var domainName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.+-]{0,127}$`)

// VMSpec says what to back up for one libvirt domain.
type VMSpec struct {
	Name    string
	Disks   []string // disk targets (vda, ...); empty means every backupable disk
	Quiesce bool     // freeze guest filesystems via the QEMU guest agent if possible
}

// VMManifest is stored in every VM snapshot.
type VMManifest struct {
	Version int               `json:"version"`
	Name    string            `json:"name"`
	UUID    string            `json:"uuid"`
	Disks   map[string]string `json:"disks"`   // target -> host path
	Formats map[string]string `json:"formats"` // target -> qcow2/raw
	Files   map[string]string `json:"files"`   // target -> file name under disks/
	Mode    string            `json:"mode"`    // "offline", "quiesced", "crash-consistent"
}

// VMSourcePath is the Kopia source path for a domain's snapshots.
func VMSourcePath(name string) string { return "/vms/" + name }

// BackupVM snapshots a domain's disks and definition. A running domain is
// switched onto temporary overlays for the duration of the read, so its
// base images are stable, and always merged back afterwards.
func (e *Engine) BackupVM(ctx context.Context, r Repo, hv virt.Hypervisor, spec VMSpec, keep Retention) (PathResult, error) {
	rep, err := e.open(ctx, r)
	if err != nil {
		return PathResult{}, err
	}
	defer rep.Close(ctx)
	res := e.backupVM(ctx, rep, hv, spec, keep)
	if err := e.maintain(ctx, rep); err != nil {
		res.Warnings = append(res.Warnings, "repository maintenance: "+err.Error())
	}
	return res, nil
}

func (e *Engine) backupVM(ctx context.Context, rep repo.Repository, hv virt.Hypervisor, spec VMSpec, keep Retention) PathResult {
	res := PathResult{Path: VMSourcePath(spec.Name)}
	fail := func(err error) PathResult { res.Err = err; return res }
	if !domainName.MatchString(spec.Name) {
		return fail(fmt.Errorf("invalid domain name %q", spec.Name))
	}
	dom, err := hv.Domain(ctx, spec.Name)
	if err != nil {
		return fail(err)
	}
	inactiveXML, err := hv.XML(ctx, spec.Name, true)
	if err != nil {
		return fail(err)
	}

	want := map[string]bool{}
	for _, t := range spec.Disks {
		want[t] = true
	}
	man := VMManifest{Version: 1, Name: dom.Name, UUID: dom.UUID,
		Disks: map[string]string{}, Formats: map[string]string{}, Files: map[string]string{}, Mode: "offline"}
	var chosen []virt.Disk
	for _, d := range dom.Disks {
		if len(want) > 0 && !want[d.Target] {
			continue
		}
		if !d.Backupable() {
			if want[d.Target] {
				res.Warnings = append(res.Warnings, fmt.Sprintf("disk %s skipped: only writable file-backed disks can be backed up", d.Target))
			}
			continue
		}
		if _, err := guard.SourcePath(d.Source); err != nil {
			return fail(fmt.Errorf("disk %s: %w", d.Target, err))
		}
		if d.Backing {
			res.Warnings = append(res.Warnings, fmt.Sprintf("disk %s has a backing image; only its top layer %s is backed up", d.Target, d.Source))
		}
		chosen = append(chosen, d)
	}
	if len(chosen) == 0 {
		return fail(errors.New("no backupable disks selected"))
	}

	var diskEntries []kfs.Entry
	for _, d := range chosen {
		ent, err := localfs.NewEntry(d.Source)
		if err != nil {
			return fail(fmt.Errorf("disk %s: %w", d.Target, err))
		}
		f, ok := ent.(kfs.File)
		if !ok {
			return fail(fmt.Errorf("disk %s: %s is not a regular file", d.Target, d.Source))
		}
		name := d.Target + "." + diskExt(d.Format)
		diskEntries = append(diskEntries, renamedFile{f, name})
		man.Disks[d.Target], man.Formats[d.Target], man.Files[d.Target] = d.Source, d.Format, name
	}

	around := func(upload func() error) error { return upload() }
	var overlayWarnings []string
	if dom.Active() {
		around = func(upload func() error) error {
			warns, err := withOverlays(ctx, hv, dom, chosen, spec.Quiesce, func(mode string) error {
				man.Mode = mode // known before the upload reads the manifest
				return upload()
			})
			overlayWarnings = warns
			return err
		}
	}

	// The manifest is read after the upload decided the mode, so it is lazy.
	manEntry := lazyStream{
		StreamingFile: virtualfs.StreamingFileFromReader(vmManifestName, io.NopCloser(bytes.NewReader(nil))),
		open: func(context.Context) (io.ReadCloser, error) {
			b, err := json.MarshalIndent(man, "", "  ")
			return io.NopCloser(bytes.NewReader(b)), err
		},
	}
	root := virtualfs.NewStaticDirectory(spec.Name, []kfs.Entry{
		virtualfs.NewStaticDirectory(vmDisksDir, diskEntries),
		virtualfs.StreamingFileFromReader(vmXMLName, io.NopCloser(bytes.NewReader([]byte(inactiveXML)))),
		manEntry,
	})
	si := snapshot.SourceInfo{Host: e.hostname, UserName: Username, Path: res.Path}
	res = e.takeSnapshot(ctx, rep, si, root, nil, keep, around, res)
	res.Warnings = append(res.Warnings, overlayWarnings...)
	return res
}

// withOverlays puts every chosen disk of a running domain onto a temporary
// overlay, runs fn while the base images are stable, then merges each
// overlay back and pivots. The merge always runs, and a failed merge is a
// warning (the snapshot itself is good) rather than a failed backup. fn
// is told how consistent the copy is ("quiesced" or "crash-consistent").
func withOverlays(ctx context.Context, hv virt.Hypervisor, dom virt.Domain, disks []virt.Disk, quiesce bool, fn func(mode string) error) (warnings []string, err error) {
	tag := "vb-" + strconv.FormatInt(time.Now().Unix(), 10)
	overlays := map[string]string{}
	for _, d := range disks {
		overlays[d.Target] = d.Source + "." + tag
	}
	xml := virt.SnapshotXML(tag, dom.Disks, overlays)

	mode := "crash-consistent"
	if quiesce {
		if qerr := hv.SnapshotDisks(ctx, dom.Name, xml, true); qerr == nil {
			mode = "quiesced"
		} else {
			warnings = append(warnings, "guest agent could not freeze the filesystems ("+qerr.Error()+"); the copy is crash-consistent")
			quiesce = false
		}
	}
	if !quiesce {
		if err := hv.SnapshotDisks(ctx, dom.Name, xml, false); err != nil {
			return warnings, fmt.Errorf("create overlay snapshot: %w", err)
		}
	}

	err = fn(mode)

	for _, d := range disks {
		if cerr := hv.BlockCommit(context.WithoutCancel(ctx), dom.Name, d.Target); cerr != nil {
			warnings = append(warnings, fmt.Sprintf(
				"ACTION NEEDED: disk %s is still running on overlay %s and needs a manual merge (virsh blockcommit %s %s --active --pivot): %v",
				d.Target, overlays[d.Target], dom.Name, d.Target, cerr))
			continue
		}
		// libvirt deletes the overlay on pivot; remove a leftover if it didn't.
		if rmErr := os.Remove(overlays[d.Target]); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			warnings = append(warnings, fmt.Sprintf("remove leftover overlay %s: %v", overlays[d.Target], rmErr))
		}
	}
	return warnings, err
}

// RestoreVM restores a VM snapshot. With targetDir set, the snapshot tree
// (disks, domain.xml) is written there. Otherwise disks go back to their
// original paths, which requires the domain to be shut off or gone; a
// missing domain is re-defined from the saved XML when define is set.
func (e *Engine) RestoreVM(ctx context.Context, r Repo, hv virt.Hypervisor, snapshotID, targetDir string, define bool) (RestoreStats, error) {
	rep, err := e.open(ctx, r)
	if err != nil {
		return RestoreStats{}, err
	}
	defer rep.Close(ctx)
	root, err := e.snapshotEntry(ctx, rep, snapshotID, "")
	if err != nil {
		return RestoreStats{}, err
	}
	var man VMManifest
	if err := readJSONEntry(ctx, root, vmManifestName, &man); err != nil {
		return RestoreStats{}, fmt.Errorf("not a VM snapshot: %w", err)
	}
	if !domainName.MatchString(man.Name) {
		return RestoreStats{}, fmt.Errorf("VM manifest has invalid name %q", man.Name)
	}

	if targetDir != "" {
		target, err := guard.RestoreTarget(targetDir)
		if err != nil {
			return RestoreStats{}, err
		}
		if empty, err := emptyOrMissing(target); err != nil {
			return RestoreStats{}, err
		} else if !empty {
			return RestoreStats{}, fmt.Errorf("restore target %s is not empty", target)
		}
		return e.restoreTo(ctx, rep, root, target, false)
	}

	dom, err := hv.Domain(ctx, man.Name)
	missing := errors.Is(err, virt.ErrNoDomain)
	if err != nil && !missing {
		return RestoreStats{}, err
	}
	if !missing && dom.State != virt.StateShutoff {
		return RestoreStats{}, fmt.Errorf("shut down %s before restoring its disks in place (it is %s)", man.Name, dom.State)
	}
	var total RestoreStats
	for _, target := range sortedKeys(man.Disks) {
		dest, err := guard.RestoreTarget(man.Disks[target])
		if err != nil {
			return total, err
		}
		ent, err := childEntry(ctx, root, vmDisksDir+"/"+man.Files[target])
		if err != nil {
			return total, err
		}
		st, err := e.restoreTo(ctx, rep, ent, dest, true)
		total.add(st)
		if err != nil {
			return total, fmt.Errorf("restore disk %s to %s: %w", target, dest, err)
		}
	}
	if missing && define {
		ent, err := childEntry(ctx, root, vmXMLName)
		if err != nil {
			return total, err
		}
		x, err := readEntry(ctx, ent, 4<<20)
		if err != nil {
			return total, err
		}
		if err := hv.Define(ctx, string(x)); err != nil {
			return total, fmt.Errorf("disks restored, but defining %s failed: %w", man.Name, err)
		}
	}
	return total, nil
}

func readJSONEntry(ctx context.Context, root kfs.Entry, name string, v any) error {
	ent, err := childEntry(ctx, root, name)
	if err != nil {
		return err
	}
	b, err := readEntry(ctx, ent, 1<<20)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func readEntry(ctx context.Context, ent kfs.Entry, limit int64) ([]byte, error) {
	f, ok := ent.(kfs.File)
	if !ok {
		return nil, fmt.Errorf("%s is not a file", ent.Name())
	}
	rd, err := f.Open(ctx)
	if err != nil {
		return nil, err
	}
	defer rd.Close()
	return io.ReadAll(io.LimitReader(rd, limit))
}

func diskExt(format string) string {
	if format == "" {
		return "img"
	}
	return format
}
