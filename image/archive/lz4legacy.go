package archive

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/pierrec/lz4/v4"
)

// Android images — the emulator's ramdisk.img/initrd, and ramdisks inside many
// devices' boot.img — are compressed with LZ4's *legacy* frame format
// (magic 0x184C2102), which the modern frame decoder rejects: it predates the
// frame descriptor and carries no sizes, just a sequence of blocks. A block is
// a 4-byte length — the length of the compressed block that follows, not
// counting the length field itself — and a raw LZ4 block, which the writer
// chose to expand to at most 8 MiB (verified against lz4(1) on real ramdisks:
// one block, 2,215,656 bytes out of 1,914,123 in).
const (
	legacyLZ4Magic  = "\x02\x21\x4c\x18" // 0x184C2102, little endian
	legacyLZ4MaxBlk = 8 << 20            // what the legacy writer used per block
	legacyLZ4MaxIn  = 16 << 20           // sanity bound on a compressed block
	legacyLZ4MaxOut = 64 << 20           // ceiling when a block needs more room
)

// The emulator appends a bootconfig blob after the initrd's LZ4 stream (visible
// on a real AVD initrd: the stream ends and plain "androidboot.*=..." text with
// a "#BOOTCONFIG" trailer follows), so a block header that cannot be a block
// ends the stream instead of failing. Truncation inside a block is still an
// error: only an implausible *header* is treated as the start of trailing data.
//
// isLegacyLZ4 reports whether b starts a legacy LZ4 stream.
func isLegacyLZ4(b []byte) bool { return len(b) >= 4 && string(b[:4]) == legacyLZ4Magic }

// legacyLZ4Reader decodes a legacy LZ4 stream.
type legacyLZ4Reader struct {
	src      io.Reader
	block    []byte // decoded bytes of the current block (empty until decoded)
	pos      int    // how much of block has been handed out
	done     bool
	hdr      [4]byte
	scratch  []byte // compressed block bytes
	dst      []byte // decode buffer, grown as needed
	produced int    // bytes decoded so far
}

func newLegacyLZ4Reader(r io.Reader) *legacyLZ4Reader {
	return &legacyLZ4Reader{src: r}
}

func (l *legacyLZ4Reader) Read(p []byte) (int, error) {
	for l.pos >= len(l.block) {
		if l.done {
			return 0, io.EOF
		}
		if err := l.next(); err != nil {
			return 0, err
		}
	}
	n := copy(p, l.block[l.pos:])
	l.pos += n
	return n, nil
}

// next decodes one block into l.block.
func (l *legacyLZ4Reader) next() error {
	if _, err := io.ReadFull(l.src, l.hdr[:]); err != nil {
		if err == io.EOF {
			l.done = true
			return io.EOF
		}
		return fmt.Errorf("read lz4 block header: %w", err)
	}
	size := int(binary.LittleEndian.Uint32(l.hdr[:]))
	if size <= 0 || size > legacyLZ4MaxIn {
		if l.produced > 0 {
			// Trailing data (a bootconfig blob, padding): the stream is over.
			l.done = true
			return io.EOF
		}
		return fmt.Errorf("lz4 block size %d out of range", size)
	}
	if cap(l.scratch) < size {
		l.scratch = make([]byte, size)
	}
	comp := l.scratch[:size]
	if _, err := io.ReadFull(l.src, comp); err != nil {
		return fmt.Errorf("read lz4 block: %w", err)
	}
	if l.dst == nil {
		l.dst = make([]byte, legacyLZ4MaxBlk)
	}
	n, err := lz4.UncompressBlock(comp, l.dst)
	if err != nil {
		// The writer's block size is a convention, not a field: give the
		// decoder more room rather than failing on a larger block.
		for len(l.dst) < legacyLZ4MaxOut {
			l.dst = make([]byte, len(l.dst)*2)
			if n, err = lz4.UncompressBlock(comp, l.dst); err == nil {
				break
			}
		}
		if err != nil {
			return fmt.Errorf("decode lz4 block: %w", err)
		}
	}
	if n == 0 {
		return fmt.Errorf("empty lz4 block")
	}
	l.block = l.dst[:n]
	l.pos = 0
	l.produced += n
	return nil
}

// legacyLZ4Stream concatenates the decoded blocks of a legacy stream.
// Decompressing eagerly keeps the reader simple, and these streams are small
// (ramdisks, not archives), so the whole thing fits in memory.
type legacyLZ4Stream struct {
	data   []byte
	offset int
}

func openLegacyLZ4(r io.Reader) (*legacyLZ4Stream, error) {
	lr := newLegacyLZ4Reader(r)
	out, err := io.ReadAll(lr)
	if err != nil {
		return nil, err
	}
	return &legacyLZ4Stream{data: out}, nil
}

func (s *legacyLZ4Stream) Read(p []byte) (int, error) {
	if s.offset >= len(s.data) {
		return 0, io.EOF
	}
	n := copy(p, s.data[s.offset:])
	s.offset += n
	return n, nil
}
