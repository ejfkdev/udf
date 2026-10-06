package image

import (
	"os"
	"strings"
	"testing"
)

// TestQCow2ZstdCompressedClusters opens a qcow2 whose clusters are compressed
// with zstd — `qemu-img convert -c -o compression_type=zstd src.img out.qcow2`
// writes them, and qemu's default compression is deflate, which the qcow2
// library already handles. The library leaves zstd to the caller, so udf
// registers the decoder at init; without that registration every read fails
// with "unsupported compression type".
//
// Point UDF_QCOW2_ZSTD at such an image (the same source converted both ways
// works well) to run this.
func TestQCow2ZstdCompressedClusters(t *testing.T) {
	path := os.Getenv("UDF_QCOW2_ZSTD")
	if path == "" {
		t.Skip("set UDF_QCOW2_ZSTD to a qcow2 with zstd-compressed clusters")
	}
	if _, err := os.Stat(path); err != nil {
		t.Skipf("no image at %s", path)
	}

	// The image opens as a disk, its filesystem is detected (which requires
	// reading the superblock and group descriptors through the compressed
	// clusters), and the root lists.
	meta, err := ScanDiskMetadata(path)
	if err != nil {
		t.Fatalf("ScanDiskMetadata: %v", err)
	}
	if len(meta.Disks) != 1 || len(meta.Disks[0].Volumes) == 0 {
		t.Fatalf("no volumes: %+v", meta.Disks)
	}
	found := false
	for _, v := range meta.Disks[0].Volumes {
		if v.FSType != "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no filesystem detected through the compressed clusters: %+v", meta.Disks[0].Volumes)
	}

	vol := ""
	for _, v := range meta.Disks[0].Volumes {
		if strings.HasSuffix(v.Name, "lost+found") {
			continue
		}
		if v.FSType != "" {
			vol = v.Name
			break
		}
	}
	if vol == "" {
		vol = meta.Disks[0].Volumes[0].Name
	}
	entries, err := ListDisk(path, vol)
	if err != nil {
		t.Fatalf("ListDisk(%s): %v", vol, err)
	}
	if len(entries) == 0 {
		t.Fatalf("volume %s listed nothing", vol)
	}
	t.Logf("%s: %s with %d entries at its root", path, meta.Disks[0].Volumes[0].FSType, len(entries))
}

// TestQCow2ZstdDecompressorRegistered is the part that needs no fixture: the
// registration must be in place before any image is opened, because the
// library looks the decompressor up once, at open time.
func TestQCow2ZstdDecompressorRegistered(t *testing.T) {
	// Reading a zstd-compressed cluster without the registration fails with
	// "unsupported compression type"; the real-image test above covers the
	// registered path. This one asserts the hook exists by re-registering it
	// through the same helper the init uses, which panics if the type is
	// unknown to the library.
	registerQCow2ZstdDecompressor()
}
