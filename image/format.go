package image

import (
	"archive/tar"
	"encoding/binary"
	"io"
	"os"
	"strings"

	"github.com/ejfkdev/udf/erofs"
	arch "github.com/ejfkdev/udf/image/archive"
	"github.com/ejfkdev/udf/superlp"
	"github.com/ejfkdev/udf/udffs"
)

// Magic signatures read from file headers. Formats are identified by content,
// never by filename extension, so a misnamed or extension-less file is still
// dispatched correctly.
const (
	magicQCow = "\x51\x46\x49\xfb" // "QFI\xfb" (qcow v1/v2/v3)
	// sparseMagic (an Android sparse image) lives in sparse.go.
	magicVMDK     = "KDMV"
	magicVHD      = "conectix"
	magicVHDX     = "vhdxfile"
	magicQED      = "QED\x00"
	magicVMA      = "VMA\x00"
	magicVMExport = "version:1.0\ncfg:"
	magicSIF      = "SIF_MAGIC\x00"
	magicWIM      = "MSWIM\x00\x00\x00"
	magicFFU      = "SignedImage " // at offset 4

	vdiSignature = 0xbeda107f // at offset 0x40

	// WIM header compression flag bits (see wim/wim.go).
	wimFlagLzms = 1 << 19
)

// readMagic opens path and reads up to n leading bytes.
func readMagic(path string, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b := make([]byte, n)
	m, err := f.Read(b)
	if err != nil && err != io.EOF {
		return nil, err
	}
	return b[:m], nil
}

func hasPrefix(b []byte, s string) bool { return len(b) >= len(s) && string(b[:len(s)]) == s }

// detectDiskContainer classifies a virtual disk container from its header. It
// returns "" for files that are not a recognized container (which may still be
// a raw filesystem image or an archive).
func detectDiskContainer(path string) (string, error) {
	b, err := readMagic(path, 512)
	if err != nil {
		return "", err
	}
	switch {
	case hasPrefix(b, magicQCow):
		if len(b) >= 8 && binary.BigEndian.Uint32(b[4:8]) == 1 {
			return "qcow1", nil
		}
		return "qcow2", nil
	case hasPrefix(b, magicVMDK):
		return "vmdk", nil
	case hasPrefix(b, magicVHD):
		return "vhd", nil
	case hasPrefix(b, magicVHDX):
		return "vhdx", nil
	case len(b) >= 0x44 && binary.LittleEndian.Uint32(b[0x40:0x44]) == vdiSignature:
		return "vdi", nil
	case hasPrefix(b, "WithoutFreeSpace") || hasPrefix(b, "WithouFreSpacExt"):
		return "parallels", nil
	case hasPrefix(b, sparseMagic):
		return "sparse", nil
	case hasPrefix(b, magicQED):
		return "qed", nil
	case hasPrefix(b, magicVMA):
		return "vma", nil
	case hasPrefix(b, magicVMExport):
		return "vmexport", nil
	case len(b) >= 42 && string(b[32:42]) == magicSIF: // 32-byte launch script precedes the SIF header
		return "sif", nil
	case len(b) >= 16 && string(b[4:16]) == magicFFU:
		return "ffu", nil
	case hasPrefix(b, magicWIM):
		totalParts := 1
		if len(b) >= 44 {
			totalParts = int(binary.LittleEndian.Uint16(b[42:44]))
		}
		if totalParts > 1 {
			return "swm", nil
		}
		if len(b) >= 20 && binary.LittleEndian.Uint32(b[16:20])&wimFlagLzms != 0 {
			return "esd", nil
		}
		return "wim", nil
	}

	if hasPrefix(b, "\x7fELF") && isAppImage(path) {
		return "appimage", nil
	}
	if isOVA(path, b) {
		return "ova", nil
	}
	if isOVF(path, b) {
		return "ovf", nil
	}
	return "", nil
}

