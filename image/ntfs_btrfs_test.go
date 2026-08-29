package image

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestDetectBtrfsNTFSBackend(t *testing.T) {
	ntfs := make([]byte, 4096)
	copy(ntfs[3:], "NTFS    ")
	btrfs := make([]byte, 0x10048)
	copy(btrfs[0x10040:], "_BHRfS_M")

	for _, tc := range []struct {
		name string
		data []byte
		want string
	}{
		{"ntfs", ntfs, "ntfs"},
		{"btrfs", btrfs, "btrfs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be := &diskBackend{ra: bytes.NewReader(tc.data), size: int64(len(tc.data)), name: "x"}
			if got := detectFilesystem(be, 0, int64(len(tc.data)), 512); got != tc.want {
				t.Fatalf("detectFilesystem = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDiscoverVolumesBareFilesystem(t *testing.T) {
	// A real NTFS boot sector ends with the 0x55AA signature, so an MBR parse
	// "succeeds" with zero valid partitions; discoverVolumes must fall back to a
	// whole-disk region instead of reporting no filesystem at all.
	data := make([]byte, 8192)
	copy(data[3:], "NTFS    ")
	data[510], data[511] = 0x55, 0xaa
	be := &diskBackend{ra: bytes.NewReader(data), size: int64(len(data)), name: "x"}

	vols, _, err := discoverVolumes(be)
	if err != nil {
		t.Fatal(err)
	}
	if len(vols) != 1 || vols[0].Name != "disk" || vols[0].FSType != "ntfs" {
		t.Fatalf("vols = %+v", vols)
	}
}

func TestRawFilesystemBtrfsNTFS(t *testing.T) {
	ntfs := make([]byte, 4096)
	copy(ntfs[3:], "NTFS    ")
	btrfs := make([]byte, 0x10048)
	copy(btrfs[0x10040:], "_BHRfS_M")

	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"ntfs", ntfs},
		{"btrfs", btrfs},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "img.raw")
			if err := os.WriteFile(p, tc.data, 0o644); err != nil {
				t.Fatal(err)
			}
			if !detectRawFilesystem(p) {
				t.Fatalf("detectRawFilesystem(%s) = false", tc.name)
			}
			if !IsDiskImage(p) {
				t.Fatalf("IsDiskImage(%s) = false", tc.name)
			}
			if got := DetectInput(p); got != "disk" {
				t.Fatalf("DetectInput(%s) = %q, want disk", tc.name, got)
			}
		})
	}
}
