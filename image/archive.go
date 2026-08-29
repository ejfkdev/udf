package image

import (
	"archive/tar"
	"archive/zip"
	"fmt"
	"github.com/klauspost/compress/gzip"
	"io"
	"os"
	"path/filepath"

	"github.com/bodgit/sevenzip"
	"github.com/cavaliergopher/cpio"
	"github.com/nwaples/rardecode/v2"
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
	if IsOCILayout(path) {
		return openOCIArchive(path)
	}
	format, err := detectArchive(path)
	if err != nil {
		return nil, err
	}
	switch format {
	case "tar":
		return &tarArchive{path: path, gzipped: false}, nil
	case "tar.gz":
		return &tarArchive{path: path, gzipped: true}, nil
	case "zip":
		// .ppkg (Windows provisioning package) is an OPC container: a ZIP.
		return &zipArchive{path: path}, nil
	case "7z":
		return &sevenzipArchive{path: path}, nil
	case "cpio":
		return &cpioArchive{path: path}, nil
	case "rar":
		return &rarArchive{path: path}, nil
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

type sevenzipArchive struct {
	path string
}

func (a *sevenzipArchive) List() ([]archiveEntry, error) {
	zr, err := sevenzip.OpenReader(a.path)
	if err != nil {
		return nil, err
	}
	defer zr.Close()

	entries := make([]archiveEntry, 0, len(zr.File))
	for _, f := range zr.File {
		entries = append(entries, archiveEntry{
			Name: f.Name,
			Size: int64(f.UncompressedSize),
		})
	}
	return entries, nil
}

func (a *sevenzipArchive) Open(name string) (io.ReadCloser, int64, error) {
	zr, err := sevenzip.OpenReader(a.path)
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
			closeZip:   func() { _ = zr.Close() },
		}, int64(f.UncompressedSize), nil
	}
	_ = zr.Close()
	return nil, 0, fmt.Errorf("entry %s not found in archive", name)
}

type cpioArchive struct {
	path string
}

func (a *cpioArchive) List() ([]archiveEntry, error) {
	f, err := os.Open(a.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	cr := cpio.NewReader(f)
	var entries []archiveEntry
	for {
		hdr, err := cr.Next()
		if err == io.EOF {
			return entries, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read cpio entry: %w", err)
		}
		entries = append(entries, archiveEntry{Name: hdr.Name, Size: hdr.Size})
	}
}

func (a *cpioArchive) Open(name string) (io.ReadCloser, int64, error) {
	f, err := os.Open(a.path)
	if err != nil {
		return nil, 0, err
	}
	cr := cpio.NewReader(f)
	for {
		hdr, err := cr.Next()
		if err == io.EOF {
			_ = f.Close()
			return nil, 0, fmt.Errorf("entry %s not found in archive", name)
		}
		if err != nil {
			_ = f.Close()
			return nil, 0, fmt.Errorf("read cpio entry: %w", err)
		}
		if hdr.Name != name {
			continue
		}
		return &tarEntryReadCloser{
			Reader:  cr,
			closeFn: func() { _ = f.Close() },
		}, hdr.Size, nil
	}
}

type rarArchive struct {
	path string
}

func (a *rarArchive) List() ([]archiveEntry, error) {
	files, err := rardecode.List(a.path)
	if err != nil {
		return nil, err
	}
	entries := make([]archiveEntry, 0, len(files))
	for _, f := range files {
		entries = append(entries, archiveEntry{Name: f.Name, Size: f.UnPackedSize})
	}
	return entries, nil
}

func (a *rarArchive) Open(name string) (io.ReadCloser, int64, error) {
	files, err := rardecode.List(a.path)
	if err != nil {
		return nil, 0, err
	}
	for _, f := range files {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, 0, err
		}
		return &zipEntryReadCloser{ReadCloser: rc, closeZip: func() {}}, f.UnPackedSize, nil
	}
	return nil, 0, fmt.Errorf("entry %s not found in archive", name)
}
