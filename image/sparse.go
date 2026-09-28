package image

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// An Android sparse image is how `fastboot flash` and the AOSP build tools ship
// filesystem images: instead of the raw bytes, the file holds chunks that
// expand to them, so a mostly-empty system image is small. udf reads one as a
// flat device — the chunk table is read up front and never expanded into
// memory — so the whole disk pipeline (partition table, LVM, filesystem) works
// on it exactly as it does on a raw image.
//
// Layout (libsparse): a 28-byte file header, then chunks of a 12-byte header
// plus data. Chunk kinds: "raw" copies its bytes, "fill" repeats one 32-bit
// word, "don't care" contributes zeros, "crc32" is metadata.
const (
	sparseMagic     = "\x3a\xff\x26\xed"
	sparseFileHdrSz = 28
	sparseChunkHdr  = 12

	sparseChunkRaw    = 0xCAC1
	sparseChunkFill   = 0xCAC2
	sparseChunkDontCa = 0xCAC3
	sparseChunkCRC32  = 0xCAC4

	// sparseMaxChunks bounds the chunk table so a corrupt header cannot make
	// the reader allocate wildly; real images stay far below it.
	sparseMaxChunks = 1 << 22
)

// sparseChunk is one entry of the chunk table, in expanded terms.
type sparseChunk struct {
	out  int64  // offset in the expanded image
	n    int64  // length in the expanded image
	off  int64  // file offset of the chunk's data (raw chunks)
	fill uint32 // the repeated word (fill chunks)
	kind uint16
}

// sparseReader presents a sparse image as a flat read-only device.
type sparseReader struct {
	f      *os.File
	blk    int64
	size   int64
	chunks []sparseChunk
}

// openSparse parses the header and chunk table of a sparse image.
func openSparse(path string) (*sparseReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*sparseReader, error) {
		_ = f.Close()
		return nil, fmt.Errorf("open sparse image %s: %w", path, err)
	}

	hdr := make([]byte, sparseFileHdrSz)
	if _, err := io.ReadFull(f, hdr); err != nil {
		return fail(err)
	}
	if string(hdr[:4]) != sparseMagic {
		return fail(fmt.Errorf("not a sparse image"))
	}
	r := &sparseReader{f: f}
	fileHdrSz := int64(binary.LittleEndian.Uint16(hdr[8:10]))
	chunkHdrSz := int64(binary.LittleEndian.Uint16(hdr[10:12]))
	blkSz := int64(binary.LittleEndian.Uint32(hdr[12:16]))
	totalBlks := int64(binary.LittleEndian.Uint32(hdr[16:20]))
	totalChunks := int64(binary.LittleEndian.Uint32(hdr[20:24]))
	if blkSz <= 0 || totalBlks < 0 || totalChunks < 0 || totalChunks > sparseMaxChunks {
		return fail(fmt.Errorf("implausible sparse header"))
	}
	if fileHdrSz < sparseFileHdrSz || chunkHdrSz < sparseChunkHdr {
		return fail(fmt.Errorf("unsupported sparse header sizes"))
	}
	r.blk = blkSz
	r.size = totalBlks * blkSz

	// The chunk table sits right after the file header.
	off := fileHdrSz
	buf := make([]byte, chunkHdrSz)
	var out int64
	for i := int64(0); i < totalChunks; i++ {
		if _, err := f.ReadAt(buf, off); err != nil {
			return fail(fmt.Errorf("chunk %d header: %w", i, err))
		}
		kind := binary.LittleEndian.Uint16(buf[0:2])
		chunkBlocks := int64(binary.LittleEndian.Uint32(buf[4:8]))
		totalSz := int64(binary.LittleEndian.Uint32(buf[8:12]))
		data := off + chunkHdrSz
		switch kind {
		case sparseChunkRaw:
			n := chunkBlocks * blkSz
			r.chunks = append(r.chunks, sparseChunk{out: out, n: n, off: data, kind: kind})
			out += n
		case sparseChunkFill:
			n := chunkBlocks * blkSz
			var word [4]byte
			if _, err := f.ReadAt(word[:], data); err != nil {
				return fail(fmt.Errorf("chunk %d fill value: %w", i, err))
			}
			r.chunks = append(r.chunks, sparseChunk{out: out, n: n, fill: binary.LittleEndian.Uint32(word[:]), kind: kind})
			out += n
		case sparseChunkDontCa:
			n := chunkBlocks * blkSz
			r.chunks = append(r.chunks, sparseChunk{out: out, n: n, kind: kind})
			out += n
		case sparseChunkCRC32:
			// Metadata: contributes nothing to the expanded image.
		default:
			return fail(fmt.Errorf("unknown sparse chunk type %#x", kind))
		}
		if totalSz < chunkHdrSz {
			return fail(fmt.Errorf("chunk %d has a short size", i))
		}
		off += totalSz
	}
	if out != r.size {
		return fail(fmt.Errorf("chunks expand to %d bytes, header says %d", out, r.size))
	}
	return r, nil
}

func (r *sparseReader) Size() int64 { return r.size }

func (r *sparseReader) Close() error { return r.f.Close() }

// ReadAt serves a range of the expanded image: raw chunks come from the file,
// fill chunks repeat their word, and everything else reads as zeros.
func (r *sparseReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("negative offset")
	}
	if off >= r.size {
		return 0, io.EOF
	}
	n := 0
	for n < len(p) && off < r.size {
		c, ok := r.chunkAt(off)
		if !ok {
			// A gap cannot happen: the table covers the image from zero up.
			break
		}
		into := off - c.out
		want := c.n - into
		if want > int64(len(p)-n) {
			want = int64(len(p) - n)
		}
		if want > r.size-off {
			want = r.size - off
		}
		if want <= 0 {
			break
		}
		switch c.kind {
		case sparseChunkRaw:
			read, err := r.f.ReadAt(p[n:n+int(want)], c.off+into)
			n += read
			off += int64(read)
			if err != nil {
				if err == io.EOF {
					err = io.ErrUnexpectedEOF
				}
				return n, err
			}
		case sparseChunkFill:
			word := c.fill
			for i := int64(0); i < want; i++ {
				pos := into + i
				p[n+int(i)] = byte(word >> (8 * uint(pos%4)))
			}
			n += int(want)
			off += want
		default: // don't care, and the gaps of a crc32 chunk: zeros
			for i := int64(0); i < want; i++ {
				p[n+int(i)] = 0
			}
			n += int(want)
			off += want
		}
	}
	if off >= r.size && n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// chunkAt finds the chunk holding an expanded offset. The table is ordered, so
// a binary search keeps random reads cheap on images with many chunks.
func (r *sparseReader) chunkAt(off int64) (sparseChunk, bool) {
	lo, hi := 0, len(r.chunks)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		c := r.chunks[mid]
		switch {
		case off < c.out:
			hi = mid
		case off >= c.out+c.n:
			lo = mid + 1
		default:
			return c, true
		}
	}
	return sparseChunk{}, false
}
