package archive

import (
	"io"
	"os"
	"strings"
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

// Detect classifies an archive by its header, returning a format name ("tar",
// "zip", "asar", "rpm", ...) or "" when the file is not a recognized archive.
func Detect(path string) (string, error) {
	b, err := readMagic(path, 512)
	if err != nil {
		return "", err
	}
	switch {
	case hasPrefix(b, "\x1f\x8b"): // gzip
		return detectCompressed(path, "gzip")
	case hasPrefix(b, "BZh"): // bzip2
		return detectCompressed(path, "bzip2")
	case hasPrefix(b, "\xfd7zXZ\x00"): // xz
		return detectCompressed(path, "xz")
	case hasPrefix(b, "\x28\xb5\x2f\xfd"): // zstd
		return detectCompressed(path, "zstd")
	case hasPrefix(b, "\x04\x22\x4d\x18"): // lz4 frame
		return detectCompressed(path, "lz4")
	case len(b) >= 262 && string(b[257:262]) == "ustar":
		return "tar", nil
	case hasPrefix(b, "PK\x03\x04") || hasPrefix(b, "PK\x05\x06") || hasPrefix(b, "PK\x07\x08"):
		return "zip", nil
	case hasPrefix(b, "7z\xbc\xaf\x27\x1c"):
		return "7z", nil
	case hasPrefix(b, "Rar!\x1a\x07\x00") || hasPrefix(b, "Rar!\x1a\x07\x01\x00"):
		return "rar", nil
	case hasPrefix(b, "070701") || hasPrefix(b, "070702"):
		return "cpio", nil
	case hasPrefix(b, cabMagic):
		return "cab", nil
	case hasPrefix(b, "\x0d\x00\x00\x00\x00\x00\x00\x00nix-archive-1"):
		return "nar", nil
	case hasPrefix(b, xarMagic):
		return "xar", nil
	case hasPrefix(b, rpmMagic):
		return "rpm", nil
	case hasPrefix(b, "!<arch>\n"): // ar; deb/ipk has a debian-binary member first
		if len(b) >= 24 && strings.TrimRight(string(b[8:24]), " /") == "debian-binary" {
			return "deb", nil
		}
		return "", nil
	}
	if isASAR(path) {
		return "asar", nil
	}
	if isPyInstaller(path) {
		return "pyinstaller", nil
	}
	if isDotnetBundle(path) {
		return "dotnet-bundle", nil
	}
	if isNuitkaOnefile(path) {
		return "nuitka", nil
	}
	if isZipSFX(path) {
		return "zip", nil
	}
	return "", nil
}

// compSuffix maps a decompressor name to the archive-extension suffix used in
// format names such as "tar.gz" and "cpio.xz".
func compSuffix(comp string) string {
	switch comp {
	case "gzip":
		return "gz"
	case "bzip2":
		return "bz2"
	case "xz":
		return "xz"
	case "zstd":
		return "zst"
	case "lz4":
		return "lz4"
	case "lzma":
		return "lzma"
	}
	return comp
}

// detectCompressed identifies what a compressed stream wraps by decompressing
// its leading bytes and inspecting the inner magic. It returns "cpio.<suffix>"
// for a cpio payload, "tar.<suffix>" for a tar, and "" when the payload is
// neither (e.g. a single compressed file, which the archive pipeline cannot
// list or extract). A payload that fails to decompress is still treated as a
// compressed tar to preserve the historical "gzip => tar.gz" behaviour.
func detectCompressed(path, comp string) (string, error) {
	suffix := compSuffix(comp)
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	r, closeDec, err := openCompressed(f, comp)
	if err != nil {
		return "tar." + suffix, nil
	}
	defer closeDec()

	var head [512]byte
	n, _ := io.ReadFull(r, head[:])
	b := head[:n]
	switch {
	case hasPrefix(b, "070701") || hasPrefix(b, "070702"):
		return "cpio." + suffix, nil
	case len(b) >= 262 && string(b[257:262]) == "ustar":
		return "tar." + suffix, nil
	default:
		// A single compressed file (not a tar/cpio) is not something the archive
		// pipeline can list or extract, so leave it unrecognized.
		return "", nil
	}
}
