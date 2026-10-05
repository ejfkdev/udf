package image

import (
	"bytes"
	"encoding/binary"
	"os"
	"strings"
	"testing"
)

// The Kylin/PlatOS appliance image is the sample that exposed the metadata_csum
// group descriptor checksum bug: seven partitions (a FAT16 ESP and six ext4
// filesystems, all with metadata_csum and 64-byte descriptors) whose
// filesystems were refused outright before the fix. Point UDF_XDR_IMAGE at the
// qcow2 to run this; it reads a few hundred kilobytes, not the whole 10 GB.
func TestXDRApplianceImage(t *testing.T) {
	path := os.Getenv("UDF_XDR_IMAGE")
	if path == "" {
		t.Skip("set UDF_XDR_IMAGE to the Kylin/PlatOS qcow2 to run this test")
	}
	if _, err := os.Stat(path); err != nil {
		t.Skipf("no image at %s", path)
	}

	meta, err := ScanDiskMetadata(path)
	if err != nil {
		t.Fatalf("ScanDiskMetadata: %v", err)
	}
	if len(meta.Disks) != 1 {
		t.Fatalf("got %d disks", len(meta.Disks))
	}
	want := map[string]string{
		"p1": "fat16",
		"p2": "ext4",
		"p3": "ext4",
		"p4": "ext4",
		"p5": "ext4",
		"p6": "ext4",
		"p7": "ext4",
	}
	got := map[string]string{}
	for _, v := range meta.Disks[0].Volumes {
		got[v.Name] = v.FSType
	}
	for name, fs := range want {
		if got[name] != fs {
			t.Fatalf("volume %s filesystem = %q, want %q (all: %v)", name, got[name], fs, meta.Disks[0].Volumes)
		}
	}

	// The ESP holds the EFI boot files.
	entries, err := ListDisk(path, "p1")
	if err != nil {
		t.Fatalf("ListDisk(p1): %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("the EFI partition lists nothing")
	}

	// /boot: a Kylin kernel, an initramfs and the grub directory.
	entries, err = ListDisk(path, "p2")
	if err != nil {
		t.Fatalf("ListDisk(p2): %v", err)
	}
	var kernel FileEntry
	for _, e := range entries {
		if strings.HasPrefix(e.Name, "vmlinuz-4.19.90") {
			kernel = e
		}
	}
	if kernel.Name == "" {
		t.Fatalf("no kernel in the boot partition: %v", entries)
	}
	// Reading the whole 10 MB kernel crosses most of its extents; its head is
	// a gzip stream and its tail carries the gzip trailer's ISIZE, so the read
	// is checked at both ends without trusting a decompressor.
	const kernelSize = 10394794
	if kernel.Size != kernelSize {
		t.Fatalf("kernel size %d, want %d", kernel.Size, kernelSize)
	}
	dest := t.TempDir() + "/vmlinuz"
	if _, err := ExtractDiskPath(path, "p2/"+kernel.Name, dest, 1<<20); err != nil {
		t.Fatalf("extract kernel: %v", err)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != kernelSize {
		t.Fatalf("kernel read %d bytes, want %d", len(data), kernelSize)
	}
	if !bytes.HasPrefix(data, []byte{0x1f, 0x8b, 0x08}) {
		t.Fatalf("kernel does not start with a gzip header: %x", data[:4])
	}
	const isize = 29383168 // the uncompressed size the gzip trailer records
	if got := binary.LittleEndian.Uint32(data[len(data)-4:]); got != isize {
		t.Fatalf("kernel gzip trailer ISIZE = %d, want %d", got, isize)
	}

	// The root filesystem's /etc/os-release identifies the appliance.
	osRelease := t.TempDir() + "/os-release"
	if _, err := ExtractDiskPath(path, "p3/etc/os-release", osRelease, 1<<20); err != nil {
		t.Fatalf("extract os-release: %v", err)
	}
	text, err := os.ReadFile(osRelease)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(text), "PlatOS") {
		t.Fatalf("os-release does not name PlatOS: %q", firstBytes(text, 120))
	}

	// A 40 GB filesystem lists too (large block numbers, group descriptors
	// beyond the first).
	entries, err = ListDisk(path, "p6")
	if err != nil {
		t.Fatalf("ListDisk(p6): %v", err)
	}
	if len(entries) < 3 {
		t.Fatalf("p6 lists %d entries", len(entries))
	}
}
