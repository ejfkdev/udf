package image

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type archiveEntry struct {
	Name string
	Size int64
}

type imageArchive interface {
	List() ([]archiveEntry, error)
	Open(name string) (io.ReadCloser, int64, error)
}

func openArchive(path string) (imageArchive, error) {
	lower := strings.ToLower(path)
	switch {
	case strings.HasSuffix(lower, ".tar"):
		return &tarArchive{path: path, gzipped: false}, nil
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return &tarArchive{path: path, gzipped: true}, nil
	case strings.HasSuffix(lower, ".zip"):
		return &zipArchive{path: path}, nil
	default:
		return nil, fmt.Errorf("unsupported archive format: %s", filepath.Base(path))
	}
}

type tarArchive struct {
	path    string
	gzipped bool
}

func (a *tarArchive) List() ([]archiveEntry, error) {
	tr, closeFn, err := a.newReader()
	if err != nil {
		return nil, err
	}
	defer closeFn()

	var entries []archiveEntry
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return entries, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read archive entry: %w", err)
		}
		entries = append(entries, archiveEntry{Name: hdr.Name, Size: hdr.Size})
	}
}

func (a *tarArchive) Open(name string) (io.ReadCloser, int64, error) {
	tr, closeFn, err := a.newReader()
	if err != nil {
		return nil, 0, err
	}

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			closeFn()
			return nil, 0, fmt.Errorf("entry %s not found in archive", name)
		}
		if err != nil {
			closeFn()
			return nil, 0, fmt.Errorf("read archive entry: %w", err)
		}
		if hdr.Name != name {
			continue
		}
		return &tarEntryReadCloser{
			Reader:  tr,
			closeFn: closeFn,
		}, hdr.Size, nil
	}
}

func (a *tarArchive) newReader() (*tar.Reader, func(), error) {
	f, err := os.Open(a.path)
	if err != nil {
		return nil, nil, err
	}

	closeFn := func() { _ = f.Close() }
	var reader io.Reader = f
	if a.gzipped {
		gr, err := gzip.NewReader(f)
		if err != nil {
			closeFn()
			return nil, nil, fmt.Errorf("open gzip archive: %w", err)
		}
		reader = gr
		closeFn = func() {
			_ = gr.Close()
			_ = f.Close()
		}
	}

	return tar.NewReader(reader), closeFn, nil
}

type tarEntryReadCloser struct {
	io.Reader
	closeFn func()
}

func (r *tarEntryReadCloser) Close() error {
	if r.closeFn != nil {
		r.closeFn()
	}
	return nil
}

type zipArchive struct {
	path string
}

func (a *zipArchive) List() ([]archiveEntry, error) {
	zr, err := zip.OpenReader(a.path)
	if err != nil {
		return nil, err
	}
	defer zr.Close()

	entries := make([]archiveEntry, 0, len(zr.File))
	for _, f := range zr.File {
		entries = append(entries, archiveEntry{
			Name: f.Name,
			Size: int64(f.UncompressedSize64),
		})
	}
	return entries, nil
}

func (a *zipArchive) Open(name string) (io.ReadCloser, int64, error) {
	zr, err := zip.OpenReader(a.path)
	if err != nil {
		return nil, 0, err
	}
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			_ = zr.Close()
			return nil, 0, err
		}
		return &zipEntryReadCloser{
			ReadCloser: rc,
			closeZip: func() {
				_ = zr.Close()
			},
		}, int64(f.UncompressedSize64), nil
	}
	_ = zr.Close()
	return nil, 0, fmt.Errorf("entry %s not found in archive", name)
}

type zipEntryReadCloser struct {
	io.ReadCloser
	closeZip func()
}

func (r *zipEntryReadCloser) Close() error {
	err := r.ReadCloser.Close()
	if r.closeZip != nil {
		r.closeZip()
	}
	return err
}
