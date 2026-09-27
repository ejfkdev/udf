package archive

import (
	"archive/tar"
	"archive/zip"
	"io"
)

// SequentialReader walks the entries of an archive in one pass over the
// underlying stream. It exists for formats whose members are expensive to
// reach by name (a compressed tar must be decompressed from the start for
// every Open), so callers that need many entries — image layer merging, for
// instance — can read them all in a single pass.
//
// Next returns the name and size of the next entry plus a reader over its
// data. The reader is only valid until the following Next call; callers that
// do not consume an entry in full can still call Next, which discards the
// remainder.
type SequentialReader interface {
	Next() (name string, size int64, r io.Reader, err error)
	Close()
}

// Sequential is implemented by archives readable in a single sequential pass.
type Sequential interface {
	NewSequentialReader() (SequentialReader, error)
}

// SequentialOf returns a sequential reader for an archive, or nil when the
// archive does not support single-pass reading.
func SequentialOf(a Archive) SequentialReader {
	s, ok := a.(Sequential)
	if !ok {
		return nil
	}
	r, err := s.NewSequentialReader()
	if err != nil {
		return nil
	}
	return r
}

type tarSequential struct {
	tr    *tar.Reader
	close func()
}

func (s *tarSequential) Next() (string, int64, io.Reader, error) {
	hdr, err := s.tr.Next()
	if err != nil {
		return "", 0, nil, err
	}
	return hdr.Name, hdr.Size, s.tr, nil
}

func (s *tarSequential) Close() { s.close() }

// NewSequentialReader streams a (possibly compressed) tar in one pass.
func (a *tarArchive) NewSequentialReader() (SequentialReader, error) {
	tr, closeFn, err := a.newReader()
	if err != nil {
		return nil, err
	}
	return &tarSequential{tr: tr, close: closeFn}, nil
}

// NewSequentialReader streams a zip in one pass.
func (a *zipArchive) NewSequentialReader() (SequentialReader, error) {
	zr, err := zip.OpenReader(a.path)
	if err != nil {
		return nil, err
	}
	return &zipSequential{zr: zr, idx: -1}, nil
}

type zipSequential struct {
	zr  *zip.ReadCloser
	idx int
	rc  io.ReadCloser
}

func (s *zipSequential) Next() (string, int64, io.Reader, error) {
	if s.rc != nil {
		_ = s.rc.Close()
		s.rc = nil
	}
	s.idx++
	if s.idx >= len(s.zr.File) {
		return "", 0, nil, io.EOF
	}
	f := s.zr.File[s.idx]
	rc, err := f.Open()
	if err != nil {
		return "", 0, nil, err
	}
	s.rc = rc
	return f.Name, int64(f.UncompressedSize64), rc, nil
}

func (s *zipSequential) Close() {
	if s.rc != nil {
		_ = s.rc.Close()
	}
	_ = s.zr.Close()
}
