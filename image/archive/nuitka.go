package archive

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"unicode/utf16"

	"github.com/ejfkdev/udf/fsview"
	"github.com/klauspost/compress/zstd"
)

// Nuitka onefile executables append a payload after the compiled binary:
//
//	"KA" + 'X' (stored) | 'Y' (zstd frame)
//	entries: name (NUL-terminated; UTF-16LE on Windows builds, UTF-8 on
//	         POSIX builds) + uint64 size + [POSIX: 1 flags byte] + data
//	terminator: an empty name
//	uint64 payload size (distance from "KA" to the end of the compressed
//	stream, i.e. the 8 size bytes sit at filesize-8)
//
// Newer Nuitka (2.x) may embed the payload via linker sections instead of
// appending it; those binaries carry no trailer and are not detected here.

type nuitkaEntry struct {
	name    string
	off     int64 // offset inside the decompressed payload stream
	size    int64
	exec    bool
	symlink bool
	target  string // symlink target when symlink is set
}

type nuitkaArchive struct {
	path    string
	payload []byte // decompressed payload stream (entries area)
	entries []nuitkaEntry
	index   map[string]int
	inited  bool
	initErr error
}

// nuitkaFindPayload locates the appended payload from the 8-byte trailer and
// validates the "KA" + compression-indicator header.
func nuitkaFindPayload(f *os.File) (start int64, compressed bool, ok bool) {
	st, err := f.Stat()
	if err != nil {
		return 0, false, false
	}
	size := st.Size()
	if size < 8+3 {
		return 0, false, false
	}
	var trailer [8]byte
	if _, err := f.ReadAt(trailer[:], size-8); err != nil {
		return 0, false, false
	}
	payloadSize := int64(binary.LittleEndian.Uint64(trailer[:]))
	// The payload plus the 8 trailer bytes must fit before EOF.
	if payloadSize <= 3 || payloadSize > size-8 {
		return 0, false, false
	}
	start = size - 8 - payloadSize
	if start < 0 {
		return 0, false, false
	}
	var hdr [3]byte
	if _, err := f.ReadAt(hdr[:], start); err != nil {
		return 0, false, false
	}
	if hdr[0] != 'K' || hdr[1] != 'A' {
		return 0, false, false
	}
	switch hdr[2] {
	case 'X':
		return start, false, true
	case 'Y':
		return start, true, true
	}
	return 0, false, false
}

func isNuitkaOnefile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	if !hasNativeHostPrefix(f) {
		return false
	}
	if _, _, ok := nuitkaFindPayload(f); ok {
		return true
	}
	payload, ok := nuitkaLoadEmbedded(f)
	return ok && len(payload) > 0
}

func (a *nuitkaArchive) init() error {
	if a.inited {
		return a.initErr
	}
	a.inited = true
	a.initErr = a.load()
	return a.initErr
}

func (a *nuitkaArchive) load() error {
	f, err := os.Open(a.path)
	if err != nil {
		return err
	}
	defer f.Close()

	payload, err := nuitkaReadAppended(f)
	if err != nil {
		embedded, ok := nuitkaLoadEmbedded(f)
		if !ok {
			return err
		}
		payload = embedded
	}

	entries, err := nuitkaParseEntries(payload)
	if err != nil {
		return err
	}
	a.payload = payload
	a.entries = entries
	a.index = make(map[string]int, len(entries))
	for i, e := range entries {
		a.index[e.name] = i
	}
	return nil
}

// nuitkaReadAppended reads the payload of an appended-mode onefile binary
// (Windows/Linux builds, and Nuitka <= 1.x on macOS).
func nuitkaReadAppended(f *os.File) ([]byte, error) {
	start, compressed, ok := nuitkaFindPayload(f)
	if !ok {
		return nil, fmt.Errorf("nuitka payload not found")
	}
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	streamLen := st.Size() - 8 - start - 3
	if streamLen <= 0 {
		return nil, fmt.Errorf("nuitka payload is empty")
	}
	sr := io.NewSectionReader(f, start+3, streamLen)
	return nuitkaInflateStream(sr, compressed)
}

