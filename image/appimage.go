package image

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// appimageSquashfsOffset returns the byte offset of the SquashFS filesystem
// embedded in an AppImage. An AppImage is an ELF executable with the SquashFS
// appended right after its loadable segments, so the offset is found by taking
// the end of the last PT_LOAD segment and locating the "hsqs" superblock magic
// from there (tolerating the usual page alignment).
func appimageSquashfsOffset(f *os.File, size int64) (int64, error) {
	end, err := elfPTLoadEnd(f)
	if err != nil {
		return 0, err
	}

	const window = 8 << 20 // 8 MiB is more than any AppImage padding
	if end < 0 || end >= size {
		return 0, fmt.Errorf("appimage squashfs offset out of range: %d", end)
	}
	limit := size - end
	if limit > window {
		limit = window
	}
	buf := make([]byte, limit)
	n, err := f.ReadAt(buf, end)
	if err != nil && err != io.EOF {
		return 0, err
	}
	if idx := bytes.Index(buf[:n], []byte("hsqs")); idx >= 0 {
		return end + int64(idx), nil
	}
	return 0, fmt.Errorf("no squashfs filesystem found after ELF at offset %d", end)
}

// elfPTLoadEnd parses an ELF header and program headers, returning the highest
// file offset covered by a PT_LOAD segment. Only 32/64-bit little- and
// big-endian ELF are supported, which covers every AppImage in practice.
func elfPTLoadEnd(f *os.File) (int64, error) {
	var ehdr [64]byte
	n, err := f.ReadAt(ehdr[:], 0)
	if err != nil && err != io.EOF {
		return 0, err
	}
	if n < 52 || !bytes.HasPrefix(ehdr[:4], []byte("\x7fELF")) {
		return 0, fmt.Errorf("not an ELF file")
	}

	var order binary.ByteOrder
	switch ehdr[5] { // EI_DATA
	case 1:
		order = binary.LittleEndian
	case 2:
		order = binary.BigEndian
	default:
		return 0, fmt.Errorf("unsupported ELF byte order")
	}

	var phoff, phentsize int64
	var phnum int
	switch ehdr[4] { // EI_CLASS
	case 1: // ELF32
		phoff = int64(order.Uint32(ehdr[28:32]))
		phentsize = int64(order.Uint16(ehdr[42:44]))
		phnum = int(order.Uint16(ehdr[44:46]))
	case 2: // ELF64
		phoff = int64(order.Uint64(ehdr[32:40]))
		phentsize = int64(order.Uint16(ehdr[54:56]))
		phnum = int(order.Uint16(ehdr[56:58]))
	default:
		return 0, fmt.Errorf("unsupported ELF class")
	}
	if phoff == 0 || phnum == 0 || phentsize < 32 {
		return 0, fmt.Errorf("no ELF program headers")
	}

	var end int64
	for i := 0; i < phnum; i++ {
		var ph [56]byte
		if _, err := f.ReadAt(ph[:], phoff+int64(i)*phentsize); err != nil {
			return 0, err
		}
		var ptype uint32
		var poff, pfilesz int64
		if ehdr[4] == 1 {
			ptype = order.Uint32(ph[0:4])
			poff = int64(order.Uint32(ph[4:8]))
			pfilesz = int64(order.Uint32(ph[16:20]))
		} else {
			ptype = order.Uint32(ph[0:4])
			poff = int64(order.Uint64(ph[8:16]))
			pfilesz = int64(order.Uint64(ph[32:40]))
		}
		if ptype == 1 && poff+pfilesz > end { // PT_LOAD
			end = poff + pfilesz
		}
	}
	if end == 0 {
		return 0, fmt.Errorf("no PT_LOAD segment found")
	}
	return end, nil
}

// isAppImage reports whether path is an AppImage: an ELF with an embedded
// SquashFS filesystem.
func isAppImage(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return false
	}
	if _, err := appimageSquashfsOffset(f, st.Size()); err != nil {
		return false
	}
	return true
}
