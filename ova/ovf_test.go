package ova

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func testOVF(t *testing.T, ovfXML string, vmdkData []byte) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "appliance.ovf"), []byte(ovfXML), 0o644); err != nil {
		t.Fatal(err)
	}
	if vmdkData != nil {
		if err := os.WriteFile(filepath.Join(dir, "disk1.vmdk"), vmdkData, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "appliance.ovf")
}

func TestOpenOVFReadsReferencedDisk(t *testing.T) {
	guest := []byte("standalone ovf disk")
	ovf := `<Envelope xmlns="http://schemas.dmtf.org/ovf/envelope/1" xmlns:ovf="http://schemas.dmtf.org/ovf/envelope/1">
  <References>
    <File ovf:id="f1" ovf:href="disk1.vmdk"/>
  </References>
</Envelope>`

	img, err := OpenOVF(testOVF(t, ovf, minimalVmdk(guest)))
	if err != nil {
		t.Fatalf("OpenOVF: %v", err)
	}
	defer img.Close()
	if len(img.Disks) != 1 {
		t.Fatalf("expected 1 disk, got %d", len(img.Disks))
	}
	got := make([]byte, len(guest))
	if _, err := img.Disks[0].ReadAt(got, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(got, guest) {
		t.Fatalf("got %q, want %q", got, guest)
	}
}

func TestOpenOVFIgnoresNonVmdkHrefs(t *testing.T) {
	// Only the .vmdk href becomes a disk; the manifest/XML hrefs are ignored.
	ovf := `<Envelope xmlns="http://schemas.dmtf.org/ovf/envelope/1" xmlns:ovf="http://schemas.dmtf.org/ovf/envelope/1">
  <References>
    <File ovf:id="m" ovf:href="appliance.mf"/>
    <File ovf:id="f1" ovf:href="disk1.vmdk"/>
  </References>
</Envelope>`

	img, err := OpenOVF(testOVF(t, ovf, minimalVmdk([]byte("data"))))
	if err != nil {
		t.Fatalf("OpenOVF: %v", err)
	}
	defer img.Close()
	if len(img.Disks) != 1 {
		t.Fatalf("expected 1 disk, got %d", len(img.Disks))
	}
}

func TestOpenOVFNoDisks(t *testing.T) {
	ovf := `<Envelope xmlns="http://schemas.dmtf.org/ovf/envelope/1"/>`
	if _, err := OpenOVF(testOVF(t, ovf, nil)); err == nil {
		t.Fatalf("expected error for a descriptor with no .vmdk reference")
	}
}
