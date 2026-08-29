package image

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ejfkdev/udf/diskkit"
	"github.com/ejfkdev/udf/diskkit/filesystem/iso9660"
)

func TestISO9660DetectAndList(t *testing.T) {
	path := createISOImage(t)

	meta, err := ScanDiskMetadata(path)
	if err != nil {
		t.Fatalf("scan metadata: %v", err)
	}
	if len(meta.Disks) != 1 || len(meta.Disks[0].Volumes) != 1 {
		t.Fatalf("unexpected metadata: %+v", meta)
	}
	if got := meta.Disks[0].Volumes[0].FSType; got != "iso9660" {
		t.Fatalf("expected iso9660, got %q", got)
	}

	// A bare filesystem image has a single "disk" volume; list its root.
	entries, err := ListDisk(path, "/")
	if err != nil {
		t.Fatalf("list root: %v", err)
	}
	if len(entries) != 1 || entries[0].FSType != "iso9660" {
		t.Fatalf("unexpected root listing: %+v", entries)
	}
	entries, err = ListDisk(path, "/disk")
	if err != nil {
		t.Fatalf("list /disk: %v", err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name] = true
	}
	if !names["hello.txt"] || !names["sub"] {
		t.Fatalf("expected hello.txt and sub in listing, got %v", names)
	}

	dest := filepath.Join(t.TempDir(), "hello.out")
	if _, err := ExtractDiskPath(path, "/disk/hello.txt", dest, 1<<16); err != nil {
		t.Fatalf("extract hello.txt: %v", err)
	}
	if data, err := os.ReadFile(dest); err != nil || string(data) != "hello iso\n" {
		t.Fatalf("unexpected extracted content %q err=%v", data, err)
	}
}

// createISOImage builds a small ISO9660 image holding a couple of files using
// go-diskfs, returning its path.
func createISOImage(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.iso")
	const size = 6 << 20
	d, err := diskfs.Create(path, size, diskfs.SectorSizeDefault)
	if err != nil {
		t.Fatalf("create raw: %v", err)
	}

	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "hello.txt"), []byte("hello iso\n"), 0o644); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "sub", "data.bin"), []byte("data"), 0o644); err != nil {
		t.Fatalf("write data: %v", err)
	}

	ifs, err := iso9660.Create(d.Backend, size, 0, 2048, workspace)
	if err != nil {
		t.Fatalf("create iso9660: %v", err)
	}
	if err := ifs.Finalize(iso9660.FinalizeOptions{RockRidge: true}); err != nil {
		t.Fatalf("finalize iso9660: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}
	return path
}
