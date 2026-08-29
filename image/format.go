package image

import (
	"archive/tar"
	"encoding/binary"
	"io"
	"os"
	"strings"

	"github.com/ejfkdev/udf/erofs"
	"github.com/ejfkdev/udf/udffs"
)

// Magic signatures read from file headers. Formats are identified by content,
// never by filename extension, so a misnamed or extension-less file is still
// dispatched correctly.
const (
	magicQCow     = "\x51\x46\x49\xfb" // "QFI\xfb" (qcow v1/v2/v3)
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

	if isOVA(path, b) {
		return "ova", nil
	}
	if isOVF(path, b) {
		return "ovf", nil
	}
	return "", nil
}

// detectArchive classifies a plain archive (tar/zip/7z/rar/cpio) by header.
func detectArchive(path string) (string, error) {
	b, err := readMagic(path, 512)
	if err != nil {
		return "", err
	}
	switch {
	case hasPrefix(b, "\x1f\x8b"): // gzip
		return "tar.gz", nil
	case len(b) >= 262 && string(b[257:262]) == "ustar":
		return "tar", nil
	case hasPrefix(b, "PK\x03\x04") || hasPrefix(b, "PK\x05\x06") || hasPrefix(b, "PK\x07\x08"):
		return "zip", nil
	case hasPrefix(b, "7z\xbc\xaf\x27\x1c"):
		return "7z", nil
	case hasPrefix(b, "Rar!\x1a\x07\x00") || hasPrefix(b, "Rar!\x1a\x07\x01\x00"):
		return "rar", nil
	case hasPrefix(b, "070701") || hasPrefix(b, "070702") || hasPrefix(b, "070707"):
		return "cpio", nil
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
		if hasPrefix(fb[3:], "EXFAT   ") {
			return true
		}
		if (fb[0] == 0xEB || fb[0] == 0xE9) &&
			(hasPrefix(fb[54:], "FAT") || hasPrefix(fb[82:], "FAT")) {
			return true
		}
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

	// UDF and EROFS have their own dedicated header probes.
	if erofs.Detect(f, 0) || udffs.Detect(f, 0) {
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
	if a, err := detectArchive(path); err == nil && a != "" {
		return "archive"
	}
	if detectRawFilesystem(path) {
		return "disk"
	}
	return ""
}
