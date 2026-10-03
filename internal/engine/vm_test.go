package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"testing"

	"github.com/voidgrid/voidgrid-backup/internal/repocfg"
	"github.com/voidgrid/voidgrid-backup/internal/virt"
	"github.com/voidgrid/voidgrid-backup/internal/virt/virttest"
)

func newVMFixture(t *testing.T, state string) (*Engine, Repo, *virttest.Fake, string) {
	t.Helper()
	dir := t.TempDir()
	disk := filepath.Join(dir, "ha.qcow2")
	writeFile(t, disk, "QFI\xfb disk contents")
	writeFile(t, filepath.Join(dir, "installer.iso"), "iso")
	hv := &virttest.Fake{
		DomainXML: "<domain><name>ha</name></domain>",
		Dom: &virt.Domain{Name: "ha", UUID: "u-1", State: state, Disks: []virt.Disk{
			{Target: "vda", Device: "disk", Type: "file", Source: disk, Format: "qcow2"},
			{Target: "sda", Device: "cdrom", Type: "file", Source: filepath.Join(dir, "installer.iso"), Format: "raw", ReadOnly: true},
		}},
	}
	e := newEngine(t, "host-a")
	r := Repo{ID: "r", Password: "pw", Config: repocfg.Config{
		Kind: repocfg.KindFilesystem, Filesystem: &repocfg.Filesystem{Path: filepath.Join(t.TempDir(), "repo")}}}
	if _, _, err := e.Init(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	return e, r, hv, disk
}

func vmManifest(t *testing.T, e *Engine, r Repo, snapID string, dir string) VMManifest {
	t.Helper()
	if _, err := e.Restore(context.Background(), r, snapID, "", dir, false); err != nil {
		t.Fatal(err)
	}
	var m VMManifest
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(dir, vmManifestName))), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestVMBackupRunning(t *testing.T) {
	ctx := context.Background()
	e, r, hv, disk := newVMFixture(t, virt.StateRunning)
	res, err := e.BackupVM(ctx, r, hv, VMSpec{Name: "ha", Quiesce: true}, keepOne)
	if err != nil || res.Err != nil || len(res.Warnings) != 0 || res.Path != "/vms/ha" {
		t.Fatalf("%+v %v", res, err)
	}
	if strings.Join(hv.Calls, ",") != "snapshot-quiesce,commit vda" {
		t.Fatalf("calls: %v", hv.Calls)
	}
	if hv.OnOverlay {
		t.Fatal("disk left on an overlay")
	}
	for _, p := range hv.Overlays {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("overlay %s left behind", p)
		}
	}
	out := filepath.Join(t.TempDir(), "out")
	m := vmManifest(t, e, r, res.SnapshotID, out)
	if m.Mode != "quiesced" || m.Disks["vda"] != disk || len(m.Disks) != 1 {
		t.Fatalf("manifest (the cdrom must be skipped): %+v", m)
	}
	if got := readFile(t, filepath.Join(out, "disks", "vda.qcow2")); got != "QFI\xfb disk contents" {
		t.Fatalf("disk copy: %q", got)
	}
	if got := readFile(t, filepath.Join(out, "domain.xml")); !strings.Contains(got, "<name>ha</name>") {
		t.Fatalf("domain.xml: %q", got)
	}
}

func TestVMBackupNoGuestAgent(t *testing.T) {
	ctx := context.Background()
	e, r, hv, _ := newVMFixture(t, virt.StateRunning)
	hv.QuiesceErr = errors.New("QEMU guest agent is not connected")
	res, err := e.BackupVM(ctx, r, hv, VMSpec{Name: "ha", Quiesce: true}, keepOne)
	if err != nil || res.Err != nil || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "crash-consistent") {
		t.Fatalf("%+v %v", res, err)
	}
	if strings.Join(hv.Calls, ",") != "snapshot-quiesce,snapshot,commit vda" {
		t.Fatalf("calls: %v", hv.Calls)
	}
	if m := vmManifest(t, e, r, res.SnapshotID, filepath.Join(t.TempDir(), "o")); m.Mode != "crash-consistent" {
		t.Fatalf("mode %q", m.Mode)
	}
}

func TestVMBackupCommitFailureKeepsSnapshot(t *testing.T) {
	ctx := context.Background()
	e, r, hv, _ := newVMFixture(t, virt.StateRunning)
	hv.CommitErr = errors.New("block job failed")
	res, err := e.BackupVM(ctx, r, hv, VMSpec{Name: "ha"}, keepOne)
	if err != nil || res.Err != nil || res.SnapshotID == "" {
		t.Fatalf("snapshot should still be saved: %+v %v", res, err)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "ACTION NEEDED") || !strings.Contains(res.Warnings[0], "virsh blockcommit ha vda") {
		t.Fatalf("warnings: %q", res.Warnings)
	}
}

func TestVMBackupShutoffAndRestore(t *testing.T) {
	ctx := context.Background()
	e, r, hv, disk := newVMFixture(t, virt.StateShutoff)
	res, err := e.BackupVM(ctx, r, hv, VMSpec{Name: "ha", Quiesce: true}, keepOne)
	if err != nil || res.Err != nil || len(hv.Calls) != 0 {
		t.Fatalf("offline backup must not snapshot: %+v %v calls=%v", res, err, hv.Calls)
	}
	if _, err := e.BackupVM(ctx, r, hv, VMSpec{Name: "ha", Disks: []string{"sda"}}, keepOne); err != nil {
		t.Fatal(err)
	} // only the cdrom selected: nothing to back up, reported as a failed path
	if res2, _ := e.BackupVM(ctx, r, hv, VMSpec{Name: "ha", Disks: []string{"sda"}}, keepOne); res2.Err == nil {
		t.Fatal("backup with no backupable disks succeeded")
	}

	// In place while running is refused.
	hv.Dom.State = virt.StateRunning
	if _, err := e.RestoreVM(ctx, r, hv, res.SnapshotID, "", false); err == nil || !strings.Contains(err.Error(), "shut down") {
		t.Fatalf("restore over a running VM: %v", err)
	}
	hv.Dom.State = virt.StateShutoff
	writeFile(t, disk, "corrupted")
	if _, err := e.RestoreVM(ctx, r, hv, res.SnapshotID, "", false); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, disk); got != "QFI\xfb disk contents" {
		t.Fatalf("disk after in-place restore: %q", got)
	}

	// Domain gone: disks restored and the domain re-defined.
	hv.Dom = nil
	os.Remove(disk)
	if _, err := e.RestoreVM(ctx, r, hv, res.SnapshotID, "", true); err != nil {
		t.Fatal(err)
	}
	if hv.Defined != "<domain><name>ha</name></domain>" || readFile(t, disk) != "QFI\xfb disk contents" {
		t.Fatalf("defined=%q", hv.Defined)
	}

	// Into a directory.
	dir := filepath.Join(t.TempDir(), "vm")
	if _, err := e.RestoreVM(ctx, r, hv, res.SnapshotID, dir, false); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dir, "disks", "vda.qcow2")); got != "QFI\xfb disk contents" {
		t.Fatalf("directory restore: %q", got)
	}
}
