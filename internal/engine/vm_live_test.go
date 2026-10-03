//go:build libvirtlive

package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitalocean/go-libvirt"
	"github.com/digitalocean/go-libvirt/socket/dialers"

	"github.com/voidgrid/voidgrid-backup/internal/repocfg"
	"github.com/voidgrid/voidgrid-backup/internal/virt"
)

// TestLiveVMBackup runs a throwaway transient domain on the per-user
// qemu:///session libvirt (never the system daemon) and backs it up while
// running. scripts/libvirt-live-test.sh prepares the disk and socket.
func TestLiveVMBackup(t *testing.T) {
	ctx := context.Background()
	sock, disk, name := os.Getenv("VB_LIBVIRT_SOCK"), os.Getenv("VB_VM_DISK"), os.Getenv("VB_VM_NAME")
	if sock == "" || disk == "" || name == "" {
		t.Skip("VB_LIBVIRT_SOCK, VB_VM_DISK, VB_VM_NAME not set")
	}
	const uri = "qemu:///session"

	l := libvirt.NewWithDialer(dialers.NewLocal(dialers.WithSocket(sock)))
	if err := l.ConnectToURI(uri); err != nil {
		t.Fatal(err)
	}
	defer l.Disconnect()
	dom, err := l.DomainCreateXML(`<domain type='kvm'><name>`+name+`</name>
		<memory unit='MiB'>64</memory><vcpu>1</vcpu>
		<os><type arch='x86_64'>hvm</type></os>
		<devices>
		  <disk type='file' device='disk'><driver name='qemu' type='qcow2'/><source file='`+disk+`'/><target dev='vda' bus='virtio'/></disk>
		</devices></domain>`, 0)
	if err != nil {
		t.Fatalf("start throwaway domain: %v", err)
	}
	defer l.DomainDestroy(dom)

	hv := &virt.Libvirt{Socket: sock, URI: uri}
	got, err := hv.Domain(ctx, name)
	if err != nil || got.State != virt.StateRunning || len(got.Disks) != 1 || got.Disks[0].Source != disk {
		t.Fatalf("domain: %+v %v", got, err)
	}

	e := newEngine(t, "live")
	r := Repo{ID: "live", Password: "pw", Config: repocfg.Config{
		Kind: repocfg.KindFilesystem, Filesystem: &repocfg.Filesystem{Path: filepath.Join(t.TempDir(), "repo")}}}
	if _, _, err := e.Init(ctx, r); err != nil {
		t.Fatal(err)
	}
	// Quiesce is requested but there is no guest agent: must fall back.
	res, err := e.BackupVM(ctx, r, hv, VMSpec{Name: name, Quiesce: true}, keepOne)
	if err != nil || res.Err != nil {
		t.Fatalf("backup: %+v %v", res, err)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "crash-consistent") {
		t.Fatalf("warnings: %q", res.Warnings)
	}

	// The domain is back on its original disk and no overlay is left.
	after, err := hv.Domain(ctx, name)
	if err != nil || after.State != virt.StateRunning || after.Disks[0].Source != disk || after.Disks[0].Backing {
		t.Fatalf("after backup: %+v %v", after, err)
	}
	if leftovers, _ := filepath.Glob(disk + ".vb-*"); len(leftovers) != 0 {
		t.Fatalf("overlay files left behind: %v", leftovers)
	}

	out := filepath.Join(t.TempDir(), "out")
	if _, err := e.RestoreVM(ctx, r, hv, res.SnapshotID, out, false); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(out, "disks", "vda.qcow2"))
	if err != nil || len(b) < 4 || string(b[:4]) != "QFI\xfb" {
		t.Fatalf("restored disk is not a qcow2 image (%d bytes): %v", len(b), err)
	}
	x, _ := os.ReadFile(filepath.Join(out, "domain.xml"))
	if !strings.Contains(string(x), "<name>"+name+"</name>") {
		t.Fatalf("domain.xml: %s", x)
	}

	// A second backup right after works too (no stale snapshot state).
	time.Sleep(time.Second)
	if res2, err := e.BackupVM(ctx, r, hv, VMSpec{Name: name}, keepOne); err != nil || res2.Err != nil {
		t.Fatalf("second backup: %+v %v", res2, err)
	}
}
