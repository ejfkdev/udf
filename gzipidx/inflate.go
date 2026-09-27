// Package gzipidx builds a random-access index for a gzip stream: restart
// checkpoints at deflate block boundaries plus the entry directory of the tar
// inside, so later commands decompress only the region they need.
//
// Deflate cannot be restarted anywhere — a block's Huffman tables are defined
// at the block's start — so checkpoints are recorded exactly at block
// boundaries while scanning the stream once with the block-aware scanner in
// this package. Reading then restarts the stock (fast) decompressor at a
// checkpoint, using a bit-shifted view of the compressed bytes plus the saved
// 32 KiB window, and verifies the result against the recorder context.
package gzipidx

import (
	"errors"
	"fmt"
	"io"
)

// maxWindow is the deflate back-reference reach.
const maxWindow = 32768

var errCorrupt = errors.New("corrupt deflate stream")

var slowOnly = false // debug switch

// sink receives decompressed bytes in order, tagged with the decompressed
// offset of the first byte.
type sink func(outPos int64, p []byte) error

// BlockCallback is called at every deflate block start with the compressed
// bit position and the decompressed offset the block starts at. window
// returns a copy of up to maxWindow bytes preceding that offset (only called
// when the caller wants to keep a restart point). Returning false stops the
// scan.
type BlockCallback func(bitPos, outPos int64, window func() []byte) bool

// scanner decodes a raw deflate stream while reporting block boundaries and
// keeping a moving window of recent output.
type scanner struct {
	r      io.Reader
	in     []byte
	inN    int
	inPos  int
	bitBuf uint64
	bitCnt uint

	totalBits int64 // bits consumed from the compressed stream

	out     []byte // recent output, holding at least maxWindow bytes of history
	outBase int64  // decompressed offset of out[0]
	flushed int64  // offset up to which the sink has seen data

	onBlock BlockCallback
	toSink  sink

	lit, dist huffmanTable
	fixedInit bool
}

func newScanner(r io.Reader, onBlock BlockCallback, toSink sink) *scanner {
	return &scanner{
		r:       r,
		in:      make([]byte, 1<<20),
		out:     make([]byte, 0, 1<<20),
		onBlock: onBlock,
		toSink:  toSink,
	}
}

// --- bit input ---

func (s *scanner) refill() error {
	if s.inPos < s.inN {
		return nil
	}
	s.inPos, s.inN = 0, 0
	n, err := s.r.Read(s.in)
	s.inN = n
	if n == 0 {
		if err == nil {
			err = io.EOF
		}
		return err
	}
	return nil
}

// fillBits ensures at least n bits (n <= 57) are available in the bit buffer.
func (s *scanner) fillBits(n uint) error {
	for s.bitCnt < n {
		if err := s.refill(); err != nil {
			return fmt.Errorf("%w: %v", errCorrupt, err)
		}
		s.bitBuf |= uint64(s.in[s.inPos]) << s.bitCnt
		s.inPos++
		s.bitCnt += 8
		s.totalBits += 0 // bits counted when consumed
	}
	return nil
}

// fillBitsBest fills the bit buffer with up to n bits, tolerating the end of
// the stream: peeking may read past the last byte, and the padding bits are
// never used by a well-formed stream.
func (s *scanner) fillBitsBest(n uint) uint {
	for s.bitCnt < n {
		if err := s.refill(); err != nil {
			break
		}
		s.bitBuf |= uint64(s.in[s.inPos]) << s.bitCnt
		s.inPos++
		s.bitCnt += 8
	}
	return s.bitCnt
}

func (s *scanner) peekBits(n uint) (uint32, error) {
	if err := s.fillBits(n); err != nil {
		return 0, err
	}
	return uint32(s.bitBuf & ((1 << n) - 1)), nil
}

func (s *scanner) readBits(n uint) (uint32, error) {
	v, err := s.peekBits(n)
	if err != nil {
		return 0, err
	}
	s.dropBits(n)
	return v, nil
}

func (s *scanner) dropBits(n uint) {
	s.bitBuf >>= n
	s.bitCnt -= n
	s.totalBits += int64(n)
}

// alignToByte drops bits up to the next byte boundary.
func (s *scanner) alignToByte() {
	if rem := s.totalBits % 8; rem != 0 {
		s.dropBits(uint(8 - rem))
	}
}

// BitPos reports the current compressed bit position.
func (s *scanner) BitPos() int64 { return s.totalBits }