// isOVA reports whether path is a tar whose entries include an .ovf descriptor.
func isOVA(path string, b []byte) bool {
	if len(b) < 262 || string(b[257:262]) != "ustar" {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for i := 0; i < 64; i++ {
		hdr, err := tr.Next()
		if err != nil {
			return false
		}
		if strings.HasSuffix(strings.ToLower(hdr.Name), ".ovf") {
			return true
		}
	}
	return false
}

// isOVF reports whether path is a standalone OVF descriptor (an XML Envelope).
func isOVF(path string, b []byte) bool {
	trimmed := strings.TrimSpace(string(b))
	if !strings.HasPrefix(trimmed, "<?xml") && !strings.HasPrefix(trimmed, "<Envelope") {
		if !strings.HasPrefix(strings.TrimPrefix(trimmed, "\xef\xbb\xbf"), "<") {
			return false
		}
	}
	return strings.Contains(string(b), "Envelope")
}

// detectRawFilesystem reports whether path holds a recognized filesystem image.
func detectRawFilesystem(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	// Boot-block magic at offset 0: squashfs, xfs, exFAT, FAT.
	var fb [512]byte
	if _, err := f.ReadAt(fb[:], 0); err == nil {
		if hasPrefix(fb[:], "hsqs") || hasPrefix(fb[:], "sqsh") || hasPrefix(fb[:], "XFSB") {
			return true
		}
		if hasPrefix(fb[3:], "NTFS    ") {
			return true
		}
		if hasPrefix(fb[3:], "EXFAT   ") {
			return true
		}
		if (fb[0] == 0xEB || fb[0] == 0xE9) &&
			(hasPrefix(fb[54:], "FAT") || hasPrefix(fb[82:], "FAT")) {
			return true
		}
	}

	// A partitioned raw disk is a whole disk, not a filesystem: a GPT header
	// at LBA 1, or an MBR with at least one populated partition entry. A GPT
	// disk's protective MBR carries no filesystem magic, so without this probe
	// an Android system.img (a GPT disk like the emulator's) would not be
	// recognized at all.
	var lba1 [8]byte
	if _, err := f.ReadAt(lba1[:], 512); err == nil && string(lba1[:]) == "EFI PART" {
		return true
	}
	if hasMBRPartitions(fb[:]) {
		return true
	}

	// An Android super image (dynamic partitions) is a whole device too: the
	// LP metadata geometry sits at 4 KiB, with no filesystem magic in front of
	// it. A super partition *inside* a disk is found by the disk reader.
	var geom [4]byte
	if _, err := f.ReadAt(geom[:], superlp.GeometryOffset); err == nil &&
		binary.LittleEndian.Uint32(geom[:]) == superlp.GeometryMagic {
		return true
	}

	// ext2/3/4: superblock magic 0xEF53 at offset 1080.
	var e [2]byte
	if _, err := f.ReadAt(e[:], 1080); err == nil && binary.LittleEndian.Uint16(e[:]) == 0xEF53 {
		return true
	}

	// ISO9660: "CD001" at offset 32769.
	var c [5]byte
	if _, err := f.ReadAt(c[:], 32769); err == nil && string(c[:]) == "CD001" {
		return true
	}

	// btrfs: superblock magic "_BHRfS_M" at 64 KiB + 0x40.
	var bs [8]byte
	if _, err := f.ReadAt(bs[:], 0x10040); err == nil && string(bs[:]) == "_BHRfS_M" {
		return true
	}

	// UDF and EROFS have their own dedicated header probes.
	if erofs.Detect(f, 0) || udffs.Detect(f, 0) {
		return true
	}
	return false
}

// hasMBRPartitions reports whether a 512-byte boot block looks like an MBR
// partition table with something in it: the 0x55AA signature plus at least one
// entry that names a type and holds a non-empty, in-range extent. The
// signature alone appears in far too much data to be worth anything.
func hasMBRPartitions(b []byte) bool {
	if len(b) < 512 || b[510] != 0x55 || b[511] != 0xAA {
		return false
	}
	for i := 0; i < 4; i++ {
		e := b[446+i*16 : 446+(i+1)*16]
		kind := e[4]
		start := binary.LittleEndian.Uint32(e[8:12])
		sectors := binary.LittleEndian.Uint32(e[12:16])
		if kind == 0 || sectors == 0 {
			continue
		}
		// A plausible entry starts within the disk and does not claim more
		// sectors than any disk could hold.
		if start == 0 || sectors > 1<<32/512 || uint64(start)+uint64(sectors) > 1<<32 {
			continue
		}
		return true
	}
	return false
}

// DetectInput returns the input class of path by content: "disk" for a virtual
// disk container or raw filesystem image, "archive" for a plain archive, and ""
// for unsupported files.
func DetectInput(path string) string {
	if c, err := detectDiskContainer(path); err == nil && c != "" {
		return "disk"
	}
	if a, err := arch.Detect(path); err == nil && a != "" {
		return "archive"
	}
	if detectRawFilesystem(path) {
		return "disk"
	}
	return ""
}
