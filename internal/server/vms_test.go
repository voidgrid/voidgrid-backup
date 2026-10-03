package server

import (
	"context"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
	"github.com/voidgrid/voidgrid-backup/internal/repocfg"
	"github.com/voidgrid/voidgrid-backup/internal/virt"
	"github.com/voidgrid/voidgrid-backup/internal/virt/virttest"
)

func TestVMJobEndToEnd(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	disk := filepath.Join(dir, "ha.qcow2")
	os.WriteFile(disk, []byte("QFI\xfb original"), 0o644)
	hv := &virttest.Fake{
		DomainXML: "<domain><name>ha</name></domain>",
		Dom: &virt.Domain{Name: "ha", UUID: "u", State: virt.StateRunning, Disks: []virt.Disk{
			{Target: "vda", Device: "disk", Type: "file", Source: disk, Format: "qcow2"},
		}},
	}

	a, addr := startAgent(t, t.TempDir())
	a.SetHypervisor(hv)
	c := newController(t)
	ag, err := enrollTestAgent(t, c, a, "host", addr)
	if err != nil {
		t.Fatal(err)
	}
	repo, _, err := c.AddRepository(ctx, "local", repocfg.Config{Kind: repocfg.KindFilesystem,
		Filesystem: &repocfg.Filesystem{Path: filepath.Join(t.TempDir(), "repo")}}, "pw", ag.ID)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	client := authedClient(t, c, srv)

	get(t, client, srv.URL+"/agents/"+ag.ID+"/vms", "Create VM job")
	post(t, client, srv.URL+"/jobs/vm", url.Values{
		"agent": {ag.ID}, "vm": {"ha"}, "name": {"vm-ha"}, "repository": {repo.ID}, "disk": {"vda"}, "quiesce": {"on"},
		"schedule": {""}, "keep_latest": {"3"}, "keep_hourly": {"0"}, "keep_daily": {"7"},
		"keep_weekly": {"0"}, "keep_monthly": {"0"}, "keep_annual": {"0"},
	})
	job, err := c.Catalog.JobByName(ctx, "vm-ha")
	if err != nil || job.Kind != catalog.JobVM || job.VM.Name != "ha" || !job.VM.Quiesce {
		t.Fatalf("vm job: %+v %v", job, err)
	}

	run, err := c.RunJob(ctx, job.ID, "manual")
	if err != nil || run.Status != catalog.RunSuccess {
		t.Fatalf("vm backup: %+v %v", run, err)
	}
	if strings.Join(hv.Calls, ",") != "snapshot-quiesce,commit vda" || hv.OnOverlay {
		t.Fatalf("hypervisor calls: %v overlay=%v", hv.Calls, hv.OnOverlay)
	}
	snaps, err := c.Snapshots(ctx, job.ID)
	if err != nil || len(snaps) != 1 || snaps[0].Path != "/vms/ha" {
		t.Fatalf("snapshots: %+v %v", snaps, err)
	}
	get(t, client, srv.URL+"/jobs/"+job.ID+"/snapshots/"+snaps[0].ID, "Restore the VM")

	// Running VM: in-place restore fails and is recorded as a failed run.
	os.WriteFile(disk, []byte("damaged"), 0o644)
	if rr, _ := c.RestoreVM(ctx, job.ID, snaps[0].ID, "", false); rr.Status != catalog.RunFailed || !strings.Contains(rr.Error, "shut down") {
		t.Fatalf("restore over a running VM: %+v", rr)
	}
	hv.Dom.State = virt.StateShutoff
	rr, err := c.RestoreVM(ctx, job.ID, snaps[0].ID, "", false)
	if err != nil || !restoreOK(rr) {
		t.Fatalf("in-place VM restore: %+v %v", rr, err)
	}
	if b, _ := os.ReadFile(disk); string(b) != "QFI\xfb original" {
		t.Fatalf("disk after restore: %q", b)
	}

	// A VM job with a stack at the same time is refused.
	if _, err := c.AddJob(ctx, JobInput{Name: "x", AgentID: ag.ID, RepositoryID: repo.ID, Keep: catalog.DefaultRetention,
		VM: &catalog.VMConfig{Name: "ha"}, Stack: &catalog.StackConfig{Project: "p", WorkingDir: "/srv/p"}}); err == nil {
		t.Fatal("job with both a VM and a stack accepted")
	}
	if _, err := c.AddJob(ctx, JobInput{Name: "y", AgentID: ag.ID, RepositoryID: repo.ID, Keep: catalog.DefaultRetention,
		VM: &catalog.VMConfig{Name: "bad name; rm"}}); err == nil {
		t.Fatal("invalid VM name accepted")
	}
}
