package archive

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ejfkdev/udf/fsview"
)

// Microsoft Installer packages (.msi, .msp) and other COM structured storage
// containers. The listing is the storage tree — every stream is a file, every
// storage a directory, which surfaces the installer tables (Property, File,
// ...) — and every embedded cabinet is expanded in place, so the payload lists
// as data1.cab/setup.exe, the way a .deb surfaces its data.tar contents.
//
// Reading is lazy: a listing touches only the directory and cabinet headers,
// and a stream is fetched sector by sector, so inspecting a large installer
// never reads its payload.

type msiArchive struct {
	path string
}

// openMSI opens a compound-file archive.
func openMSI(path string) (Archive, error) {
	return &msiArchive{path: path}, nil
}

// msiStream is one walked entry: its tree path and directory metadata. No
// stream bytes are read to build it.
type msiStream struct {
	path  string
	isDir bool
	size  int64
	mode  int64
	mtime time.Time
	isCab bool // name ends in .cab: a candidate for in-place expansion
}

// loadStreams walks the compound file's directory tree without reading any
// stream data.
func (a *msiArchive) loadStreams() ([]msiStream, error) {
	f, err := os.Open(a.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	c, err := openCFB(f)
	if err != nil {
		return nil, fmt.Errorf("open compound file %s: %w", a.path, err)
	}

	var out []msiStream
	err = c.walk(func(p string, e *cfbEntry) error {
		s := msiStream{
			path:  p,
			isDir: e.typ == 1,
			size:  e.size,
			mtime: e.mtime,
			mode:  0o644,
		}
		if e.typ == 1 {
			s.mode = 0o755
		} else {
			s.isCab = strings.HasSuffix(strings.ToLower(p), ".cab")
		}
		out = append(out, s)
		return nil
	})
	return out, err
}

// openCompound re-opens the compound file for stream access. The caller closes
// the file after the cfb and any stream readers are done.
func (a *msiArchive) openCompound() (*cfb, *os.File, error) {
	f, err := os.Open(a.path)
	if err != nil {
		return nil, nil, err
	}
	c, err := openCFB(f)
	if err != nil {
		_ = f.Close()
		return nil, nil, fmt.Errorf("open compound file %s: %w", a.path, err)
	}
	return c, f, nil
}

// entryByName finds the directory entry of one stream.
func (c *cfb) entryByName(name string) *cfbEntry {
	for i := range c.dir {
		if c.dir[i].typ == 2 && c.dir[i].name == name {
			return &c.dir[i]
		}
	}
	return nil
}

// streamByName finds one stream entry by its tree path.
func streamByName(streams []msiStream, name string) (msiStream, bool) {
	for _, s := range streams {
		if !s.isDir && s.path == name {
			return s, true
		}
	}
	return msiStream{}, false
}

func (a *msiArchive) List() ([]Entry, error) {
	streams, err := a.loadStreams()
	if err != nil {
		return nil, err
	}
	var entries []Entry
	for _, s := range streams {
		kind := fsview.KindFile
		if s.isDir {
			kind = fsview.KindDir
		}
		entries = append(entries, Entry{Name: s.path, Size: s.size, Kind: kind, Mode: s.mode, ModTime: s.mtime})
	}

	// Expand embedded cabinets in place: members list as "<cab>/member". Only
	// the cabinet headers are read, whatever the payload sizes — through one
	// compound-file handle for the whole listing.
	c, f, err := a.openCompound()
	if err != nil {
		return nil, err
	}
	defer f.Close()
	for _, s := range streams {
		if s.isDir || !s.isCab || s.size < 8 {
			continue
		}
		cab, err := a.parseCabStream(c, s)
		if err != nil {
			continue // not a readable cabinet: keep the stream as a plain file
		}
		for _, cf := range cab.files {
			if cf.name == "" {
				continue // cabinet-set continuation marker
			}
			entries = append(entries, Entry{
				Name: s.path + "/" + cf.name,
				Size: cf.size,
				Kind: fsview.KindFile,
				Mode: 0o644,
			})
		}
	}
	return entries, nil
}

// parseCabStream parses one embedded cabinet stream's header through the lazy
// chain reader of an already-open compound file.
func (a *msiArchive) parseCabStream(c *cfb, s msiStream) (*cabData, error) {
	e := c.entryByName(s.path)
	if e == nil {
		return nil, fmt.Errorf("stream %s not found", s.path)
	}
	sr, err := c.streamReaderAt(e)
	if err != nil {
		return nil, err
	}
	return parseCabFrom(sr, s.size)
}

func (a *msiArchive) Open(name string) (io.ReadCloser, int64, error) {
	c, f, err := a.openCompound()
	if err != nil {
		return nil, 0, err
	}
	closeAll := func() { _ = f.Close() }
	fail := func(err error) (io.ReadCloser, int64, error) {
		closeAll()
		return nil, 0, err
	}

	streams, err := a.loadStreams()
	if err != nil {
		return fail(err)
	}

	// Longest-prefix match so a member inside a cabinet wins over a stream
	// with the same prefix.
	best := -1
	for i, s := range streams {
		if s.isDir || !s.isCab || s.size < 8 {
			continue
		}
		if strings.HasPrefix(name, s.path+"/") && (best < 0 || len(s.path) > len(streams[best].path)) {
			best = i
		}
	}
	if best >= 0 {
		return a.openCabMember(c, f, streams[best], strings.TrimPrefix(name, streams[best].path+"/"))
	}

	s, ok := streamByName(streams, name)
	if !ok {
		return fail(fmt.Errorf("entry %s not found in archive", name))
	}
	e := c.entryByName(name)
	if e == nil {
		return fail(fmt.Errorf("entry %s not found in archive", name))
	}
	sr, err := c.streamReaderAt(e)
	if err != nil {
		return fail(err)
	}
	// The stream reader reads lazily from the sector chain; the compound file
	// stays open until the caller finishes with the reader.
	return &msiStreamReader{
		Reader:  io.NewSectionReader(sr, 0, s.size),
		closeFn: closeAll,
	}, s.size, nil
}

// openCabMember reads one file inside an embedded cabinet, decompressing only
// the CFDATA blocks that cover its range.
func (a *msiArchive) openCabMember(c *cfb, f *os.File, cab msiStream, member string) (io.ReadCloser, int64, error) {
	fail := func(err error) (io.ReadCloser, int64, error) {
		_ = f.Close()
		return nil, 0, err
	}
	e := c.entryByName(cab.path)
	if e == nil {
		return fail(fmt.Errorf("stream %s not found", cab.path))
	}
	sr, err := c.streamReaderAt(e)
	if err != nil {
		return fail(err)
	}
	parsed, err := parseCabFrom(sr, cab.size)
	if err != nil {
		return fail(err)
	}
	for _, cf := range parsed.files {
		if cf.name != member {
			continue
		}
		body, err := parsed.folderRange(cf.folder, cf.uoffStart, cf.size)
		if err != nil {
			return fail(err)
		}
		return &msiStreamReader{
			Reader:  bytes.NewReader(body),
			closeFn: func() { _ = f.Close() },
		}, cf.size, nil
	}
	return fail(fmt.Errorf("entry %s not found in archive", member))
}

// msiStreamReader ties a stream's reader to the compound file behind it.
type msiStreamReader struct {
	io.Reader
	closeFn func()
}

func (r *msiStreamReader) Close() error {
	r.closeFn()
	return nil
}

var _ Archive = (*msiArchive)(nil)
