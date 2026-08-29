package ova

import (
	"archive/tar"
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// minimalVmdk builds a single-grain uncompressed monolithicSparse VMDK holding
// guest (len(guest) <= 512).
func minimalVmdk(guest []byte) []byte {
	const (
		gdSector   = 1
		gtSector   = 2
		dataSector = 3
	)
	// Two sectors of virtual capacity (the second left as a zero grain).
	buf := make([]byte, (dataSector+2)*512)
	le := binary.LittleEndian
	le.PutUint32(buf[0:4], 0x564d444b)
	le.PutUint32(buf[4:8], 1)   // version
	le.PutUint32(buf[8:12], 3)  // flags
	le.PutUint64(buf[12:20], 2) // capacity: 2 sectors
	le.PutUint64(buf[20:28], 1) // grainSize: 1 sector
	le.PutUint32(buf[44:48], 128)
	le.PutUint64(buf[56:64], gdSector)
	le.PutUint64(buf[64:72], dataSector)
	le.PutUint32(buf[gdSector*512:], gtSector)
	le.PutUint32(buf[gtSector*512:], dataSector)
	le.PutUint32(buf[gtSector*512+4:], 1) // zero grain
	copy(buf[dataSector*512:], guest)
	return buf
}

func testOVA(t *testing.T, entries map[string][]byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.ova")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create ova: %v", err)
	}
	tw := tar.NewWriter(f)
	for name, data := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data))}); err != nil {
			t.Fatalf("tar header: %v", err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatalf("tar write: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close ova: %v", err)
	}
	return path
}

func TestOpenFileReadsDisk(t *testing.T) {
	guest := []byte("OVA disk contents")
	path := testOVA(t, map[string][]byte{
		"appliance.ovf": []byte(`<Envelope/>`),
		"disk1.vmdk":    minimalVmdk(guest),
	})

	img, err := OpenFile(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer img.Close()
	if len(img.Disks) != 1 {
		t.Fatalf("expected 1 disk, got %d", len(img.Disks))
	}
	d := img.Disks[0]
	if d.Size() != 1024 {
		t.Fatalf("unexpected size %d", d.Size())
	}
	got := make([]byte, len(guest))
	if _, err := d.ReadAt(got, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(got, guest) {
		t.Fatalf("got %q, want %q", got, guest)
	}
}

func TestOpenFileIgnoresNonVmdk(t *testing.T) {
	// Only .vmdk entries become disks; the .ovf and README are ignored.
	path := testOVA(t, map[string][]byte{
		"appliance.ovf": []byte(`<Envelope/>`),
		"README.txt":    []byte("hi"),
		"disk1.vmdk":    minimalVmdk([]byte("data")),
	})
	img, err := OpenFile(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer img.Close()
	if len(img.Disks) != 1 {
		t.Fatalf("expected 1 disk, got %d", len(img.Disks))
	}
}