// bytesAvailableInBuffer reports how many compressed bytes are buffered
// (used to bound checkpoint searches by callers).
func (s *scanner) bytesAvailableInBuffer() int { return s.inN - s.inPos }

// --- output ---

// appendOut appends decompressed bytes, flushing to the sink when the buffer
// grows past the flush threshold while keeping maxWindow bytes of history.
func (s *scanner) appendOut(p []byte) error {
	s.out = append(s.out, p...)
	return s.maybeFlush()
}

func (s *scanner) maybeFlush() error {
	const flushAt = 1 << 20
	if len(s.out) < flushAt {
		return nil
	}
	keep := maxWindow
	if keep > len(s.out) {
		keep = len(s.out)
	}
	cut := len(s.out) - keep
	if s.toSink != nil && cut > 0 {
		if err := s.toSink(s.outBase, s.out[:cut]); err != nil {
			return err
		}
	}
	// Slide the retained history to the front.
	copy(s.out, s.out[cut:])
	s.out = s.out[:keep]
	s.outBase += int64(cut)
	s.flushed = s.outBase
	return nil
}

// emitByte appends one decompressed byte.
func (s *scanner) emitByte(b byte) error {
	s.out = append(s.out, b)
	return s.maybeFlush()
}

// outOff returns the current decompressed offset (total output produced).
func (s *scanner) outOff() int64 { return s.outBase + int64(len(s.out)) }

// WindowBefore returns a copy of up to maxWindow bytes preceding off.
func (s *scanner) WindowBefore(off int64) []byte {
	start := off - maxWindow
	if start < s.outBase {
		start = s.outBase
	}
	if off > s.outOff() {
		off = s.outOff()
	}
	if start >= off {
		return nil
	}
	w := make([]byte, off-start)
	copy(w, s.out[start-s.outBase:off-s.outBase])
	return w
}

// --- huffman tables ---

type huffmanTable struct {
	// fast holds entries for codes up to fastBits long; symbol is packed with
	// its code length so the slow path can resume from the canonical tables.
	fast     []tableEntry
	counts   [16]int32
	symbols  []uint16
	complete bool
}

type tableEntry struct {
	sym   uint16
	bits  uint8 // code length, 0 when the slot has no short code
	valid bool
}

const fastBits = 9

func (h *huffmanTable) build(lengths []uint8) error {
	for i := range h.counts {
		h.counts[i] = 0
	}
	for _, l := range lengths {
		if l > 15 {
			return errCorrupt
		}
		h.counts[l]++
	}
	h.counts[0] = 0

	// Deflate tolerates incomplete codes (a single distance code, for
	// instance); only oversubscribed codes are invalid.
	left := int32(1)
	for l := 1; l <= 15; l++ {
		left <<= 1
		left -= h.counts[l]
		if left < 0 {
			return errCorrupt
		}
	}
	h.complete = left == 0

	offs := [16]int32{}
	var sum int32
	for l := 1; l <= 15; l++ {
		offs[l] = sum
		sum += h.counts[l]
	}
	h.symbols = make([]uint16, sum)
	var next [16]int32
	copy(next[:], offs[:])
	for sym, l := range lengths {
		if l == 0 {
			continue
		}
		h.symbols[next[l]] = uint16(sym)
		next[l]++
	}

	// Fast lookup for short codes.
	if h.fast == nil {
		h.fast = make([]tableEntry, 1<<fastBits)
	} else {
		for i := range h.fast {
			h.fast[i] = tableEntry{}
		}
	}
	code := int32(0)
	for l := 1; l <= 15; l++ {
		n := h.counts[l]
		if l <= fastBits {
			for i := int32(0); i < n; i++ {
				sym := h.symbols[offs[l]+i]
				rev := reverseBits(uint32(code+i), uint(l))
				fill := 1 << (fastBits - l)
				for j := 0; j < fill; j++ {
					h.fast[j<<l|int(rev)] = tableEntry{sym: sym, bits: uint8(l), valid: true}
				}
			}
		}
		code = (code + n) << 1
	}
	return nil
}

func reverseBits(v uint32, n uint) uint32 {
	var r uint32
	for i := uint(0); i < n; i++ {
		r |= ((v >> i) & 1) << (n - 1 - i)
	}
	return r
}

