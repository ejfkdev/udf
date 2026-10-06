package image

import (
	"fmt"
	"io"
	"time"

	"github.com/ejfkdev/udf/fsview"
	arch "github.com/ejfkdev/udf/image/archive"
)

// A plain archive — a tar.gz that is not a docker-save image — is normally
// listed and read sequentially, which on a large one costs a full
// decompression per command: a 26 GB upgrade bundle took 69 s for info, and
// every later command paid it again.
//
// For a large single-member gzip tar there is a better way now that the index
// records each member's header fields: the listing comes straight from the
// index, and a member's bytes come from restarting the decompressor at its own
// checkpoint. The index is built once (one pass, the same cost as the first
// sequential command) and reused from then on.

// plainSource is where a plain archive's members come from.
type plainSource struct {
	ar     arch.Archive
	close  func() error
	source string // "index" or "sequential", for diagnostics
}

// openPlainSource returns the best reader for a plain archive: the cached
// index when one applies and can be built or loaded, the archive itself
// otherwise.
func openPlainSource(archivePath string) (*plainSource, error) {
	if src, ok := openIndexedPlainArchive(archivePath); ok {
		return src, nil
	}
	ar, err := arch.Open(archivePath)
	if err != nil {
		return nil, err
	}
	return &plainSource{ar: ar, close: func() error { return nil }, source: "sequential"}, nil
}

// openIndexedPlainArchive opens a plain archive through the index, building
// the index when it is missing. The bool is false whenever the index does not
// apply or cannot be used, in which case the caller reads sequentially.
func openIndexedPlainArchive(archivePath string) (*plainSource, bool) {
	if !indexableImage(archivePath) {
		return nil, false
	}
	r, ok := ensureImageIndex(archivePath)
	if !ok {
		return nil, false
	}
	entries := make([]arch.Entry, 0, len(r.Entries()))
	for _, e := range r.Entries() {
		kind := kindForTarTypeflag(e.Typeflag)
		mode := e.Mode
		if mode == 0 {
			mode = plainDefaultMode(kind)
		}
		entries = append(entries, arch.Entry{
			Name:     e.Name,
			Size:     e.Size,
			Kind:     kind,
			Mode:     mode,
			ModTime:  time.Unix(e.ModTime, 0).UTC(),
			UID:      int(e.UID),
			GID:      int(e.GID),
			Linkname: e.Linkname,
		})
	}
	return &plainSource{
		ar:     &indexedPlainArchive{ix: r, entries: entries},
		close:  r.Close,
		source: "index",
	}, true
}

// indexedPlainArchive serves a plain archive's members from the index.
type indexedPlainArchive struct {
	ix      indexedOuter
	entries []arch.Entry
}

func (a *indexedPlainArchive) List() ([]arch.Entry, error) { return a.entries, nil }

// Open restarts the decompressor at the member's checkpoint and reads its
// bytes: no part of the stream before it is decompressed.
func (a *indexedPlainArchive) Open(name string) (io.ReadCloser, int64, error) {
	e, ok := a.ix.Lookup(name)
	if !ok {
		return nil, 0, fmt.Errorf("entry %s not found in archive", name)
	}
	rc, err := a.ix.ReadAt(e.OutOff)
	if err != nil {
		return nil, 0, err
	}
	return &limitedReadCloser{rc: rc, remain: e.Size}, e.Size, nil
}

// limitedReadCloser bounds a reader to the member's size and closes the index
// reader behind it.
type limitedReadCloser struct {
	rc     io.ReadCloser
	remain int64
}

func (l *limitedReadCloser) Read(p []byte) (int, error) {
	if l.remain <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > l.remain {
		p = p[:l.remain]
	}
	n, err := l.rc.Read(p)
	l.remain -= int64(n)
	if err == io.EOF && l.remain > 0 {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

func (l *limitedReadCloser) Close() error { return l.rc.Close() }

// plainDefaultMode is the conventional mode for a member whose header carries
// none (the tar reader applies the same fallback).
func plainDefaultMode(kind fsview.Kind) int64 {
	if kind == fsview.KindDir {
		return 0o755
	}
	return 0o644
}

// kindForTarTypeflag maps a tar typeflag to the entry kind, the same way the
// tar reader does.
func kindForTarTypeflag(tf byte) fsview.Kind {
	switch tf {
	case '5':
		return fsview.KindDir
	case '1':
		return fsview.KindHardlink
	case '2':
		return fsview.KindSymlink
	case '3':
		return fsview.KindCharDev
	case '4':
		return fsview.KindBlockDev
	case '6':
		return fsview.KindFifo
	}
	return fsview.KindFile
}
