package image

import (
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ejfkdev/udf/diskkit"
	diskd "github.com/ejfkdev/udf/diskkit/disk"
	"github.com/ejfkdev/udf/diskkit/filesystem"
	"github.com/ejfkdev/udf/diskkit/filesystem/ext4"
	"github.com/ejfkdev/udf/diskkit/partition/mbr"
)

var deviceRe = regexp.MustCompile(`/dev/disk\d+\b`)

func TestIsDiskImage(t *testing.T) {
	// A disk is identified by header content, never by extension.
	magic := func(name string, fill func([]byte)) string {
		b := make([]byte, 512)
		fill(b)
		p := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	le := binary.LittleEndian
	disks := map[string]string{
		"w.qcow2":     magic("w", func(b []byte) { copy(b, "QFI\xfb") }),
		"w.vmdk":      magic("w", func(b []byte) { copy(b, "KDMV") }),
		"w.vhd":       magic("w", func(b []byte) { copy(b, "conectix") }),
		"w.vhdx":      magic("w", func(b []byte) { copy(b, "vhdxfile") }),
		"w.vdi":       magic("w", func(b []byte) { le.PutUint32(b[0x40:], 0xbeda107f) }),
		"w.parallels": magic("w", func(b []byte) { copy(b, "WithoutFreeSpace") }),
		"w.qed":       magic("w", func(b []byte) { copy(b, "QED\x00") }),
		"w.vma":       magic("w", func(b []byte) { copy(b, "VMA\x00") }),
		"w.sif":       magic("w", func(b []byte) { copy(b[32:42], "SIF_MAGIC\x00") }),
		"w.ffu":       magic("w", func(b []byte) { copy(b[4:16], "SignedImage ") }),
		"w.wim":       magic("w", func(b []byte) { copy(b, "MSWIM\x00\x00\x00") }),
	}
	// The magic files are written with deliberately wrong extensions to prove
	// detection is content-based: every path above is whitelisted by content.
	for name, p := range disks {
		if c, _ := detectDiskContainer(p); c == "" {
			t.Fatalf("expected content detection to classify %q, got none", name)
		}
	}

	// Archives are not disk images.
	archives := map[string]string{
		"tar.pxz": magic("w", func(b []byte) { copy(b[257:262], "ustar") }),
		"zip.ovf": magic("w", func(b []byte) { copy(b, "PK\x03\x04") }),
	}
	for name, p := range archives {
		if IsDiskImage(p) {
			t.Fatalf("expected %q not to be a disk image", name)
		}
	}

	// A qcow2 with a misleading ".img" extension is still a disk image.
	qcow2 := writeQCow2Image(t, mustReadFile(t, createExt4Image(t)), 16)
	misnamed := filepath.Join(t.TempDir(), "misleading.img")
	if err := os.Rename(qcow2, misnamed); err != nil {
		t.Fatal(err)
	}
	if !IsDiskImage(misnamed) {
		t.Fatalf("expected a qcow2 named .img to be detected as a disk image")
	}
	if c, _ := detectDiskContainer(misnamed); c != "qcow2" {
		t.Fatalf("expected qcow2 detection for misnamed file, got %q", c)
	}
}

func TestDetectRawFilesystem(t *testing.T) {
	write := func(fill func([]byte)) string {
		b := make([]byte, 40000) // covers ISO9660 magic at offset 32769
		fill(b)
		p := filepath.Join(t.TempDir(), "fs.bin")
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	le := binary.LittleEndian
	positive := map[string]string{
		"squashfs-le": write(func(b []byte) { copy(b, "hsqs") }),
		"squashfs-be": write(func(b []byte) { copy(b, "sqsh") }),
		"xfs":         write(func(b []byte) { copy(b, "XFSB") }),
		"exfat":       write(func(b []byte) { copy(b[3:], "EXFAT   ") }),
		"ext4":        write(func(b []byte) { le.PutUint16(b[1080:], 0xEF53) }),
		"iso9660":     write(func(b []byte) { copy(b[32769:], "CD001") }),
	}
	for name, p := range positive {
		if !detectRawFilesystem(p) {
			t.Fatalf("expected %q (%s) to be detected as a filesystem image", name, p)
		}
	}
	if detectRawFilesystem(write(func(b []byte) {})) {
		t.Fatalf("a zero-filled file should not be a filesystem image")
	}
}

func TestResolveVirtualVolumePrefix(t *testing.T) {
	img := fakeDiskImage([]string{"disk.qcow2"}, [][]DiskVolume{{
		{Name: "p1", Kind: "partition"},
		{Name: "p2", Kind: "partition", FSType: "ext4", Size: 100},
		{Name: "vg1/root", Kind: "lvm", FSType: "ext4", Size: 200},
		{Name: "vg1/log", Kind: "lvm", FSType: "ext4", Size: 300},
	}})

	if _, err := img.resolveVirtual("etc"); err == nil {
		t.Fatal("expected multiple volumes to require a volume prefix")
	}
	tv, err := img.resolveVirtual("vg1/root/etc")
	if err != nil || tv.level != "fs" || tv.vol.Name != "vg1/root" || tv.rel != "etc" {
		t.Fatalf("resolve vg1/root/etc: %v -> %+v", err, tv)
	}
	tv, err = img.resolveVirtual("vg1/root")
	if err != nil || tv.vol.Name != "vg1/root" || tv.rel != "" {
		t.Fatalf("resolve vg1/root: %v -> %+v", err, tv)
	}
	tv, err = img.resolveVirtual("root/etc")
	if err != nil || tv.vol.Name != "vg1/root" || tv.rel != "etc" {
		t.Fatalf("resolve bare root/etc: %v -> %+v", err, tv)
	}
}

func TestResolveVirtualSingleVolumeImplicit(t *testing.T) {
	img := fakeDiskImage([]string{"disk.qcow2"}, [][]DiskVolume{{
		{Name: "p1", Kind: "partition"},
		{Name: "p2", Kind: "partition", FSType: "ext4", Size: 100},
	}})
	tv, err := img.resolveVirtual("etc/passwd")
	if err != nil || tv.level != "fs" || tv.vol.Name != "p2" || tv.rel != "etc/passwd" {
		t.Fatalf("single volume should be implicit: %v -> %+v", err, tv)
	}
}

func TestResolveVirtualMultiDisk(t *testing.T) {
	img := fakeDiskImage([]string{"disk1.vmdk", "disk2.vmdk"}, [][]DiskVolume{
		{{Name: "p1", Kind: "partition", FSType: "ext4", Size: 100}},
		{{Name: "p1", Kind: "partition", FSType: "ext4", Size: 200}},
	})
	if r, err := img.resolveVirtual(""); err != nil || r.level != "disks" {
		t.Fatalf("empty path on multi-disk should list disks: %v -> %+v", err, r)
	}
	if r, err := img.resolveVirtual("disk2.vmdk"); err != nil || r.level != "volumes" || r.diskIdx != 1 {
		t.Fatalf("disk prefix should select disk: %v -> %+v", err, r)
	}
	if r, err := img.resolveVirtual("disk2.vmdk/p1"); err != nil || r.level != "fs" || r.vol.Name != "p1" {
		t.Fatalf("disk/volume path: %v -> %+v", err, r)
	}
	if _, err := img.resolveVirtual("p1"); err == nil {
		t.Fatal("expected multi-disk to require a disk prefix")
	}
}

func fakeDiskImage(names []string, vols [][]DiskVolume) *diskImage {
	disks := make([]*diskBackend, len(names))
	for i, n := range names {
		disks[i] = &diskBackend{name: n, format: "qcow2", size: 1}
	}
	return &diskImage{disks: disks, vols: vols, blockSizes: make([]int64, len(names))}
}

func TestOpenDiskBackendRejectsNonQcow2(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-image.qcow2")
	if err := os.WriteFile(path, []byte("this is not a qcow2 image"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if _, _, err := openQcow2Backend(path); err == nil {
		t.Fatal("expected non-qcow2 file to be rejected")
	}
}

func TestOpenDiskBackendReadsGuestBytes(t *testing.T) {
	guest := []byte("hello qcow2 world")
	path := writeQCow2Image(t, guest, 9)

	disks, closeFn, err := openQcow2Backend(path)
	if err != nil {
		t.Fatalf("openQcow2Backend: %v", err)
	}
	defer closeFn()
	if len(disks) != 1 {
		t.Fatalf("expected 1 disk, got %d", len(disks))
	}
	buf := make([]byte, len(guest))
	if n, err := disks[0].ReadAt(buf, 0); err != nil || n != len(guest) {
		t.Fatalf("ReadAt: n=%d err=%v", n, err)
	}
	if string(buf) != string(guest) {
		t.Fatalf("unexpected guest data: %q", string(buf))
	}
}

func TestDiskExtractListAndCp(t *testing.T) {
	rawPath := createExt4Image(t)
	qcow2Path := writeQCow2Image(t, mustReadFile(t, rawPath), 16)

	meta, err := ScanDiskMetadata(qcow2Path)
	if err != nil {
		t.Fatalf("scan disk metadata: %v", err)
	}
	if len(meta.Disks) != 1 || meta.Disks[0].Format != "qcow2" || meta.Disks[0].Filesystem != "ext4" {
		t.Fatalf("unexpected disk metadata: %+v", meta)
	}

	out := filepath.Join(t.TempDir(), "rootfs")
	if _, err := ExtractDiskVolumes(qcow2Path, out, 1<<16); err != nil {
		t.Fatalf("extract disk volumes: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(out, "etc", "passwd")); err != nil || string(data) != "root:x:0:0\n" {
		t.Fatalf("unexpected extracted passwd: %q err=%v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(out, "usr", "bin", "tool")); err != nil || string(data) != "#!/bin/sh\n" {
		t.Fatalf("unexpected extracted tool: %q err=%v", data, err)
	}
	if target, err := os.Readlink(filepath.Join(out, "etc", "passwd-link")); err != nil || target != "passwd" {
		t.Fatalf("unexpected symlink %q err=%v", target, err)
	}

	// Root listing shows the volume list (single volume, so one row).
	rootEntries, err := ListDisk(qcow2Path, "/")
	if err != nil {
		t.Fatalf("list root: %v", err)
	}
	if len(rootEntries) != 1 || rootEntries[0].Name != "disk" {
		t.Fatalf("unexpected root listing: %+v", rootEntries)
	}

	// Single-volume shorthand: /etc reaches straight into the volume.
	entries, err := ListDisk(qcow2Path, "/etc")
	if err != nil {
		t.Fatalf("list /etc: %v", err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name] = true
	}
	if !names["passwd"] || !names["passwd-link"] {
		t.Fatalf("expected passwd and passwd-link in listing, got %v", names)
	}

	dest := filepath.Join(t.TempDir(), "passwd.out")
	if _, err := ExtractDiskPath(qcow2Path, "/etc/passwd", dest, 1<<16); err != nil {
		t.Fatalf("extract disk path: %v", err)
	}
	if data, err := os.ReadFile(dest); err != nil || string(data) != "root:x:0:0\n" {
		t.Fatalf("unexpected cp result: %q err=%v", data, err)
	}
}

func TestDiskExtractFromPartitionedImage(t *testing.T) {
	rawPath := createPartitionedExt4Image(t)
	qcow2Path := writeQCow2Image(t, mustReadFile(t, rawPath), 16)

	meta, err := ScanDiskMetadata(qcow2Path)
	if err != nil {
		t.Fatalf("scan disk metadata: %v", err)
	}
	if len(meta.Disks) != 1 || meta.Disks[0].Volume != "p1" || meta.Disks[0].Filesystem != "ext4" {
		t.Fatalf("expected p1 ext4, got: %+v", meta)
	}
	vols := meta.Disks[0].Volumes
	if len(vols) != 1 || vols[0].Name != "p1" || vols[0].FSType != "ext4" {
		t.Fatalf("unexpected volumes: %+v", vols)
	}

	out := filepath.Join(t.TempDir(), "rootfs")
	if _, err := ExtractDiskVolumes(qcow2Path, out, 1<<16); err != nil {
		t.Fatalf("extract disk volumes: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(out, "etc", "passwd")); err != nil || string(data) != "root:x:0:0\n" {
		t.Fatalf("unexpected extracted passwd: %q err=%v", data, err)
	}
}

func TestDiskExtractFromQED(t *testing.T) {
	rawPath := createExt4Image(t)
	qedPath := writeQEDImage(t, mustReadFile(t, rawPath), 16)

	meta, err := ScanDiskMetadata(qedPath)
	if err != nil {
		t.Fatalf("scan disk metadata: %v", err)
	}
	if len(meta.Disks) != 1 || meta.Disks[0].Format != "qed" || meta.Disks[0].Filesystem != "ext4" {
		t.Fatalf("unexpected disk metadata: %+v", meta)
	}

	out := filepath.Join(t.TempDir(), "rootfs")
	if _, err := ExtractDiskVolumes(qedPath, out, 1<<16); err != nil {
		t.Fatalf("extract disk volumes: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(out, "etc", "passwd")); err != nil || string(data) != "root:x:0:0\n" {
		t.Fatalf("unexpected extracted passwd: %q err=%v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(out, "usr", "bin", "tool")); err != nil || string(data) != "#!/bin/sh\n" {
		t.Fatalf("unexpected extracted tool: %q err=%v", data, err)
	}

	entries, err := ListDisk(qedPath, "/etc")
	if err != nil {
		t.Fatalf("list /etc: %v", err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name] = true
	}
	if !names["passwd"] {
		t.Fatalf("expected passwd in listing, got %v", names)
	}
}

func TestDiskExtractFromQCOW(t *testing.T) {
	rawPath := createExt4Image(t)
	qcowPath := writeQCOWImage(t, mustReadFile(t, rawPath), 16)

	meta, err := ScanDiskMetadata(qcowPath)
	if err != nil {
		t.Fatalf("scan disk metadata: %v", err)
	}
	if len(meta.Disks) != 1 || meta.Disks[0].Format != "qcow1" || meta.Disks[0].Filesystem != "ext4" {
		t.Fatalf("unexpected disk metadata: %+v", meta)
	}

	out := filepath.Join(t.TempDir(), "rootfs")
	if _, err := ExtractDiskVolumes(qcowPath, out, 1<<16); err != nil {
		t.Fatalf("extract disk volumes: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(out, "etc", "passwd")); err != nil || string(data) != "root:x:0:0\n" {
		t.Fatalf("unexpected extracted passwd: %q err=%v", data, err)
	}
}

func TestDiskExtractFromUDF(t *testing.T) {
	udfPath := writeUDFImage(t)

	meta, err := ScanDiskMetadata(udfPath)
	if err != nil {
		t.Fatalf("scan disk metadata: %v", err)
	}
	if len(meta.Disks) != 1 || len(meta.Disks[0].Volumes) != 1 {
		t.Fatalf("unexpected disk metadata: %+v", meta)
	}
	if got := meta.Disks[0].Volumes[0].FSType; got != "udf" {
		t.Fatalf("expected udf filesystem, got %q", got)
	}

	entries, err := ListDisk(udfPath, "/disk")
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
	if _, err := ExtractDiskPath(udfPath, "/disk/hello.txt", dest, 1<<16); err != nil {
		t.Fatalf("extract hello.txt: %v", err)
	}
	if data, err := os.ReadFile(dest); err != nil || string(data) != "hello UDF\n" {
		t.Fatalf("unexpected extracted hello.txt %q err=%v", data, err)
	}

	dest2 := filepath.Join(t.TempDir(), "nested.out")
	if _, err := ExtractDiskPath(udfPath, "/disk/sub/nested.txt", dest2, 1<<16); err != nil {
		t.Fatalf("extract sub/nested.txt: %v", err)
	}
	if data, err := os.ReadFile(dest2); err != nil || string(data) != "nested file\n" {
		t.Fatalf("unexpected extracted nested.txt %q err=%v", data, err)
	}
}

// writeUDFImage builds a minimal pure-Go UDF volume (2048-byte blocks, a single
// partition, File Entries with short/in-ICB allocation) holding hello.txt and
// sub/nested.txt, and returns the image path. It mirrors the layout an
// hdiutil-generated UDF image uses: the anchor pointer at block 256, the
// volume descriptor sequence one-descriptor-per-block, and small directories
// stored in-ICB.
func writeUDFImage(t *testing.T) string {
	t.Helper()

	const (
		bs        = 2048
		partStart = 64
		mainLoc   = 32
	)
	// Partition logical block numbers.
	const (
		lbnFSD        = 0
		lbnRootFE     = 1
		lbnHelloFE    = 2
		lbnHelloData  = 3
		lbnSubFE      = 4
		lbnNestedFE   = 5
		lbnNestedData = 6
	)
	put16 := binary.LittleEndian.PutUint16
	put32 := binary.LittleEndian.PutUint32
	put64 := binary.LittleEndian.PutUint64

	blocks := func(n int) []byte { return make([]byte, n*bs) }
	image := blocks(257) // up to block 256 (the anchor)

	write := func(abs int, data []byte) {
		copy(image[abs*bs:], data)
	}

	// Volume Recognition Sequence: BEA01 at block 16.
	{
		var b [bs]byte
		b[0] = 0
		copy(b[1:], "BEA01")
		b[6] = 1
		write(16, b[:])
	}

	// Anchor Volume Descriptor Pointer at block 256.
	{
		var b [bs]byte
		put16(b[0:2], 0x0002)
		put32(b[16:20], 3*bs) // main sequence length (three descriptor blocks)
		put32(b[20:24], mainLoc)
		write(256, b[:])
	}

	// Main volume descriptor sequence: PD, LVD, TD (one per block).
	{
		var pd [bs]byte
		put16(pd[0:2], 0x0005)
		put32(pd[188:192], partStart)
		write(mainLoc, pd[:])

		var lvd [bs]byte
		put16(lvd[0:2], 0x0006)
		put32(lvd[252:256], lbnFSD) // logical volume contents use extLocation
		write(mainLoc+1, lvd[:])

		var td [bs]byte
		put16(td[0:2], 0x0008)
		write(mainLoc+2, td[:])
	}

	pabs := func(lbn uint32) int { return partStart + int(lbn) }

	// File Set Descriptor at partition lbn 0; root ICB at lbn 1.
	{
		var fsd [bs]byte
		put16(fsd[0:2], 0x0100)
		put32(fsd[404:408], lbnRootFE)
		write(pabs(lbnFSD), fsd[:])
	}

	dstr := func(name string) []byte {
		b := []byte{8} // compression ID 8 (8-bit / Latin-1)
		return append(b, name...)
	}
	fid := func(name string, lbn uint32, isDir bool) []byte {
		nb := dstr(name)
		f := make([]byte, 38+len(nb))
		put16(f[0:2], 0x0101) // tag FID
		put16(f[16:18], 1)    // file version number
		if isDir {
			f[18] = 0x02 // fidDirectory
		}
		f[19] = byte(len(nb))
		put32(f[24:28], lbn) // ICB long_ad extLocation
		copy(f[38:], nb)
		for len(f)%4 != 0 {
			f = append(f, 0)
		}
		return f
	}
	fe := func(fileType uint8, flags uint16, perm uint32, infoLen uint64, ads []byte) []byte {
		f := make([]byte, 176+len(ads))
		put16(f[0:2], 0x0105) // tag FE
		f[27] = fileType
		put16(f[34:36], flags)
		put32(f[44:48], perm)
		put16(f[48:50], 1) // file link count
		put64(f[56:64], infoLen)
		put16(f[86:88], 2024)               // mtime: year
		f[88] = 1                           // month
		f[89] = 2                           // day
		f[90] = 3                           // hour
		f[91] = 4                           // minute
		f[92] = 5                           // second
		put32(f[168:172], 0)                // lenEA
		put32(f[172:176], uint32(len(ads))) // lenAD
		copy(f[176:], ads)
		return f
	}
	// udfPerm maps a Go permission set onto UDF File Entry permission bits.
	udfPerm := func(m os.FileMode) uint32 {
		var v uint32
		for _, b := range []struct {
			mask os.FileMode
			bit  uint32
		}{
			{0o400, 0x1000}, {0o200, 0x800}, {0o100, 0x400},
			{0o040, 0x80}, {0o020, 0x40}, {0o010, 0x20},
			{0o004, 0x4}, {0o002, 0x2}, {0o001, 0x1},
		} {
			if m.Perm()&b.mask != 0 {
				v |= b.bit
			}
		}
		return v
	}
	shortAD := func(length, lbn uint32) []byte {
		a := make([]byte, 8)
		put32(a[0:4], length)
		put32(a[4:8], lbn)
		return a
	}

	rootFIDs := append(fid("hello.txt", lbnHelloFE, false), fid("sub", lbnSubFE, true)...)
	subFIDs := fid("nested.txt", lbnNestedFE, false)

	var blk [bs]byte
	copy(blk[:], fe(0x04 /* dir */, 0x0003 /* in-ICB */, udfPerm(0o755), uint64(len(rootFIDs)), rootFIDs))
	write(pabs(lbnRootFE), blk[:])

	clear(blk[:])
	copy(blk[:], fe(0x05 /* regular */, 0x0000 /* short */, udfPerm(0o644), uint64(len("hello UDF\n")), shortAD(uint32(len("hello UDF\n")), lbnHelloData)))
	write(pabs(lbnHelloFE), blk[:])

	clear(blk[:])
	copy(blk[:], "hello UDF\n")
	write(pabs(lbnHelloData), blk[:])

	clear(blk[:])
	copy(blk[:], fe(0x04 /* dir */, 0x0003 /* in-ICB */, udfPerm(0o755), uint64(len(subFIDs)), subFIDs))
	write(pabs(lbnSubFE), blk[:])

	clear(blk[:])
	copy(blk[:], fe(0x05 /* regular */, 0x0000 /* short */, udfPerm(0o644), uint64(len("nested file\n")), shortAD(uint32(len("nested file\n")), lbnNestedData)))
	write(pabs(lbnNestedFE), blk[:])

	clear(blk[:])
	copy(blk[:], "nested file\n")
	write(pabs(lbnNestedData), blk[:])

	path := filepath.Join(t.TempDir(), "test.udf")
	if err := os.WriteFile(path, image, 0o644); err != nil {
		t.Fatalf("write udf image: %v", err)
	}
	return path
}

// createWIMImage builds a WIM file holding hello.txt and sub/nested.txt and
// returns its path. It prefers wimlib-imagex (which produces an LZX-compressed
// image, exercising the vendored decompressor); when that is unavailable it
// falls back to 7-Zip (uncompressed). The test skips when neither is present.
// wimSourceDir builds a small directory tree used as WIM/ESD capture input.
func wimSourceDir(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "hello.txt"), []byte("hello WIM\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "nested.txt"), []byte("nested WIM\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return src
}

func createWIMImage(t *testing.T) string {
	t.Helper()

	src := wimSourceDir(t)
	wimPath := filepath.Join(t.TempDir(), "test.wim")

	if p, err := exec.LookPath("wimlib-imagex"); err == nil {
		if out, err := exec.Command(p, "capture", src, wimPath, "test", "--compress=LZX").CombinedOutput(); err != nil {
			t.Fatalf("wimlib-imagex capture: %v: %s", err, out)
		}
		return wimPath
	}
	if p, err := exec.LookPath("7z"); err == nil {
		cmd := exec.Command(p, "a", "-tWIM", wimPath, "hello.txt", "sub")
		cmd.Dir = src
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("7z a -tWIM: %v: %s", err, out)
		}
		return wimPath
	}
	t.Skip("neither wimlib-imagex nor 7z is available to build a WIM fixture")
	return ""
}

// createESDImage builds an LZMS-compressed ESD (WIM) image; requires
// wimlib-imagex, skipping otherwise.
func createESDImage(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("wimlib-imagex")
	if err != nil {
		t.Skip("wimlib-imagex not available to build an ESD fixture")
	}
	src := wimSourceDir(t)
	esdPath := filepath.Join(t.TempDir(), "test.esd")
	if out, err := exec.Command(p, "capture", src, esdPath, "test", "--compress=LZMS").CombinedOutput(); err != nil {
		t.Fatalf("wimlib-imagex capture LZMS: %v: %s", err, out)
	}
	return esdPath
}

func TestDiskExtractFromWIM(t *testing.T) {
	wimPath := createWIMImage(t)

	meta, err := ScanDiskMetadata(wimPath)
	if err != nil {
		t.Fatalf("scan disk metadata: %v", err)
	}
	if len(meta.Disks) != 1 || len(meta.Disks[0].Volumes) != 1 {
		t.Fatalf("unexpected disk metadata: %+v", meta)
	}
	if got := meta.Disks[0].Format; got != "wim" {
		t.Fatalf("expected format wim, got %q", got)
	}
	if got := meta.Disks[0].Volumes[0].FSType; got != "wim" {
		t.Fatalf("expected wim filesystem, got %q", got)
	}

	entries, err := ListDisk(wimPath, "/wim")
	if err != nil {
		t.Fatalf("list /wim: %v", err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name] = true
	}
	if !names["hello.txt"] || !names["sub"] {
		t.Fatalf("expected hello.txt and sub in listing, got %v", names)
	}

	dest := filepath.Join(t.TempDir(), "hello.out")
	if _, err := ExtractDiskPath(wimPath, "/wim/hello.txt", dest, 1<<16); err != nil {
		t.Fatalf("extract hello.txt: %v", err)
	}
	if data, _ := os.ReadFile(dest); string(data) != "hello WIM\n" {
		t.Fatalf("unexpected extracted hello.txt %q", data)
	}

	dest2 := filepath.Join(t.TempDir(), "nested.out")
	if _, err := ExtractDiskPath(wimPath, "/wim/sub/nested.txt", dest2, 1<<16); err != nil {
		t.Fatalf("extract sub/nested.txt: %v", err)
	}
	if data, _ := os.ReadFile(dest2); string(data) != "nested WIM\n" {
		t.Fatalf("unexpected extracted nested.txt %q", data)
	}
}

func TestDiskExtractFromESD(t *testing.T) {
	esdPath := createESDImage(t)

	meta, err := ScanDiskMetadata(esdPath)
	if err != nil {
		t.Fatalf("scan disk metadata: %v", err)
	}
	if len(meta.Disks) != 1 || len(meta.Disks[0].Volumes) != 1 {
		t.Fatalf("unexpected disk metadata: %+v", meta)
	}
	if got := meta.Disks[0].Format; got != "esd" {
		t.Fatalf("expected format esd, got %q", got)
	}
	if got := meta.Disks[0].Volumes[0].FSType; got != "wim" {
		t.Fatalf("expected wim filesystem, got %q", got)
	}

	entries, err := ListDisk(esdPath, "/wim")
	if err != nil {
		t.Fatalf("list /wim: %v", err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name] = true
	}
	if !names["hello.txt"] || !names["sub"] {
		t.Fatalf("expected hello.txt and sub in listing, got %v", names)
	}

	dest := filepath.Join(t.TempDir(), "hello.out")
	if _, err := ExtractDiskPath(esdPath, "/wim/hello.txt", dest, 1<<16); err != nil {
		t.Fatalf("extract hello.txt: %v", err)
	}
	if data, _ := os.ReadFile(dest); string(data) != "hello WIM\n" {
		t.Fatalf("unexpected extracted hello.txt %q", data)
	}
}

func TestDiskExtractFromWIMXPRESS(t *testing.T) {
	p, err := exec.LookPath("wimlib-imagex")
	if err != nil {
		t.Skip("wimlib-imagex not available to build an XPRESS WIM fixture")
	}
	src := wimSourceDir(t)
	wimPath := filepath.Join(t.TempDir(), "test.wim")
	if out, err := exec.Command(p, "capture", src, wimPath, "test", "--compress=XPRESS").CombinedOutput(); err != nil {
		t.Fatalf("wimlib-imagex capture XPRESS: %v: %s", err, out)
	}

	dest := filepath.Join(t.TempDir(), "hello.out")
	if _, err := ExtractDiskPath(wimPath, "/wim/hello.txt", dest, 1<<16); err != nil {
		t.Fatalf("extract hello.txt: %v", err)
	}
	if data, _ := os.ReadFile(dest); string(data) != "hello WIM\n" {
		t.Fatalf("unexpected extracted hello.txt %q", data)
	}
}

// createSWMImage builds a split WIM (multiple parts) holding a single large
// incompressible file, and returns part 1's path ("<base>.swm"). It requires
// wimlib-imagex and skips otherwise.
func createSWMImage(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("wimlib-imagex")
	if err != nil {
		t.Skip("wimlib-imagex not available to build a split WIM fixture")
	}

	src := t.TempDir()
	data := make([]byte, 5<<20)
	x := uint32(1)
	for i := range data {
		x = x*1664525 + 1013904223 // deterministic incompressible bytes
		data[i] = byte(x >> 24)
	}
	if err := os.WriteFile(filepath.Join(src, "big.bin"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	full := filepath.Join(t.TempDir(), "full.wim")
	if out, err := exec.Command(p, "capture", src, full, "test", "--compress=LZX").CombinedOutput(); err != nil {
		t.Fatalf("wimlib-imagex capture: %v: %s", err, out)
	}

	base := filepath.Join(t.TempDir(), "part")
	part1 := base + ".swm"
	if out, err := exec.Command(p, "split", full, part1, "1").CombinedOutput(); err != nil {
		t.Fatalf("wimlib-imagex split: %v: %s", err, out)
	}

	// Guard: the fixture must actually be split into multiple parts.
	if _, err := os.Stat(base + "2.swm"); err != nil {
		t.Fatalf("expected a second split part, got: %v", err)
	}
	return part1
}

func TestDiskExtractFromSWM(t *testing.T) {
	swmPath := createSWMImage(t)

	meta, err := ScanDiskMetadata(swmPath)
	if err != nil {
		t.Fatalf("scan disk metadata: %v", err)
	}
	if len(meta.Disks) != 1 || len(meta.Disks[0].Volumes) != 1 {
		t.Fatalf("unexpected disk metadata: %+v", meta)
	}
	if got := meta.Disks[0].Format; got != "swm" {
		t.Fatalf("expected format swm, got %q", got)
	}

	entries, err := ListDisk(swmPath, "/wim")
	if err != nil {
		t.Fatalf("list /wim: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Name == "big.bin" && e.Size == 5<<20 {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected big.bin (%d bytes) in listing, got %+v", 5<<20, entries)
	}

	dest := filepath.Join(t.TempDir(), "big.out")
	if _, err := ExtractDiskPath(swmPath, "/wim/big.bin", dest, 1<<20); err != nil {
		t.Fatalf("extract big.bin: %v", err)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read extracted big.bin: %v", err)
	}
	if int64(len(data)) != 5<<20 {
		t.Fatalf("extracted size %d, want %d", len(data), 5<<20)
	}
	// Verify the deterministic byte pattern round-tripped.
	x := uint32(1)
	for i, b := range data {
		x = x*1664525 + 1013904223
		if b != byte(x>>24) {
			t.Fatalf("mismatch at byte %d: got %d want %d", i, b, byte(x>>24))
		}
	}
}

// writeVMDKImage wraps raw guest bytes into an uncompressed monolithicSparse
// VMDK and returns the image path.
func writeVMDKImage(t *testing.T, raw []byte) string {
	t.Helper()
	const (
		sector       = 512
		grainSectors = 128 // 64 KiB grains
		numGTEsPerGT = 512
	)
	grainBytes := sector * grainSectors
	numGrains := (len(raw) + grainBytes - 1) / grainBytes

	gdSector := 1
	gts := (numGrains + numGTEsPerGT - 1) / numGTEsPerGT
	gdSectors := (gts*4 + sector - 1) / sector
	gtSector := gdSector + gdSectors
	gtSectors := (numGTEsPerGT*4 + sector - 1) / sector
	dataSector := gtSector + gtSectors

	capacity := numGrains * grainSectors
	buf := make([]byte, (dataSector+capacity)*sector)
	le := binary.LittleEndian
	le.PutUint32(buf[0:4], 0x564d444b)
	le.PutUint32(buf[4:8], 1)                  // version
	le.PutUint32(buf[8:12], 3)                 // flags
	le.PutUint64(buf[12:20], uint64(capacity)) // capacity, sectors
	le.PutUint64(buf[20:28], uint64(grainSectors))
	le.PutUint32(buf[44:48], numGTEsPerGT)
	le.PutUint64(buf[56:64], uint64(gdSector))

	for i := 0; i < gts; i++ {
		le.PutUint32(buf[gdSector*sector+i*4:], uint32(gtSector+i*gtSectors))
	}
	for i := 0; i < numGrains; i++ {
		le.PutUint32(buf[gtSector*sector+i*4:], uint32(dataSector+i*grainSectors))
		start := i * grainBytes
		end := start + grainBytes
		if end > len(raw) {
			end = len(raw)
		}
		copy(buf[(dataSector+i*grainSectors)*sector:], raw[start:end])
	}

	path := filepath.Join(t.TempDir(), "disk.vmdk")
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatalf("write vmdk: %v", err)
	}
	return path
}

// createOVFImage builds a standalone OVF descriptor plus a sibling .vmdk disk
// (holding an ext4 filesystem) and returns the .ovf path.
func createOVFImage(t *testing.T) string {
	t.Helper()
	rawPath := createExt4Image(t)
	vmdkPath := writeVMDKImage(t, mustReadFile(t, rawPath))
	dir := filepath.Dir(vmdkPath)

	ovf := `<?xml version="1.0" encoding="UTF-8"?>
<Envelope xmlns="http://schemas.dmtf.org/ovf/envelope/1" xmlns:ovf="http://schemas.dmtf.org/ovf/envelope/1">
  <References>
    <File ovf:id="file1" ovf:href="disk.vmdk"/>
  </References>
  <DiskSection><Info/><Disk ovf:diskId="vmdisk1" ovf:fileRef="file1" ovf:capacity="32"/></DiskSection>
</Envelope>`

	ovfPath := filepath.Join(dir, "appliance.ovf")
	if err := os.WriteFile(ovfPath, []byte(ovf), 0o644); err != nil {
		t.Fatalf("write ovf: %v", err)
	}
	return ovfPath
}

func TestDiskExtractFromOVF(t *testing.T) {
	ovfPath := createOVFImage(t)

	meta, err := ScanDiskMetadata(ovfPath)
	if err != nil {
		t.Fatalf("scan disk metadata: %v", err)
	}
	if len(meta.Disks) != 1 || meta.Disks[0].Format != "vmdk" || meta.Disks[0].Filesystem != "ext4" {
		t.Fatalf("unexpected disk metadata: %+v", meta)
	}

	dest := filepath.Join(t.TempDir(), "passwd.out")
	if _, err := ExtractDiskPath(ovfPath, "/disk.vmdk/etc/passwd", dest, 1<<16); err != nil {
		t.Fatalf("extract /disk.vmdk/etc/passwd: %v", err)
	}
	if data, _ := os.ReadFile(dest); string(data) != "root:x:0:0\n" {
		t.Fatalf("unexpected extracted passwd %q", data)
	}
}

// createFFUImage builds a synthetic Full Flash Update image: a 4 KiB security
// header (with the "SignedImage " signature) followed by a partitioned ext4
// disk.
func createFFUImage(t *testing.T) string {
	t.Helper()
	rawPath := createPartitionedExt4Image(t)
	raw := mustReadFile(t, rawPath)

	const hdrSize = 4096
	buf := make([]byte, hdrSize+len(raw))
	le := binary.LittleEndian
	le.PutUint32(buf[0:4], 24) // nominal security-header struct size
	copy(buf[4:16], "SignedImage ")
	copy(buf[hdrSize:], raw)

	path := filepath.Join(t.TempDir(), "image.ffu")
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatalf("write ffu: %v", err)
	}
	return path
}

func TestDiskExtractFromFFU(t *testing.T) {
	ffuPath := createFFUImage(t)

	meta, err := ScanDiskMetadata(ffuPath)
	if err != nil {
		t.Fatalf("scan disk metadata: %v", err)
	}
	if len(meta.Disks) != 1 || meta.Disks[0].Format != "ffu" || meta.Disks[0].Filesystem != "ext4" {
		t.Fatalf("unexpected disk metadata: %+v", meta)
	}

	dest := filepath.Join(t.TempDir(), "passwd.out")
	if _, err := ExtractDiskPath(ffuPath, "/p1/etc/passwd", dest, 1<<16); err != nil {
		t.Fatalf("extract /p1/etc/passwd: %v", err)
	}
	if data, _ := os.ReadFile(dest); string(data) != "root:x:0:0\n" {
		t.Fatalf("unexpected extracted passwd %q", data)
	}
}

// createExFATImage builds a raw exFAT filesystem image (via macOS hdiutil)
// holding hello.txt and returns the image path. It requires hdiutil and skips
// otherwise.
func createExFATImage(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("hdiutil")
	if err != nil {
		t.Skip("hdiutil not available to build an exFAT fixture")
	}

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "hello.txt"), []byte("hello exfat\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dmg := filepath.Join(t.TempDir(), "test.dmg")
	if out, err := exec.Command(p, "create", "-size", "64m", "-fs", "ExFAT", "-volname", "TESTVOL", "-srcfolder", src, dmg).CombinedOutput(); err != nil {
		t.Fatalf("hdiutil create: %v: %s", err, out)
	}

	attach, err := exec.Command(p, "attach", "-nomount", dmg).CombinedOutput()
	if err != nil {
		t.Fatalf("hdiutil attach: %v", err)
	}
	dev := deviceRe.FindString(string(attach))
	if dev == "" {
		t.Fatalf("could not parse attach output: %q", attach)
	}
	defer exec.Command(p, "detach", dev).Run()

	raw := filepath.Join(t.TempDir(), "disk.img")
	rawPartition := "/dev/r" + strings.TrimPrefix(dev, "/dev/") + "s1"
	if out, err := exec.Command("dd", "if="+rawPartition, "of="+raw, "bs=1m").CombinedOutput(); err != nil {
		t.Fatalf("dd partition: %v: %s", err, out)
	}
	return raw
}

func TestDiskExtractFromExFAT(t *testing.T) {
	exfatPath := createExFATImage(t)

	meta, err := ScanDiskMetadata(exfatPath)
	if err != nil {
		t.Fatalf("scan disk metadata: %v", err)
	}
	if len(meta.Disks) != 1 || len(meta.Disks[0].Volumes) != 1 {
		t.Fatalf("unexpected disk metadata: %+v", meta)
	}
	if got := meta.Disks[0].Volumes[0].FSType; got != "exfat" {
		t.Fatalf("expected exfat filesystem, got %q", got)
	}

	dest := filepath.Join(t.TempDir(), "hello.out")
	if _, err := ExtractDiskPath(exfatPath, "/disk/hello.txt", dest, 1<<16); err != nil {
		t.Fatalf("extract /disk/hello.txt: %v", err)
	}
	if data, _ := os.ReadFile(dest); string(data) != "hello exfat\n" {
		t.Fatalf("unexpected extracted hello.txt %q", data)
	}
}

// createEROFSImage builds an uncompressed EROFS image (via mkfs.erofs with
// small files, which stay inline/uncompressed) holding hello.txt and
// sub/nested.txt, and returns its path. It requires mkfs.erofs and skips
// otherwise.
func createEROFSImage(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("mkfs.erofs")
	if err != nil {
		t.Skip("mkfs.erofs not available to build an EROFS fixture")
	}

	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "hello.txt"), []byte("hello erofs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "nested.txt"), []byte("nested erofs\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "test.erofs")
	if b, err := exec.Command(p, out, src).CombinedOutput(); err != nil {
		t.Fatalf("mkfs.erofs: %v: %s", err, b)
	}
	return out
}

func TestDiskExtractFromEROFS(t *testing.T) {
	erofsPath := createEROFSImage(t)

	meta, err := ScanDiskMetadata(erofsPath)
	if err != nil {
		t.Fatalf("scan disk metadata: %v", err)
	}
	if len(meta.Disks) != 1 || len(meta.Disks[0].Volumes) != 1 {
		t.Fatalf("unexpected disk metadata: %+v", meta)
	}
	if got := meta.Disks[0].Volumes[0].FSType; got != "erofs" {
		t.Fatalf("expected erofs filesystem, got %q", got)
	}

	dest := filepath.Join(t.TempDir(), "hello.out")
	if _, err := ExtractDiskPath(erofsPath, "/disk/hello.txt", dest, 1<<16); err != nil {
		t.Fatalf("extract /disk/hello.txt: %v", err)
	}
	if data, _ := os.ReadFile(dest); string(data) != "hello erofs\n" {
		t.Fatalf("unexpected extracted hello.txt %q", data)
	}

	dest2 := filepath.Join(t.TempDir(), "nested.out")
	if _, err := ExtractDiskPath(erofsPath, "/disk/sub/nested.txt", dest2, 1<<16); err != nil {
		t.Fatalf("extract /disk/sub/nested.txt: %v", err)
	}
	if data, _ := os.ReadFile(dest2); string(data) != "nested erofs\n" {
		t.Fatalf("unexpected extracted nested.txt %q", data)
	}
}

func TestDiskExtractFromVMA(t *testing.T) {
	rawPath := createExt4Image(t)
	vmaPath := writeVMADisk(t, mustReadFile(t, rawPath))

	meta, err := ScanDiskMetadata(vmaPath)
	if err != nil {
		t.Fatalf("scan vma metadata: %v", err)
	}
	if len(meta.Disks) != 1 || meta.Disks[0].Format != "vma" || meta.Disks[0].Filesystem != "ext4" {
		t.Fatalf("unexpected vma metadata: %+v", meta)
	}

	dest := filepath.Join(t.TempDir(), "passwd.out")
	if _, err := ExtractDiskPath(vmaPath, "/etc/passwd", dest, 1<<16); err != nil {
		t.Fatalf("extract vma file: %v", err)
	}
	if data, _ := os.ReadFile(dest); string(data) != "root:x:0:0\n" {
		t.Fatalf("unexpected extracted passwd %q", data)
	}
}

// writeVMADisk wraps raw (a whole-disk image) into a minimal single-device VMA
// archive using the same layout the vma package's fixture encodes. It exercises
// the whole pipeline — content detection, vma.OpenFile, filesystem discovery,
// extraction — rather than only the vma parser in isolation.
func writeVMADisk(t *testing.T, raw []byte) string {
	t.Helper()

	const clusterSize = 65536
	const blockSize = 4096
	const fixedHeaderSize = 12288
	// (512 - 40) / 8 = 59 block-info records fit in one 512-byte extent header.
	const blocksPerExtent = 59

	devName := "disk-0.raw\x00"
	// Blob buffer: 1 padding byte + 2-byte length + name; the first blob's key
	// (and the device name offset) is 1, per vma_spec.txt.
	blobBufferSize := 1 + 2 + len(devName)
	headerSize := fixedHeaderSize + blobBufferSize

	nClusters := (len(raw) + clusterSize - 1) / clusterSize

	allZeroDisk := func(b []byte) bool {
		for _, c := range b {
			if c != 0 {
				return false
			}
		}
		return true
	}

	// Emit extents in order; each is its 512-byte header followed by the data
	// the block records describe, exactly as the vma parser consumes them.
	var extents []byte
	for c0 := 0; c0 < nClusters; c0 += blocksPerExtent {
		n := blocksPerExtent
		if c0+n > nClusters {
			n = nClusters - c0
		}
		extent := make([]byte, 512)
		copy(extent[0:4], "VMAE")
		binary.BigEndian.PutUint16(extent[6:8], uint16(n))

		var data []byte
		for k := 0; k < n; k++ {
			c := c0 + k
			start := c * clusterSize
			end := start + clusterSize
			if end > len(raw) {
				end = len(raw)
			}
			mask := uint16(0)
			for i := 0; i < 16; i++ {
				cs := start + i*blockSize
				if cs >= end {
					break
				}
				ce := cs + blockSize
				if ce > end {
					ce = end
				}
				if !allZeroDisk(raw[cs:ce]) {
					mask |= 1 << i
					data = append(data, raw[cs:ce]...)
				}
			}
			bi := extent[40+k*8 : 40+k*8+8]
			binary.BigEndian.PutUint16(bi[0:2], mask)
			bi[3] = 1 // dev id
			binary.BigEndian.PutUint32(bi[4:8], uint32(c))
		}
		extents = append(extents, extent...)
		extents = append(extents, data...)
	}

	hdr := make([]byte, headerSize)
	copy(hdr[0:4], "VMA\x00")
	binary.BigEndian.PutUint32(hdr[4:8], 1)
	binary.BigEndian.PutUint32(hdr[48:52], fixedHeaderSize)
	binary.BigEndian.PutUint32(hdr[52:56], uint32(blobBufferSize))
	binary.BigEndian.PutUint32(hdr[56:60], uint32(headerSize))
	dev := hdr[4096+32 : 4096+64]
	binary.BigEndian.PutUint32(dev[0:4], 1) // device name offset = blob key 1
	binary.BigEndian.PutUint64(dev[8:16], uint64(len(raw)))
	hdr[fixedHeaderSize] = 0
	binary.LittleEndian.PutUint16(hdr[fixedHeaderSize+1:fixedHeaderSize+3], uint16(len(devName)))
	copy(hdr[fixedHeaderSize+3:fixedHeaderSize+3+len(devName)], devName)

	out := make([]byte, 0, headerSize+len(extents))
	out = append(out, hdr...)
	out = append(out, extents...)

	path := filepath.Join(t.TempDir(), "disk.vma")
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatalf("write vma fixture: %v", err)
	}
	return path
}

// createExt4Image builds a small disk image containing an ext4 filesystem with
// a couple of files, a nested directory and a symlink, returning the raw image
// path. The image has no partition table (the filesystem covers the whole
// disk).
func createExt4Image(t *testing.T) string {
	t.Helper()

	raw := filepath.Join(t.TempDir(), "disk.raw")
	d, err := diskfs.Create(raw, 16<<20, diskfs.SectorSizeDefault)
	if err != nil {
		t.Fatalf("create raw disk: %v", err)
	}
	fsys, err := d.CreateFilesystem(diskd.FilesystemSpec{FSType: filesystem.TypeExt4})
	if err != nil {
		t.Fatalf("create ext4: %v", err)
	}
	populateExt4(t, fsys)
	closeDisk(t, fsys, d)
	return raw
}

// createPartitionedExt4Image builds a disk image with an MBR partition table
// and a single Linux partition holding the ext4 filesystem, returning the raw
// image path. This exercises the partition-detection path used by real cloud
// images.
func createPartitionedExt4Image(t *testing.T) string {
	t.Helper()

	raw := filepath.Join(t.TempDir(), "disk.raw")
	d, err := diskfs.Create(raw, 24<<20, diskfs.SectorSizeDefault)
	if err != nil {
		t.Fatalf("create raw disk: %v", err)
	}

	const totalSectors = (24 << 20) / 512
	table := &mbr.Table{
		LogicalSectorSize:  512,
		PhysicalSectorSize: 512,
		Partitions: []*mbr.Partition{{
			Index: 1,
			Type:  mbr.Linux,
			Start: 2048,
			Size:  totalSectors - 2048,
		}},
	}
	if err := d.Partition(table); err != nil {
		t.Fatalf("write partition table: %v", err)
	}

	fsys, err := d.CreateFilesystem(diskd.FilesystemSpec{Partition: 1, FSType: filesystem.TypeExt4})
	if err != nil {
		t.Fatalf("create ext4: %v", err)
	}
	populateExt4(t, fsys)
	closeDisk(t, fsys, d)
	return raw
}

func populateExt4(t *testing.T, fsys filesystem.FileSystem) {
	t.Helper()
	efs := fsys.(*ext4.FileSystem)

	writeFile := func(name, body string) {
		t.Helper()
		f, err := efs.OpenFile(name, os.O_CREATE|os.O_RDWR)
		if err != nil {
			t.Fatalf("create file %s: %v", name, err)
		}
		if _, err := f.Write([]byte(body)); err != nil {
			_ = f.Close()
			t.Fatalf("write file %s: %v", name, err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("close file %s: %v", name, err)
		}
	}

	if err := efs.Mkdir("etc"); err != nil {
		t.Fatalf("mkdir etc: %v", err)
	}
	if err := efs.Mkdir("usr"); err != nil {
		t.Fatalf("mkdir usr: %v", err)
	}
	if err := efs.Mkdir("usr/bin"); err != nil {
		t.Fatalf("mkdir usr/bin: %v", err)
	}
	writeFile("/etc/passwd", "root:x:0:0\n")
	writeFile("/usr/bin/tool", "#!/bin/sh\n")
	if err := efs.Symlink("passwd", "etc/passwd-link"); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
}

func closeDisk(t *testing.T, fsys filesystem.FileSystem, d *diskd.Disk) {
	t.Helper()
	if err := fsys.Close(); err != nil {
		t.Fatalf("close fs: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("close disk: %v", err)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

// writeQCow2Image wraps raw guest bytes in a minimal, valid uncompressed qcow2
// (version 3) and returns the image path. Only the header, one level of L1/L2
// tables and plain data clusters are written: enough for a read-only reader.
func writeQCow2Image(t *testing.T, raw []byte, clusterBits uint32) string {
	t.Helper()

	clusterSize := int64(1) << clusterBits
	l2Entries := clusterSize / 8
	ncluster := (int64(len(raw)) + clusterSize - 1) / clusterSize
	l1Size := (ncluster + l2Entries - 1) / l2Entries

	l1Offset := int64(104) // version-3 header length
	l1Bytes := l1Size * 8
	l2Offset := align512(l1Offset + l1Bytes)
	dataOffset := align512(l2Offset + l1Size*clusterSize)
	total := dataOffset + ncluster*clusterSize

	buf := make([]byte, total)

	// Header fields (version 2 portion).
	putU32 := func(off int, v uint32) {
		if v > 0 {
			binary.BigEndian.PutUint32(buf[off:off+4], v)
		}
	}
	putU64 := func(off int, v uint64) {
		if v > 0 {
			binary.BigEndian.PutUint64(buf[off:off+8], v)
		}
	}
	copy(buf[0:4], []byte{'Q', 'F', 'I', 0xfb})
	putU32(4, 3)            // version
	putU32(20, clusterBits) // cluster_bits
	putU64(24, uint64(len(raw)))
	putU32(36, uint32(l1Size))
	putU64(40, uint64(l1Offset))
	// version 3 fields: refcount order and header length.
	putU32(96, 4)
	putU32(100, 104)

	// L1 table.
	for i := int64(0); i < l1Size; i++ {
		binary.BigEndian.PutUint64(buf[l1Offset+i*8:l1Offset+i*8+8], uint64(l2Offset+i*clusterSize))
	}

	// L2 tables and data clusters.
	for c := int64(0); c < ncluster; c++ {
		l1 := c / l2Entries
		l2 := c % l2Entries
		tableBase := l2Offset + l1*clusterSize
		hostOffset := dataOffset + c*clusterSize
		binary.BigEndian.PutUint64(buf[tableBase+l2*8:tableBase+l2*8+8], uint64(hostOffset))

		start := c * clusterSize
		end := start + clusterSize
		if end > int64(len(raw)) {
			end = int64(len(raw))
		}
		copy(buf[hostOffset:hostOffset+end-start], raw[start:end])
	}

	path := filepath.Join(t.TempDir(), "disk.qcow2")
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatalf("write qcow2: %v", err)
	}
	return path
}

func align512(n int64) int64 {
	return (n + 511) &^ 511
}

// writeQEDImage wraps raw guest bytes in a minimal, valid QED image and
// returns the image path. It writes only the header, an L1 table, a single L2
// table and plain data clusters: enough for the read-only reader.
func writeQEDImage(t *testing.T, raw []byte, clusterBits uint32) string {
	t.Helper()

	clusterSize := int64(1) << clusterBits
	tableNelems := clusterSize / 8
	ncluster := (int64(len(raw)) + clusterSize - 1) / clusterSize

	// One L2 table covers tableNelems clusters, so a single table suffices
	// while ncluster <= tableNelems.
	if ncluster > tableNelems {
		t.Fatalf("raw too large for a single QED L2 table (%d clusters)", ncluster)
	}

	l1Off := int64(1) * clusterSize
	l2Off := int64(2) * clusterSize
	dataOff := int64(3) * clusterSize
	total := dataOff + ncluster*clusterSize

	buf := make([]byte, total)
	le := binary.LittleEndian
	le.PutUint32(buf[0:4], 0x00444551) // "QED\0"
	le.PutUint32(buf[4:8], uint32(clusterSize))
	le.PutUint32(buf[8:12], 1)  // table_size in clusters
	le.PutUint32(buf[12:16], 1) // header_size in clusters
	le.PutUint64(buf[40:48], uint64(l1Off))
	le.PutUint64(buf[48:56], uint64(len(raw)))

	// L1 entry 0 points at the single L2 table.
	le.PutUint64(buf[l1Off:l1Off+8], uint64(l2Off))

	for c := int64(0); c < ncluster; c++ {
		hostOff := dataOff + c*clusterSize
		le.PutUint64(buf[l2Off+c*8:l2Off+c*8+8], uint64(hostOff))
		start := c * clusterSize
		end := start + clusterSize
		if end > int64(len(raw)) {
			end = int64(len(raw))
		}
		copy(buf[hostOff:hostOff+end-start], raw[start:end])
	}

	path := filepath.Join(t.TempDir(), "disk.qed")
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatalf("write qed: %v", err)
	}
	return path
}

// writeQCOWImage wraps raw guest bytes in a minimal QCOW version 1 image and
// returns the image path. It writes a header, an L1 table, one L2 table per
// image chunk and plain (uncompressed) data clusters.
func writeQCOWImage(t *testing.T, raw []byte, clusterBits uint32) string {
	t.Helper()

	clusterSize := int64(1) << clusterBits
	ncluster := (int64(len(raw)) + clusterSize - 1) / clusterSize

	// Smallest l2_bits so one L2 table holds ncluster entries.
	l2Bits := 0
	for (int64(1) << l2Bits) < ncluster {
		l2Bits++
	}
	l2Size := int64(1) << l2Bits
	shift := int(clusterBits) + l2Bits
	l1Size := (int64(len(raw)) + (int64(1) << shift) - 1) >> shift

	l1Off := int64(4096)
	l2Off := l1Off + l1Size*8
	dataOff := (l2Off + l2Size*8 + clusterSize - 1) &^ (clusterSize - 1)
	total := dataOff + ncluster*clusterSize

	buf := make([]byte, total)
	be := binary.BigEndian
	be.PutUint32(buf[0:4], 0x514649FB) // "QFI\xfb"
	be.PutUint32(buf[4:8], 1)          // version
	be.PutUint64(buf[24:32], uint64(len(raw)))
	buf[32] = byte(clusterBits)
	buf[33] = byte(l2Bits)
	be.PutUint32(buf[36:40], 0) // crypt_method
	be.PutUint64(buf[40:48], uint64(l1Off))

	for i := int64(0); i < l1Size; i++ {
		be.PutUint64(buf[l1Off+i*8:l1Off+i*8+8], uint64(l2Off+i*l2Size*8))
	}
	for c := int64(0); c < ncluster; c++ {
		l2Table := l2Off + (c>>l2Bits)*l2Size*8
		l2Index := c & (l2Size - 1)
		hostOff := dataOff + c*clusterSize
		be.PutUint64(buf[l2Table+l2Index*8:l2Table+l2Index*8+8], uint64(hostOff))
		start := c * clusterSize
		end := start + clusterSize
		if end > int64(len(raw)) {
			end = int64(len(raw))
		}
		copy(buf[hostOff:hostOff+end-start], raw[start:end])
	}

	path := filepath.Join(t.TempDir(), "disk.qcow")
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatalf("write qcow: %v", err)
	}
	return path
}
