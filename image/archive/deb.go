package archive

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// A Debian package (.deb) and OpenWrt package (.ipk) are an ar archive holding
// a "debian-binary" marker, a control.tar.* and a data.tar.* — the file
// payload. debArchive exposes the contents of data.tar.* as a plain archive so
// the same listing/extraction path used for tarballs applies.

type arMember struct {
	name string
	off  int64
	size int64
}

// readArMembers parses an ar archive from f, returning each member's name, the
// offset of its data and its size.
func readArMembers(f *os.File) ([]arMember, error) {
	var magic [8]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return nil, fmt.Errorf("read ar magic: %w", err)
	}
	if string(magic[:]) != "!<arch>\n" {
		return nil, fmt.Errorf("not an ar archive")
	}

	var members []arMember
	pos := int64(8)
	for {
		if _, err := f.Seek(pos, io.SeekStart); err != nil {
			return nil, err
		}
		var hdr [60]byte
		_, err := io.ReadFull(f, hdr[:])
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read ar header: %w", err)
		}

		name := strings.TrimRight(string(hdr[0:16]), " /")
		size, err := strconv.ParseInt(strings.TrimSpace(string(hdr[48:58])), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse ar member size: %w", err)
		}
		members = append(members, arMember{name: name, off: pos + 60, size: size})

		pos += 60 + size
		if size%2 == 1 {
			pos++ // ar pads member data to an even boundary
		}
	}
	return members, nil
}

// compressionFromSuffix maps a data.tar.* member suffix to the decompressor
// name openCompressed understands.
func compressionFromSuffix(name string) string {
	switch {
	case strings.HasSuffix(name, ".gz"):
		return "gzip"
	case strings.HasSuffix(name, ".bz2"):
		return "bzip2"
	case strings.HasSuffix(name, ".xz"):
		return "xz"
	case strings.HasSuffix(name, ".lzma"):
		return "lzma"
	case strings.HasSuffix(name, ".zst"):
		return "zstd"
	case strings.HasSuffix(name, ".lz4"):
		return "lz4"
	case name == "data.tar":
		return ""
	}
	return "gzip" // default for a bare "data.tar" that is gzip-compressed despite the name
}

type debArchive struct {
	path string
}

// openDataTar locates the data.tar.* payload, decompresses it and returns a tar
// reader over its entries.
func (a *debArchive) openDataTar() (*tar.Reader, func(), error) {
	f, err := os.Open(a.path)
	if err != nil {
		return nil, nil, err
	}
	fail := func(err error) (*tar.Reader, func(), error) {
		_ = f.Close()
		return nil, nil, err
	}

	members, err := readArMembers(f)
	if err != nil {
		return fail(err)
	}
	var data *arMember
	for i := range members {
		if members[i].name == "data.tar" || strings.HasPrefix(members[i].name, "data.tar.") {
			data = &members[i]
			break
		}
	}
	if data == nil {
		return fail(fmt.Errorf("no data.tar member found in package"))
	}

	sr := io.NewSectionReader(f, data.off, data.size)
	r, closeDec, err := openCompressed(sr, compressionFromSuffix(data.name))
	if err != nil {
		return fail(fmt.Errorf("open %s: %w", data.name, err))
	}
	return tar.NewReader(r), func() { closeDec(); _ = f.Close() }, nil
}

func (a *debArchive) List() ([]Entry, error) {
	tr, closeFn, err := a.openDataTar()
	if err != nil {
		return nil, err
	}
	defer closeFn()

	var entries []Entry
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return entries, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read data.tar entry: %w", err)
		}
		kind := tarKind(hdr)
		entries = append(entries, Entry{
			Name:     hdr.Name,
			Size:     hdr.Size,
			Kind:     kind,
			Mode:     defaultMode(kind, hdr.Mode&0o7777),
			ModTime:  hdr.ModTime,
			UID:      hdr.Uid,
			GID:      hdr.Gid,
			Uname:    hdr.Uname,
			Gname:    hdr.Gname,
			Linkname: hdr.Linkname,
		})
	}
}

func (a *debArchive) Open(name string) (io.ReadCloser, int64, error) {
	tr, closeFn, err := a.openDataTar()
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
			return nil, 0, fmt.Errorf("read data.tar entry: %w", err)
		}
		if hdr.Name != name {
			continue
		}
		if hdr.Typeflag == tar.TypeSymlink || hdr.Typeflag == tar.TypeLink {
			closeFn()
			return nil, 0, fmt.Errorf("entry %s is not a regular file", name)
		}
		return &tarEntryReadCloser{Reader: tr, closeFn: closeFn}, hdr.Size, nil
	}
}

var _ Archive = (*debArchive)(nil)
