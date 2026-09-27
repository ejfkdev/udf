package gzipidx

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// gzipHeaderSize parses a gzip member header from r and returns the size of
// the header, leaving r positioned at the deflate stream.
func gzipHeaderSize(r *bufio.Reader) (int64, error) {
	var fixed [10]byte
	if _, err := io.ReadFull(r, fixed[:]); err != nil {
		return 0, err
	}
	if fixed[0] != 0x1f || fixed[1] != 0x8b {
		return 0, fmt.Errorf("not a gzip stream")
	}
	if fixed[2] != 8 {
		return 0, fmt.Errorf("unsupported gzip compression method %d", fixed[2])
	}
	flg := fixed[3]
	size := int64(10)
	if flg&0x04 != 0 { // FEXTRA
		var xlen [2]byte
		if _, err := io.ReadFull(r, xlen[:]); err != nil {
			return 0, err
		}
		n := int64(binary.LittleEndian.Uint16(xlen[:]))
		if _, err := io.CopyN(io.Discard, r, n); err != nil {
			return 0, err
		}
		size += 2 + n
	}
	for _, name := range []byte{0x08, 0x10} { // FNAME, FCOMMENT
		if flg&name == 0 {
			continue
		}
		for {
			b, err := r.ReadByte()
			if err != nil {
				return 0, err
			}
			size++
			if b == 0 {
				break
			}
		}
	}
	if flg&0x02 != 0 { // FHCRC
		if _, err := io.CopyN(io.Discard, r, 2); err != nil {
			return 0, err
		}
		size += 2
	}
	return size, nil
}

// deflateStart opens path and returns the file offset where the deflate
// stream of the first gzip member begins, plus a reader positioned there.
func deflateStart(path string) (*os.File, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	br := bufio.NewReaderSize(f, 1<<16)
	n, err := gzipHeaderSize(br)
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	// bufio may have consumed more than the header; the returned offset is
	// the header size, so callers must re-open or use ReaderAt.
	return f, n, nil
}
