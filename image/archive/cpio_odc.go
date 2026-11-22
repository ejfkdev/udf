package archive

import (
	"fmt"
	"io"
	"strconv"
	"time"
)

// Old-style ("odc", portable ASCII) cpio support: the format macOS pkgbuild
// writes into .pkg Payload members (magic "070707"). The archive/reader the
// repo uses elsewhere (cavaliergopher/cpio) covers only the SVR4 "newc" magic
// (070701), which initramfs and RPM payloads use.
//
// odc layout: a 76-byte header of fixed-width octal ASCII fields, the file
// name right after (no padding anywhere), then the data:
//
//	magic(6) dev(6) ino(6) mode(6) uid(6) gid(6) nlink(6) rdev(6)
//	mtime(11) namesize(6) filesize(11)
//
// The stream ends at a header whose name is "TRAILER!!!".

const odcMagic = "070707"

type odcHeader struct {
	Name  string
	Mode  uint32
	UID   uint32
	GID   uint32
	Size  int64
	MTime time.Time
}

type odcReader struct {
	r         io.Reader
	hdr       *odcHeader
	remaining int64
}

func newODCReader(r io.Reader) *odcReader {
	return &odcReader{r: r}
}

// Next advances to the next entry. io.EOF marks the end of the archive.
func (o *odcReader) Next() (*odcHeader, error) {
	// Drain the previous entry's body before reading the next header.
	if o.hdr != nil && o.remaining > 0 {
		if _, err := io.CopyN(io.Discard, o.r, o.remaining); err != nil {
			return nil, err
		}
		o.remaining = 0
	}
	var head [76]byte
	if _, err := io.ReadFull(o.r, head[:]); err != nil {
		return nil, err
	}
	if string(head[0:6]) != odcMagic {
		return nil, fmt.Errorf("cpio: invalid odc header magic %q", head[0:6])
	}
	field := func(b []byte) (int64, error) { return strconv.ParseInt(string(b), 8, 64) }
	namesize, err := field(head[59:65])
	if err != nil || namesize <= 0 || namesize > 1<<16 {
		return nil, fmt.Errorf("cpio: bad odc namesize")
	}
	size, err := field(head[65:76])
	if err != nil || size < 0 {
		return nil, fmt.Errorf("cpio: bad odc filesize")
	}
	name := make([]byte, namesize)
	if _, err := io.ReadFull(o.r, name); err != nil {
		return nil, err
	}
	name = name[:len(name)-1] // the length includes the terminating NUL
	if string(name) == "TRAILER!!!" {
		return nil, io.EOF
	}
	mode, _ := field(head[18:24])
	uid, _ := field(head[24:30])
	gid, _ := field(head[30:36])
	mtime, _ := field(head[48:59])
	o.hdr = &odcHeader{
		Name:  string(name),
		Mode:  uint32(mode),
		UID:   uint32(uid),
		GID:   uint32(gid),
		Size:  size,
		MTime: time.Unix(mtime, 0).UTC(),
	}
	o.remaining = size
	return o.hdr, nil
}

// Read consumes the current entry's body.
func (o *odcReader) Read(p []byte) (int, error) {
	if o.hdr == nil || o.remaining <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > o.remaining {
		p = p[:o.remaining]
	}
	n, err := o.r.Read(p)
	o.remaining -= int64(n)
	return n, err
}
