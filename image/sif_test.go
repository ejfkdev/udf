package image

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	sif "github.com/sylabs/sif/v2/pkg/sif"
)

func TestSIFDetectAndList(t *testing.T) {
	// A bare ext4 filesystem image becomes the SIF "system" partition.
	rawPath := createExt4Image(t)
	data := mustReadFile(t, rawPath)

	sifPath := filepath.Join(t.TempDir(), "test.sif")
	fimg, err := sif.CreateContainerAtPath(sifPath, sif.OptCreateDeterministic())
	if err != nil {
		t.Fatalf("create sif container: %v", err)
	}
	di, err := sif.NewDescriptorInput(sif.DataPartition, bytes.NewReader(data),
		sif.OptPartitionMetadata(sif.FsExt3, sif.PartSystem, "amd64"),
		sif.OptObjectName("system"),
	)
	if err != nil {
		t.Fatalf("new partition descriptor: %v", err)
	}
	if err := fimg.AddObject(di); err != nil {
		t.Fatalf("add partition: %v", err)
	}
	if err := fimg.UnloadContainer(); err != nil {
		t.Fatalf("unload container: %v", err)
	}

	meta, err := ScanDiskMetadata(sifPath)
	if err != nil {
		t.Fatalf("scan sif metadata: %v", err)
	}
	if len(meta.Disks) != 1 || meta.Disks[0].Format != "sif" {
		t.Fatalf("unexpected sif metadata: %+v", meta)
	}
	if len(meta.Disks[0].Volumes) != 1 || meta.Disks[0].Volumes[0].FSType != "ext4" {
		t.Fatalf("expected single ext4 volume, got %+v", meta.Disks[0].Volumes)
	}

	entries, err := ListDisk(sifPath, "/disk")
	if err != nil {
		t.Fatalf("list /disk: %v", err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name] = true
	}
	if !names["etc"] || !names["usr"] {
		t.Fatalf("expected etc and usr in listing, got %v", names)
	}

	dest := filepath.Join(t.TempDir(), "passwd.out")
	if _, err := ExtractDiskPath(sifPath, "/disk/etc/passwd", dest, 1<<16); err != nil {
		t.Fatalf("extract passwd: %v", err)
	}
	if data, err := os.ReadFile(dest); err != nil || string(data) != "root:x:0:0\n" {
		t.Fatalf("unexpected passwd %q err=%v", data, err)
	}
}
