package virt

import (
	"encoding/xml"
	"strings"
	"testing"
)

const domXML = `<domain type='kvm'>
  <name>ha</name>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='/var/lib/libvirt/images/ha.qcow2'/>
      <backingStore/>
      <target dev='vda' bus='virtio'/>
    </disk>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='/var/lib/libvirt/images/data.qcow2'/>
      <backingStore type='file'><format type='qcow2'/><source file='/var/lib/libvirt/images/base.qcow2'/></backingStore>
      <target dev='vdb' bus='virtio'/>
    </disk>
    <disk type='file' device='cdrom'>
      <driver name='qemu' type='raw'/>
      <source file='/iso/installer.iso'/>
      <target dev='sda' bus='sata'/>
      <readonly/>
    </disk>
    <disk type='block' device='disk'>
      <source dev='/dev/vg0/lv'/>
      <target dev='vdc' bus='virtio'/>
    </disk>
  </devices>
</domain>`

func TestParseDisks(t *testing.T) {
	disks, err := ParseDisks(domXML)
	if err != nil || len(disks) != 4 {
		t.Fatalf("%+v %v", disks, err)
	}
	vda, vdb, sda, vdc := disks[0], disks[1], disks[2], disks[3]
	if vda.Target != "vda" || vda.Source != "/var/lib/libvirt/images/ha.qcow2" || vda.Format != "qcow2" || vda.Backing || !vda.Backupable() {
		t.Errorf("vda: %+v", vda)
	}
	if !vdb.Backing {
		t.Errorf("vdb backing chain not detected: %+v", vdb)
	}
	if sda.Backupable() || !sda.ReadOnly {
		t.Errorf("cdrom counted as backupable: %+v", sda)
	}
	if vdc.Backupable() || vdc.Source != "/dev/vg0/lv" {
		t.Errorf("block disk: %+v", vdc)
	}
}

func TestSnapshotXML(t *testing.T) {
	disks, _ := ParseDisks(domXML)
	x := SnapshotXML(`vb-1 & "x"`, disks, map[string]string{"vda": "/var/lib/libvirt/images/ha.qcow2.vb-1"})
	var parsed struct {
		Name  string `xml:"name"`
		Disks []struct {
			Name     string `xml:"name,attr"`
			Snapshot string `xml:"snapshot,attr"`
			Source   struct {
				File string `xml:"file,attr"`
			} `xml:"source"`
		} `xml:"disks>disk"`
	}
	if err := xml.Unmarshal([]byte(x), &parsed); err != nil {
		t.Fatalf("invalid XML %s: %v", x, err)
	}
	if parsed.Name != `vb-1 & "x"` || len(parsed.Disks) != 4 {
		t.Fatalf("%+v", parsed)
	}
	if d := parsed.Disks[0]; d.Snapshot != "external" || !strings.HasSuffix(d.Source.File, ".vb-1") {
		t.Errorf("vda: %+v", d)
	}
	for _, d := range parsed.Disks[1:] {
		if d.Snapshot != "no" {
			t.Errorf("%s should not be snapshotted: %+v", d.Name, d)
		}
	}
}