// nuitkaInflateStream materializes the entry stream. Windows builds pad the
// payload to an 8-byte boundary; the padding follows the final zstd frame and
// surfaces as a read error after all frame data was returned. Entry parsing
// is the real validator, so a trailing error with usable data is tolerated.
func nuitkaInflateStream(sr io.Reader, compressed bool) ([]byte, error) {
	if !compressed {
		return io.ReadAll(sr)
	}
	zr, err := zstd.NewReader(sr)
	if err != nil {
		return nil, fmt.Errorf("open nuitka zstd payload: %w", err)
	}
	defer zr.Close()
	payload, err := io.ReadAll(zr)
	if err != nil && len(payload) == 0 {
		return nil, fmt.Errorf("inflate nuitka payload: %w", err)
	}
	return payload, nil
}

// nuitkaEmbeddedScanLimit bounds the embedded-payload scan. Nuitka binaries
// top out well below this; the cap keeps unrelated huge native binaries cheap.
const nuitkaEmbeddedScanLimit = 512 << 20

var zstdFrameMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}

// nuitkaLoadEmbedded handles Nuitka 2.x on macOS, which embeds the payload
// via linker sections instead of appending it: there is no size trailer, so
// candidates ("KAY"+zstd frame, or "KAX") are located by scanning and
// validated by fully parsing the entry stream.
func nuitkaLoadEmbedded(f *os.File) ([]byte, bool) {
	st, err := f.Stat()
	if err != nil {
		return nil, false
	}
	size := st.Size()
	if size > nuitkaEmbeddedScanLimit {
		size = nuitkaEmbeddedScanLimit
	}

	const chunk = 1 << 20
	const sigLen = 7 // "KAY" + 4-byte zstd magic
	buf := make([]byte, chunk+sigLen-1)
	for off := int64(0); off < size; {
		n, _ := f.ReadAt(buf, off)
		if n < sigLen {
			// Tail too short to hold another candidate signature.
			if data, ok := nuitkaTryEmbeddedCandidates(f, buf[:n], off, size); ok {
				return data, true
			}
			break
		}
		if data, ok := nuitkaTryEmbeddedCandidates(f, buf[:n], off, size); ok {
			return data, true
		}
		off += int64(n) - (sigLen - 1)
	}
	return nil, false
}

// nuitkaTryEmbeddedCandidates tests every "KAY"/"KAX" occurrence in region
// (whose file offset is base) and returns the first payload that parses.
func nuitkaTryEmbeddedCandidates(f *os.File, region []byte, base, size int64) ([]byte, bool) {
	for i := 0; i+3 <= len(region); i++ {
		if region[i] != 'K' || region[i+1] != 'A' {
			continue
		}
		indicator := region[i+2]
		if indicator != 'X' && indicator != 'Y' {
			continue
		}
		off := base + int64(i) + 3
		if indicator == 'Y' {
			if i+7 > len(region) || !bytes.Equal(region[i+3:i+7], zstdFrameMagic) {
				continue
			}
			data, err := nuitkaInflateStream(io.NewSectionReader(f, off, size-off), true)
			if err != nil {
				continue
			}
			if _, err := nuitkaParseEntries(data); err == nil {
				return data, true
			}
			continue
		}
		// Stored payload: parse in place over a bounded read.
		data, err := nuitkaInflateStream(io.NewSectionReader(f, off, size-off), false)
		if err != nil {
			continue
		}
		if _, err := nuitkaParseEntries(data); err == nil {
			return data, true
		}
	}
	return nil, false
}

// nuitkaParseEntries walks the decompressed payload stream. Windows builds
// write UTF-16LE names and no flags byte; POSIX builds write UTF-8 names and
// one flags byte (bit 0: executable, bit 1: symlink). Two on-disk layouts
// exist and are distinguished by sniffing:
//
//	Nuitka <= 1.4: name, size(uint64), flags, data
//	Nuitka 2.x:    name, flags, [symlink target name], size(uint64), data
//
// The name encoding is sniffed from the first name (a NUL second byte means
// UTF-16LE).
func nuitkaParseEntries(payload []byte) ([]nuitkaEntry, error) {
	entries, err := nuitkaParseEntriesLayout(payload, false)
	if err == nil {
		return entries, nil
	}
	entries2, err2 := nuitkaParseEntriesLayout(payload, true)
	if err2 == nil {
		return entries2, nil
	}
	return nil, err
}

