package wim

import (
	"bytes"
	"encoding/binary"
	"io"

	"github.com/ejfkdev/udf/wim/lzms"
	"github.com/ejfkdev/udf/wim/lzx"
	"github.com/ejfkdev/udf/wim/xpress"
)

// chunkSize is the seek-reset threshold used by the directory-metadata reader
// in wim.go. Compressed-resource chunking itself uses the WIM header's
// CompressionSize, which differs by codec (32768 for XPRESS/LZX, larger for
// LZMS).
const chunkSize = 32768

type compressedReader struct {
	r            *io.SectionReader
	d            io.ReadCloser
	chunks       []int64
	curChunk     int
	originalSize int64
	chunkSz      int64
	compress     hdrFlag
}

func newCompressedReader(r *io.SectionReader, originalSize int64, offset int64, compress hdrFlag, chunkSz int64) (*compressedReader, error) {
	nchunks := (originalSize + chunkSz - 1) / chunkSz
	var base int64
	chunks := make([]int64, nchunks)
	if originalSize <= 0xffffffff {
		// 32-bit chunk offsets
		base = (nchunks - 1) * 4
		chunks32 := make([]uint32, nchunks-1)
		err := binary.Read(r, binary.LittleEndian, chunks32)
		if err != nil {
			return nil, err
		}
		for i, n := range chunks32 {
			chunks[i+1] = int64(n)
		}
	} else {
		// 64-bit chunk offsets
		base = (nchunks - 1) * 8
		err := binary.Read(r, binary.LittleEndian, chunks[1:])
		if err != nil {
			return nil, err
		}
	}

	for i, c := range chunks {
		chunks[i] = c + base
	}

	cr := &compressedReader{
		r:            r,
		chunks:       chunks,
		originalSize: originalSize,
		chunkSz:      chunkSz,
		compress:     compress,
	}

	err := cr.reset(int(offset / chunkSz))
	if err != nil {
		return nil, err
	}

	suboff := offset % chunkSz
	if suboff != 0 {
		_, err := io.CopyN(io.Discard, cr.d, suboff)
		if err != nil {
			return nil, err
		}
	}
	return cr, nil
}

func (r *compressedReader) chunkOffset(n int) int64 {
	if n == len(r.chunks) {
		return r.r.Size()
	}
	return r.chunks[n]
}

func (r *compressedReader) compressedChunkSize(n int) int {
	return int(r.chunkOffset(n+1) - r.chunkOffset(n))
}

func (r *compressedReader) uncompressedSize(n int) int {
	if n < len(r.chunks)-1 {
		return int(r.chunkSz)
	}
	size := int(r.originalSize % r.chunkSz)
	if size == 0 {
		size = int(r.chunkSz)
	}
	return size
}

func (r *compressedReader) reset(n int) error {
	if n >= len(r.chunks) {
		return io.EOF
	}
	if r.d != nil {
		r.d.Close()
	}
	r.curChunk = n
	size := r.compressedChunkSize(n)
	uncompressedSize := r.uncompressedSize(n)
	section := io.NewSectionReader(r.r, r.chunkOffset(n), int64(size))
	if size != uncompressedSize {
		switch r.compress {
		case hdrFlagCompressXpress, hdrFlagCompressLzms:
			// XPRESS and LZMS decode a whole chunk into a buffer.
			comp := make([]byte, size)
			if _, err := io.ReadFull(section, comp); err != nil {
				return err
			}
			var out []byte
			var err error
			if r.compress == hdrFlagCompressXpress {
				out, err = xpress.Decompress(comp, uncompressedSize)
			} else {
				out, err = lzms.Decompress(comp, uncompressedSize)
			}
			if err != nil {
				return err
			}
			r.d = io.NopCloser(bytes.NewReader(out))
		default: // hdrFlagCompressLzx (and uncompressed-resource fallback)
			d, err := lzx.NewReader(section, uncompressedSize)
			if err != nil {
				return err
			}
			r.d = d
		}
	} else {
		r.d = io.NopCloser(section)
	}

	return nil
}

func (r *compressedReader) Read(b []byte) (int, error) {
	for {
		n, err := r.d.Read(b)
		if err != io.EOF { //nolint:errorlint
			return n, err
		}

		err = r.reset(r.curChunk + 1)
		if err != nil {
			return n, err
		}
	}
}

func (r *compressedReader) Close() error {
	var err error
	if r.d != nil {
		err = r.d.Close()
		r.d = nil
	}
	return err
}