// decode reads one symbol.
func (s *scanner) decode(h *huffmanTable) (uint16, error) {
	// Peek at the next fastBits bits. Filling is best-effort so the last
	// symbols of a stream (which may leave fewer bits than the table width)
	// still decode; the zero padding is never part of a well-formed symbol.
	if !slowOnly && s.fillBitsBest(fastBits) >= fastBits {
		e := h.fast[s.bitBuf&(1<<fastBits-1)]
		if e.valid {
			s.dropBits(uint(e.bits))
			return e.sym, nil
		}
	}
	// Slow path: codes longer than fastBits. Bits arrive least-significant
	// first, matching the bit-reversed codes the fast table is built from.
	code := int32(0)
	first := int32(0)
	index := int32(0)
	for l := 1; l <= 15; l++ {
		b, err := s.readBits(1)
		if err != nil {
			return 0, err
		}
		code |= int32(b)
		count := h.counts[l]
		if code-first < count {
			return h.symbols[index+(code-first)], nil
		}
		index += count
		first += count
		first <<= 1
		code <<= 1
	}
	return 0, errCorrupt
}

// --- block decoding ---

var (
	lengthBase  = [29]int32{3, 4, 5, 6, 7, 8, 9, 10, 11, 13, 15, 17, 19, 23, 27, 31, 35, 43, 51, 59, 67, 83, 99, 115, 131, 163, 195, 227, 258}
	lengthExtra = [29]uint{0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 1, 1, 2, 2, 2, 2, 3, 3, 3, 3, 4, 4, 4, 4, 5, 5, 5, 5, 0}
	distBase    = [30]int32{1, 2, 3, 4, 5, 7, 9, 13, 17, 25, 33, 49, 65, 97, 129, 193, 257, 385, 513, 769, 1025, 1537, 2049, 3073, 4097, 6145, 8193, 12289, 16385, 24577}
	distExtra   = [30]uint{0, 0, 0, 0, 1, 1, 2, 2, 3, 3, 4, 4, 5, 5, 6, 6, 7, 7, 8, 8, 9, 9, 10, 10, 11, 11, 12, 12, 13, 13}
)

var codeLengthOrder = [19]uint8{16, 17, 18, 0, 8, 7, 9, 6, 10, 5, 11, 4, 12, 3, 13, 2, 14, 1, 15}

// ScanBlockBoundaries decodes the deflate stream in r, invoking onBlock at
// every block start, and forwards decompressed bytes to toSink (which may be
// nil). It returns the total decompressed size.
func ScanBlockBoundaries(r io.Reader, onBlock BlockCallback, toSink sink) (int64, error) {
	s := newScanner(r, onBlock, toSink)
	if err := s.scan(); err != nil {
		return 0, err
	}
	if s.toSink != nil && s.flushed < s.outOff() {
		if err := s.toSink(s.flushed, s.out[s.flushed-s.outBase:]); err != nil {
			return 0, err
		}
		s.flushed = s.outOff()
	}
	return s.outOff(), nil
}

func (s *scanner) scan() error {
	for {
		bitStart := s.totalBits
		outStart := s.outOff()
		if s.onBlock != nil && !s.onBlock(bitStart, outStart, func() []byte { return s.WindowBefore(outStart) }) {
			return nil
		}
		final, err := s.readBits(1)
		if err != nil {
			return err
		}
		btype, err := s.readBits(2)
		if err != nil {
			return err
		}
		switch btype {
		case 0:
			if err := s.storedBlock(); err != nil {
				return err
			}
		case 1:
			if !s.fixedInit {
				if err := s.buildFixed(); err != nil {
					return err
				}
				s.fixedInit = true
			}
			if err := s.huffmanBlock(); err != nil {
				return err
			}
		case 2:
			if err := s.dynamicBlock(); err != nil {
				return err
			}
			if err := s.huffmanBlock(); err != nil {
				return err
			}
		default:
			return errCorrupt
		}
		if final == 1 {
			return nil
		}
	}
}

func (s *scanner) storedBlock() error {
	s.alignToByte()
	length, err := s.readBits(16)
	if err != nil {
		return err
	}
	nlength, err := s.readBits(16)
	if err != nil {
		return err
	}
	if uint16(length) != ^uint16(nlength) {
		return errCorrupt
	}
	for i := uint32(0); i < length; i++ {
		b, err := s.readBits(8)
		if err != nil {
			return err
		}
		if err := s.emitByte(byte(b)); err != nil {
			return err
		}
	}
	return nil
}

