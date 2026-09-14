package archive

import (
	"archive/zip"
	"bytes"
	"io"
	"os"
)

// Self-extracting ZIP executables: a host program (PE/ELF/Mach-O/shell stub)
// with a ZIP appended as an overlay. The central-directory offsets are
// relative to the ZIP start, so Go's zip reader transparently rebases them —
// detection only needs to find the end-of-central-directory record in the
// tail and let zip.OpenReader validate the rest.

// sfxEOCDScanLimit bounds the backward EOCD search: the record is at most
// 22 bytes plus a 64KiB-1 comment from the end of file.
const sfxEOCDScanLimit = 22 + 0xffff

var zipEOCD = []byte("PK\x05\x06")

// sfxHostPrefixes are the leading bytes of host programs that legitimately
// carry a ZIP overlay.
var sfxHostPrefixes = [][]byte{
	{'M', 'Z'},                                         // PE
	{0x7f, 'E', 'L', 'F'},                              // ELF
	{0xcf, 0xfa, 0xed, 0xfe}, {0xfe, 0xed, 0xfa, 0xcf}, // Mach-O 64
	{0xce, 0xfa, 0xed, 0xfe}, {0xfe, 0xed, 0xfa, 0xce}, // Mach-O 32
	{0xca, 0xfe, 0xba, 0xbe}, // Mach-O universal
	{'#', '!'},               // shell stub (shar/installers)
}

func isZipSFX(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	if !hasHostPrefix(f, sfxHostPrefixes) {
		return false
	}

	st, err := f.Stat()
	if err != nil {
		return false
	}
	scan := st.Size()
	if scan > sfxEOCDScanLimit {
		scan = sfxEOCDScanLimit
	}
	if scan < int64(len(zipEOCD)) {
		return false
	}
	buf := make([]byte, scan)
	if _, err := f.ReadAt(buf, st.Size()-scan); err != nil && err != io.EOF {
		return false
	}
	if bytes.LastIndex(buf, zipEOCD) < 0 {
		return false
	}

	// Full validation: the central directory must parse and hold entries.
	zr, err := zip.OpenReader(path)
	if err != nil {
		return false
	}
	defer zr.Close()
	return len(zr.File) > 0
}

// nativeHostPrefixes are the leading bytes of native binaries (no shell
// stubs); used to gate format probes that only make sense on executables.
var nativeHostPrefixes = [][]byte{
	{'M', 'Z'},                                         // PE
	{0x7f, 'E', 'L', 'F'},                              // ELF
	{0xcf, 0xfa, 0xed, 0xfe}, {0xfe, 0xed, 0xfa, 0xcf}, // Mach-O 64
	{0xce, 0xfa, 0xed, 0xfe}, {0xfe, 0xed, 0xfa, 0xce}, // Mach-O 32
	{0xca, 0xfe, 0xba, 0xbe}, // Mach-O universal
}

// hasHostPrefix reports whether f starts with one of the given prefixes.
func hasHostPrefix(f *os.File, prefixes [][]byte) bool {
	var head [4]byte
	if _, err := f.ReadAt(head[:], 0); err != nil && err != io.EOF {
		return false
	}
	for _, p := range prefixes {
		if bytes.HasPrefix(head[:], p) {
			return true
		}
	}
	return false
}

func hasNativeHostPrefix(f *os.File) bool {
	return hasHostPrefix(f, nativeHostPrefixes)
}
