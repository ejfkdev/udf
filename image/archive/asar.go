package archive

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/ejfkdev/udf/fsview"
)

// ASAR is Electron's archive format. Layout (all integers little-endian):
//
//	offset 0:  a Pickle holding one uint32 = the header-pickle length we call n
//	   [0:4] = 4 (that pickle's own payload size), [4:8] = n
//	offset 8:  the header pickle, n bytes:
//	   [0:4] = payload size, [4:8] = JSON string length L, [8:8+L] = JSON
//	offset 8+n: file contents; each file's "offset" in the JSON is relative to
//	   this point.
//
// The JSON header is {"files": {...}} where a directory value is
// {"files": {...}}, a regular file is {"offset": "<num>", "size": <int>,
// "executable": <bool>, "unpacked": <bool>}, and a symlink is {"link": "..."}.
const asarMaxHeader = 256 << 20

type asarFile struct {
	path     string
	size     int64
	kind     fsview.Kind
	link     string
	offset   int64 // absolute file-data offset for a regular file
	unpacked bool
	exec     bool
}

// loadASAR parses the header and returns the flattened file list plus an index
// from path to its position in that list.
func loadASAR(path string) ([]asarFile, map[string]int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	var head [8]byte
	if _, err := io.ReadFull(f, head[:]); err != nil {
		return nil, nil, fmt.Errorf("read asar header: %w", err)
	}
	headerLen := int64(binary.LittleEndian.Uint32(head[4:8]))
	if headerLen < 8 || headerLen > asarMaxHeader {
		return nil, nil, fmt.Errorf("invalid asar header length %d", headerLen)
	}

	headerBuf := make([]byte, headerLen)
	if _, err := io.ReadFull(f, headerBuf); err != nil {
		return nil, nil, fmt.Errorf("read asar header: %w", err)
	}
	strLen := int64(binary.LittleEndian.Uint32(headerBuf[4:8]))
	if strLen < 0 || 8+strLen > headerLen {
		return nil, nil, fmt.Errorf("invalid asar header string length %d", strLen)
	}

	dec := json.NewDecoder(bytes.NewReader(headerBuf[8 : 8+strLen]))
	dec.UseNumber()
	var root map[string]any
	if err := dec.Decode(&root); err != nil {
		return nil, nil, fmt.Errorf("parse asar header: %w", err)
	}

	base := 8 + headerLen
	files, _ := root["files"].(map[string]any)

	var entries []asarFile
	index := make(map[string]int)
	add := func(e asarFile) {
		index[e.path] = len(entries)
		entries = append(entries, e)
	}

	var walk func(dir string, files map[string]any) error
	walk = func(dir string, files map[string]any) error {
		for name, raw := range files {
			obj, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			full := name
			if dir != "" {
				full = dir + "/" + name
			}

			if link, ok := obj["link"].(string); ok {
				add(asarFile{path: full, kind: fsview.KindSymlink, link: link, size: int64(len(link))})
				continue
			}
			if sub, ok := obj["files"].(map[string]any); ok {
				add(asarFile{path: full, kind: fsview.KindDir})
				if err := walk(full, sub); err != nil {
					return err
				}
				continue
			}

			offset := int64(0)
			if s, ok := obj["offset"].(string); ok {
				if v, err := strconv.ParseInt(s, 10, 64); err == nil {
					offset = v
				}
			}
			size := int64(0)
			if v, ok := obj["size"].(json.Number); ok {
				if n, err := v.Int64(); err == nil {
					size = n
				}
			}
			_, unpacked := obj["unpacked"].(bool)
			exec, _ := obj["executable"].(bool)
			add(asarFile{path: full, kind: fsview.KindFile, size: size, offset: base + offset, unpacked: unpacked, exec: exec})
		}
		return nil
	}

	if err := walk("", files); err != nil {
		return nil, nil, err
	}
	return entries, index, nil
}

// isASAR reports whether path looks like an ASAR archive. It checks the
// double-length-prefixed pickle header and that the embedded JSON string starts
// with '{'.
func isASAR(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	var head [8]byte
	if _, err := io.ReadFull(f, head[:]); err != nil {
		return false
	}
	if binary.LittleEndian.Uint32(head[0:4]) != 4 {
		return false
	}
	headerLen := int64(binary.LittleEndian.Uint32(head[4:8]))
	if headerLen < 16 || headerLen > asarMaxHeader {
		return false
	}

	headerBuf := make([]byte, headerLen)
	if _, err := io.ReadFull(f, headerBuf); err != nil {
		return false
	}
	strLen := int64(binary.LittleEndian.Uint32(headerBuf[4:8]))
	if strLen < 2 || 8+strLen > headerLen {
		return false
	}
	return len(bytes.TrimSpace(headerBuf[8:8+strLen])) > 0 &&
		bytes.HasPrefix(bytes.TrimSpace(headerBuf[8:8+strLen]), []byte("{"))
}

type asarArchive struct {
	path string
}

func (a *asarArchive) List() ([]Entry, error) {
	files, _, err := loadASAR(a.path)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(files))
	for _, f := range files {
		mode := int64(0)
		switch f.kind {
		case fsview.KindDir:
			mode = 0o755
		case fsview.KindSymlink:
			mode = 0o777
		default:
			mode = 0o644
			if f.exec {
				mode = 0o755
			}
		}
		out = append(out, Entry{
			Name:     f.path,
			Size:     f.size,
			Kind:     f.kind,
			Mode:     mode,
			Linkname: f.link,
		})
	}
	return out, nil
}

func (a *asarArchive) Open(name string) (io.ReadCloser, int64, error) {
	files, index, err := loadASAR(a.path)
	if err != nil {
		return nil, 0, err
	}
	i, ok := index[name]
	if !ok {
		return nil, 0, fmt.Errorf("entry %s not found in archive", name)
	}
	entry := files[i]
	if entry.kind != fsview.KindFile {
		return nil, 0, fmt.Errorf("entry %s is not a regular file", name)
	}

	// Unpacked entries live beside the archive in a "<archive>.unpacked/" dir.
	if entry.unpacked {
		f, err := os.Open(a.path + ".unpacked/" + filepath.FromSlash(name))
		if err != nil {
			return nil, 0, fmt.Errorf("open unpacked %s: %w", name, err)
		}
		st, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return nil, 0, err
		}
		return f, st.Size(), nil
	}

	f, err := os.Open(a.path)
	if err != nil {
		return nil, 0, err
	}
	return &asarEntryReadCloser{
		SectionReader: io.NewSectionReader(f, entry.offset, entry.size),
		f:             f,
	}, entry.size, nil
}

type asarEntryReadCloser struct {
	*io.SectionReader
	f *os.File
}

func (r *asarEntryReadCloser) Close() error { return r.f.Close() }
