package gzipidx

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"

	kflate "github.com/klauspost/compress/flate"
	kgzip "github.com/klauspost/compress/gzip"
)

// bitShiftReader presents the underlying bytes shifted right by k bits, so a
// deflate decoder consuming bytes LSB-first starts at bit k of the first byte.
// A restart point inside the stream therefore needs a bit-shifted view rather
// than a bit-level decoder.
type bitShiftReader struct {
	r   io.Reader
	k   uint8
	in  []byte
	off int
	n   int
	err error
}

func newBitShiftReader(r io.Reader, k uint8) *bitShiftReader {
	return &bitShiftReader{r: r, k: k, in: make([]byte, 1<<16)}
}

func (b *bitShiftReader) refill() int {
	if b.off < b.n {
		return b.n - b.off
	}
	b.off, b.n = 0, 0
	if b.err != nil {
		return 0
	}
	n, err := b.r.Read(b.in)
	b.n, b.err = n, err
	if n == 0 && err == nil {
		b.err = io.EOF
	}
	return b.n
}

func (b *bitShiftReader) Read(p []byte) (int, error) {
	if b.k == 0 {
		if b.refill() == 0 {
			return 0, b.err
		}
		got := copy(p, b.in[b.off:b.n])
		b.off += got
		return got, nil
	}
	n := 0
	for n < len(p) {
		if b.refill() == 0 {
			if n == 0 {
				return 0, b.err
			}
			return n, nil
		}
		cur := b.in[b.off]
		b.off++
		var next byte
		if b.refill() > 0 {
			next = b.in[b.off]
		}
		p[n] = (cur >> b.k) | (next << (8 - b.k))
		n++
	}
	return n, nil
}

// newRestartReader decodes the deflate stream of path starting at the given
// bit position (relative to the start of the deflate stream), with window as
// the initial history.
func newRestartReader(path string, deflateStart, bitPos int64, window []byte) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	byteOff := deflateStart + bitPos/8
	bitOff := uint8(bitPos % 8)
	if byteOff < 0 || byteOff >= st.Size() {
		_ = f.Close()
		return nil, errCorrupt
	}
	sr := io.NewSectionReader(f, byteOff, st.Size()-byteOff)
	// The dictionary slice is handed to the decoder as its history buffer, so
	// it must not be reused by the caller while reading.
	dict := make([]byte, len(window))
	copy(dict, window)
	r := kflate.NewReaderDict(newBitShiftReader(sr, bitOff), dict)
	return &restartReadCloser{r: r, f: f, inner: r}, nil
}

type restartReadCloser struct {
	r     io.Reader
	f     *os.File
	inner io.Closer
}

func (c *restartReadCloser) Read(p []byte) (int, error) { return c.r.Read(p) }

func (c *restartReadCloser) Close() error {
	var err error
	if c.inner != nil {
		err = c.inner.Close()
	}
	if c.f != nil {
		_ = c.f.Close()
	}
	return err
}

// Reader provides random access into an indexed gzip file: it restarts the
// decompressor at the checkpoint before a requested offset and decodes only
// as far as needed.
type Reader struct {
	path string
	ix   *Index
	f    *os.File
}

// OpenReader opens the indexed file for random access.
func OpenReader(path string, ix *Index) (*Reader, error) {
	if !ix.Usable() {
		return nil, fmt.Errorf("index cannot serve random access")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return &Reader{path: path, ix: ix, f: f}, nil
}

func (r *Reader) Close() error { return r.f.Close() }

// Index returns the index backing this reader.
func (r *Reader) Index() *Index { return r.ix }

// ReadAt returns a reader over the decompressed stream starting at outPos.
// The caller must close it before the next call.
func (r *Reader) ReadAt(outPos int64) (io.ReadCloser, error) {
	if outPos < 0 || outPos > r.ix.TotalOut {
		return nil, fmt.Errorf("offset %d out of range", outPos)
	}
	cp := r.ix.CheckpointFor(outPos)
	if cp == nil {
		gz, err := openGzipFrom(r.f, 0)
		if err != nil {
			return nil, err
		}
		if outPos > 0 {
			if _, err := io.CopyN(io.Discard, gz, outPos); err != nil {
				_ = gz.Close()
				return nil, err
			}
		}
		return gz, nil
	}

	src, err := newRestartReader(r.path, r.ix.DeflateStart, cp.BitPos, cp.Window)
	if err != nil {
		return nil, err
	}
	// Verify before serving: a checkpoint that does not reproduce its context
	// means the file changed or the index is stale.
	lead := outPos - cp.OutPos
	if lead < int64(len(cp.Ctx)) {
		got := make([]byte, len(cp.Ctx))
		if _, err := io.ReadFull(src, got); err != nil {
			_ = src.Close()
			return nil, err
		}
		if !bytes.Equal(got, cp.Ctx) {
			_ = src.Close()
			return nil, fmt.Errorf("checkpoint at %d failed verification", cp.OutPos)
		}
		// The requested offset sits inside the verified context: serve the
		// rest of it, then continue with the decoder.
		return &restartReadCloser{
			r:     io.MultiReader(bytes.NewReader(got[lead:]), src),
			f:     nil,
			inner: src,
		}, nil
	}
	skip := lead
	if _, err := io.CopyN(io.Discard, src, skip); err != nil {
		_ = src.Close()
		return nil, err
	}
	return src, nil
}

// ReadRange reads exactly n bytes at outPos.
func (r *Reader) ReadRange(outPos, n int64) ([]byte, error) {
	rc, err := r.ReadAt(outPos)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	buf := make([]byte, n)
	if _, err := io.ReadFull(rc, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// openGzipFrom opens the file as a plain gzip stream from the given offset.
func openGzipFrom(f *os.File, off int64) (io.ReadCloser, error) {
	sr := io.NewSectionReader(f, off, 1<<62)
	return kgzip.NewReader(bufio.NewReaderSize(sr, 1<<20))
}