func nuitkaParseEntriesLayout(payload []byte, flagsFirst bool) ([]nuitkaEntry, error) {
	if len(payload) < 2 {
		return nil, fmt.Errorf("nuitka payload too short")
	}
	wide := payload[0] != 0 && payload[1] == 0

	var entries []nuitkaEntry
	pos := 0
	readName := func() (string, error) {
		if wide {
			start := pos
			for pos+1 < len(payload) {
				if payload[pos] == 0 && payload[pos+1] == 0 {
					u := payload[start:pos]
					pos += 2
					if len(u)%2 != 0 {
						return "", fmt.Errorf("misaligned UTF-16 name in nuitka payload")
					}
					units := make([]uint16, len(u)/2)
					for i := range units {
						units[i] = binary.LittleEndian.Uint16(u[i*2:])
					}
					return string(utf16.Decode(units)), nil
				}
				pos += 2
			}
			return "", fmt.Errorf("unterminated UTF-16 name in nuitka payload")
		}
		start := pos
		idx := bytes.IndexByte(payload[pos:], 0)
		if idx < 0 {
			return "", fmt.Errorf("unterminated name in nuitka payload")
		}
		pos += idx + 1
		return string(payload[start : start+idx]), nil
	}
	readFlags := func(name string) (byte, error) {
		if wide {
			return 0, nil // Windows builds carry no flags byte
		}
		if pos >= len(payload) {
			return 0, fmt.Errorf("truncated flags for nuitka entry %s", name)
		}
		flags := payload[pos]
		pos++
		if flags&^byte(3) != 0 {
			return 0, fmt.Errorf("invalid nuitka flags %#x for %s", flags, name)
		}
		return flags, nil
	}
	readSize := func(name string) (int64, error) {
		if pos+8 > len(payload) {
			return 0, fmt.Errorf("truncated size for nuitka entry %s", name)
		}
		size := int64(binary.LittleEndian.Uint64(payload[pos:]))
		pos += 8
		if size < 0 || size > int64(len(payload)) {
			return 0, fmt.Errorf("implausible size %d for nuitka entry %s", size, name)
		}
		return size, nil
	}

	for {
		if pos >= len(payload) {
			return nil, fmt.Errorf("nuitka payload ended without terminator")
		}
		name, err := readName()
		if err != nil {
			return nil, err
		}
		if name == "" {
			return entries, nil // terminator
		}

		var flags byte
		if flagsFirst {
			if flags, err = readFlags(name); err != nil {
				return nil, err
			}
			if flags&2 != 0 {
				// Symlink entry: a target name follows, no size or data.
				target, err := readName()
				if err != nil {
					return nil, err
				}
				entries = append(entries, nuitkaEntry{name: pyinstCleanName(name), symlink: true, target: target})
				continue
			}
		}
		size, err := readSize(name)
		if err != nil {
			return nil, err
		}
		if !flagsFirst {
			if flags, err = readFlags(name); err != nil {
				return nil, err
			}
		}
		if pos+int(size) > len(payload) {
			return nil, fmt.Errorf("nuitka entry %s data out of range", name)
		}
		entries = append(entries, nuitkaEntry{
			name: pyinstCleanName(name),
			off:  int64(pos),
			size: size,
			exec: flags&1 == 1,
		})
		pos += int(size)
	}
}

func (a *nuitkaArchive) List() ([]Entry, error) {
	if err := a.init(); err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(a.entries))
	for _, e := range a.entries {
		if e.symlink {
			entries = append(entries, Entry{
				Name:     e.name,
				Kind:     fsview.KindSymlink,
				Mode:     0o777,
				Linkname: e.target,
			})
			continue
		}
		mode := int64(0o644)
		if e.exec {
			mode = 0o755
		}
		entries = append(entries, Entry{
			Name: e.name,
			Size: e.size,
			Kind: fsview.KindFile,
			Mode: mode,
		})
	}
	return entries, nil
}

func (a *nuitkaArchive) Open(name string) (io.ReadCloser, int64, error) {
	if err := a.init(); err != nil {
		return nil, 0, err
	}
	i, ok := a.index[name]
	if !ok {
		return nil, 0, fmt.Errorf("entry %s not found in archive", name)
	}
	e := a.entries[i]
	if e.symlink {
		return nil, 0, fmt.Errorf("entry %s is not a regular file", name)
	}
	data := a.payload[e.off : e.off+e.size]
	return io.NopCloser(bytes.NewReader(data)), e.size, nil
}

var _ Archive = (*nuitkaArchive)(nil)