func (s *scanner) buildFixed() error {
	lit := make([]uint8, 288)
	for i := 0; i <= 143; i++ {
		lit[i] = 8
	}
	for i := 144; i <= 255; i++ {
		lit[i] = 9
	}
	for i := 256; i <= 279; i++ {
		lit[i] = 7
	}
	for i := 280; i <= 287; i++ {
		lit[i] = 8
	}
	if err := s.lit.build(lit); err != nil {
		return err
	}
	dist := make([]uint8, 30)
	for i := range dist {
		dist[i] = 5
	}
	return s.dist.build(dist)
}

func (s *scanner) dynamicBlock() error {
	hlit, err := s.readBits(5)
	if err != nil {
		return err
	}
	hdist, err := s.readBits(5)
	if err != nil {
		return err
	}
	hclen, err := s.readBits(4)
	if err != nil {
		return err
	}
	nlit := int(hlit) + 257
	ndist := int(hdist) + 1
	ncode := int(hclen) + 4

	clLengths := make([]uint8, 19)
	for i := 0; i < ncode; i++ {
		v, err := s.readBits(3)
		if err != nil {
			return err
		}
		clLengths[codeLengthOrder[i]] = uint8(v)
	}
	var cl huffmanTable
	if err := cl.build(clLengths); err != nil {
		return err
	}

	lengths := make([]uint8, 0, nlit+ndist)
	for len(lengths) < nlit+ndist {
		sym, err := s.decode(&cl)
		if err != nil {
			return err
		}
		switch {
		case sym < 16:
			lengths = append(lengths, uint8(sym))
		case sym == 16:
			if len(lengths) == 0 {
				return errCorrupt
			}
			rep, err := s.readBits(2)
			if err != nil {
				return err
			}
			last := lengths[len(lengths)-1]
			for i := 0; i < int(rep)+3; i++ {
				lengths = append(lengths, last)
			}
		case sym == 17:
			rep, err := s.readBits(3)
			if err != nil {
				return err
			}
			for i := 0; i < int(rep)+3; i++ {
				lengths = append(lengths, 0)
			}
		case sym == 18:
			rep, err := s.readBits(7)
			if err != nil {
				return err
			}
			for i := 0; i < int(rep)+11; i++ {
				lengths = append(lengths, 0)
			}
		}
		if len(lengths) > nlit+ndist {
			return errCorrupt
		}
	}
	if err := s.lit.build(lengths[:nlit]); err != nil {
		return err
	}
	return s.dist.build(lengths[nlit:])
}

func (s *scanner) huffmanBlock() error {
	for {
		sym, err := s.decode(&s.lit)
		if err != nil {
			return err
		}
		switch {
		case sym < 256:
			if err := s.emitByte(byte(sym)); err != nil {
				return err
			}
		case sym == 256:
			return nil
		default:
			idx := int(sym) - 257
			if idx >= len(lengthBase) {
				return errCorrupt
			}
			length := lengthBase[idx]
			if ne := lengthExtra[idx]; ne > 0 {
				extra, err := s.readBits(ne)
				if err != nil {
					return err
				}
				length += int32(extra)
			}
			dsym, err := s.decode(&s.dist)
			if err != nil {
				return err
			}
			if int(dsym) >= len(distBase) {
				return errCorrupt
			}
			dist := distBase[dsym]
			if de := distExtra[dsym]; de > 0 {
				extra, err := s.readBits(de)
				if err != nil {
					return err
				}
				dist += int32(extra)
			}
			if err := s.copyMatch(int(dist), int(length)); err != nil {
				return err
			}
		}
	}
}

func (s *scanner) copyMatch(dist, length int) error {
	if dist <= 0 || int64(dist) > s.outOff() {
		return errCorrupt
	}
	if dist > maxWindow {
		return errCorrupt
	}
	// Grow the buffer first (preserving history) so the copy has room, then
	// extend the length to cover it.
	if cap(s.out)-len(s.out) < length {
		grown := make([]byte, len(s.out), len(s.out)+length+maxWindow)
		copy(grown, s.out)
		s.out = grown
	}
	start := len(s.out)
	s.out = s.out[:start+length]
	src := start - dist
	if src < 0 {
		return errCorrupt
	}
	for i := 0; i < length; i++ {
		s.out[start+i] = s.out[src+i]
	}
	return s.maybeFlush()
}
