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
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

// maxWindow is the deflate back-reference reach.
const maxWindow = 32768

// tableKind says which tables the scanner's literal/distance tables hold.
type tableKind uint8

const (
	tablesNone tableKind = iota
	tablesFixed
	tablesDynamic
)

// flushThreshold is how much output the scanner buffers before handing it to
// the sink and sliding the retained history forward. Buffers are allocated
// with room for a flush threshold plus maxWindow of history, which lets the
// symbol loop guarantee capacity in one comparison per iteration.
const flushThreshold = 1 << 20

// matchMax is the longest match a deflate stream can encode, so the symbol
// loop never writes past a capacity check made for matchMax bytes.
const matchMax = 258

// groupBits is the share of the bit buffer the symbol loop keeps filled: one
// iteration can consume at most a 15-bit length code, 5 length extra bits, a
// 15-bit distance code and 13 distance extra bits.
const groupBits = 48

// fastBits is the width of the direct-lookup table in front of the canonical
// code tables.
const fastBits = 10

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
	clTable   huffmanTable
	// tableKind records which code tables the literal/distance tables
	// currently hold. A dynamic block replaces them, so a fixed block that
	// follows one must be rebuilt — building the fixed tables once and reusing
	// them decodes a fixed block with the previous dynamic block's lengths.
	tableKind tableKind

	// reusable scratch buffers for header parsing
	clLengths []uint8
	lengths   []uint8
}

func newScanner(r io.Reader, onBlock BlockCallback, toSink sink) *scanner {
	return &scanner{
		r:         r,
		in:        make([]byte, 1<<20),
		clLengths: make([]uint8, 19),
		lengths:   make([]uint8, 0, 320),
		out:       make([]byte, 0, flushThreshold+maxWindow+512),
		onBlock:   onBlock,
		toSink:    toSink,
	}
}

// --- bit input ---

// refill reads the next block of compressed bytes. A nil r means the whole
// deflate stream is already in s.in (mapped or loaded by ScanMapped), so
// running out of it is the end of the stream.
func (s *scanner) refill() error {
	if s.inPos < s.inN {
		return nil
	}
	if s.r == nil {
		return io.EOF
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
// Whole words are pulled in one go when the buffer has room, which matters for
// the per-block header parsing on large streams.
func (s *scanner) fillBits(n uint) error {
	for s.bitCnt < n {
		if s.bitCnt <= 32 && s.inPos+4 <= s.inN {
			s.bitBuf |= uint64(binary.LittleEndian.Uint32(s.in[s.inPos:])) << s.bitCnt
			s.inPos += 4
			s.bitCnt += 32
			continue
		}
		if err := s.refill(); err != nil {
			return fmt.Errorf("%w: %v", errCorrupt, err)
		}
		s.bitBuf |= uint64(s.in[s.inPos]) << s.bitCnt
		s.inPos++
		s.bitCnt += 8
	}
	return nil
}

// fillBitsBest fills the bit buffer with up to n bits, tolerating the end of
// the stream: peeking may read past the last byte, and the padding bits are
// never used by a well-formed stream.
func (s *scanner) fillBitsBest(n uint) uint {
	for s.bitCnt < n {
		if s.bitCnt <= 32 && s.inPos+4 <= s.inN {
			s.bitBuf |= uint64(binary.LittleEndian.Uint32(s.in[s.inPos:])) << s.bitCnt
			s.inPos += 4
			s.bitCnt += 32
			continue
		}
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
	if len(s.out) >= flushThreshold {
		return s.flushOut()
	}
	return nil
}

// flushOut hands the part of the buffer that is older than the retained
// history to the sink and slides the history to the front. It is the cold half
// of maybeFlush, kept out of line so the symbol loop's capacity check inlines.
func (s *scanner) flushOut() error {
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

func (s *scanner) maybeFlush() error {
	if len(s.out) < flushThreshold {
		return nil
	}
	return s.flushOut()
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
	// An entry is sym | len<<16, zero when the slot holds no short code, which
	// keeps the table a plain []uint32 that clear() can wipe in one memclr.
	fast     []uint32
	counts   [16]int32
	symbols  []uint16
	complete bool
}

// packEntry builds a fast-table entry: the symbol plus its code length.
func packEntry(sym uint16, bits uint) uint32 {
	return uint32(sym) | uint32(bits)<<16
}

func (h *huffmanTable) build(lengths []uint8) error {
	for i := range h.counts {
		h.counts[i] = 0
	}
	for _, l := range lengths {
		if l > 15 {
			return fmt.Errorf("%w: code length %d > 15", errCorrupt, l)
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
			return fmt.Errorf("%w: oversubscribed code at length %d", errCorrupt, l)
		}
	}
	h.complete = left == 0

	offs := [16]int32{}
	var sum int32
	for l := 1; l <= 15; l++ {
		offs[l] = sum
		sum += h.counts[l]
	}
	// The symbols slice is reused across blocks: a layer with a hundred
	// thousand blocks would otherwise allocate one per block.
	if cap(h.symbols) < int(sum) {
		h.symbols = make([]uint16, sum)
	} else {
		h.symbols = h.symbols[:sum]
	}
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
		h.fast = make([]uint32, 1<<fastBits)
	} else {
		clear(h.fast)
	}
	code := int32(0)
	for l := 1; l <= 15; l++ {
		n := h.counts[l]
		if l <= fastBits {
			for i := int32(0); i < n; i++ {
				sym := h.symbols[offs[l]+i]
				rev := reverseBits(uint32(code+i), uint(l))
				fill := 1 << (fastBits - l)
				e := packEntry(sym, uint(l))
				for j := 0; j < fill; j++ {
					h.fast[j<<l|int(rev)] = e
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

// decode reads one symbol. It is the conservative path (used for block
// headers and the end of a stream), where the bit buffer may hold fewer bits
// than a code needs.
func (s *scanner) decode(h *huffmanTable) (uint16, error) {
	// Peek at the next fastBits bits. Filling is best-effort so the last
	// symbols of a stream (which may leave fewer bits than the table width)
	// still decode; the zero padding is never part of a well-formed symbol.
	if !slowOnly && s.fillBitsBest(fastBits) >= fastBits {
		if e := h.fast[s.bitBuf&(1<<fastBits-1)]; e != 0 {
			s.dropBits(uint(e >> 16))
			return uint16(e), nil
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
	return 0, fmt.Errorf("%w: no canonical code matches the lookahead", errCorrupt)
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
	return s.finish()
}

// ScanMapped is ScanBlockBoundaries for a deflate stream that is already in
// memory (typically a read-only mapping of the file). The scanner decodes the
// bytes in place, which saves a copy of the whole compressed stream.
func ScanMapped(data []byte, onBlock BlockCallback, toSink sink) (int64, error) {
	s := newScanner(nil, onBlock, toSink)
	s.in, s.inPos, s.inN = data, 0, len(data)
	return s.finish()
}

// scanDeflate scans the deflate stream of the gzip member that starts at off
// in path, mapping the file when the platform allows it and falling back to
// buffered reads otherwise.
func scanDeflate(path string, off, end int64, onBlock BlockCallback, toSink sink) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	size := end - off
	if mapFileAvailable && size > 0 {
		if data, unmap, err := mapFile(f, off, size); err == nil {
			defer unmap()
			return ScanMapped(data, onBlock, toSink)
		}
	}
	return ScanBlockBoundaries(io.NewSectionReader(f, off, size), onBlock, toSink)
}

func (s *scanner) finish() (int64, error) {
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
			return s.at(bitStart, outStart, err)
		}
		btype, err := s.readBits(2)
		if err != nil {
			return s.at(bitStart, outStart, err)
		}
		switch btype {
		case 0:
			if err := s.storedBlock(); err != nil {
				return s.at(bitStart, outStart, err)
			}
		case 1:
			if s.tableKind != tablesFixed {
				if err := s.buildFixed(); err != nil {
					return s.at(bitStart, outStart, err)
				}
				s.tableKind = tablesFixed
			}
			if err := s.huffmanBlock(); err != nil {
				return s.at(bitStart, outStart, err)
			}
		case 2:
			if err := s.dynamicBlock(); err != nil {
				return s.at(bitStart, outStart, err)
			}
			s.tableKind = tablesDynamic
			if err := s.huffmanBlock(); err != nil {
				return s.at(bitStart, outStart, err)
			}
		default:
			return s.at(bitStart, outStart, fmt.Errorf("block type %d", btype))
		}
		if final == 1 {
			return nil
		}
	}
}

// at adds where a failure happened to the error: a bit position and an output
// offset locate the block, and the block type says which tables were in play.
func (s *scanner) at(bitStart, outStart int64, err error) error {
	return fmt.Errorf("at bit %d (output offset %d): %w", bitStart, outStart, err)
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
	remaining := int(length)
	// Whole bytes may already sit in the bit buffer; drain those first.
	for remaining > 0 && s.bitCnt >= 8 {
		b := byte(s.bitBuf)
		s.bitBuf >>= 8
		s.bitCnt -= 8
		s.totalBits += 8
		if err := s.emitByte(b); err != nil {
			return err
		}
		remaining--
	}
	// Then copy straight out of the input buffer in bulk.
	for remaining > 0 {
		if s.inPos >= s.inN {
			if err := s.refill(); err != nil {
				return fmt.Errorf("%w: %v", errCorrupt, err)
			}
		}
		n := s.inN - s.inPos
		if n > remaining {
			n = remaining
		}
		if err := s.appendOut(s.in[s.inPos : s.inPos+n]); err != nil {
			return err
		}
		s.inPos += n
		s.totalBits += int64(8 * n)
		remaining -= n
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

// dynamicBlock parses a dynamic block header and builds its tables. The
// header appears once per block, so on streams with hundreds of thousands of
// blocks its field reads and code-length decoding add up; the loop below keeps
// the bit state in locals like the symbol loop does.
func (s *scanner) dynamicBlock() error {
	bb, bc := s.bitBuf, s.bitCnt
	in, inPos, inN := s.in, s.inPos, s.inN
	tb := int64(0)

	sync := func() {
		s.totalBits += tb
		tb = 0
		s.bitBuf, s.bitCnt, s.inPos, s.inN = bb, bc, inPos, inN
	}
	reload := func() {
		bb, bc, in, inPos, inN = s.bitBuf, s.bitCnt, s.in, s.inPos, s.inN
	}
	// refill makes sure at least need bits are buffered (need <= 32).
	refill := func(need uint) error {
		for bc < need {
			if inPos >= inN {
				sync()
				if err := s.refill(); err != nil {
					reload()
					return errCorrupt
				}
				reload()
				continue
			}
			bb |= uint64(in[inPos]) << bc
			inPos++
			bc += 8
		}
		return nil
	}
	readBits := func(n uint) (uint32, error) {
		if err := refill(n); err != nil {
			return 0, err
		}
		v := uint32(bb & (1<<n - 1))
		bb >>= n
		bc -= n
		tb += int64(n)
		return v, nil
	}
	// decodeSym decodes one symbol from h, taking the fast table when it can.
	decodeSym := func(h *huffmanTable) (uint16, error) {
		if err := refill(fastBits); err != nil {
			return 0, err
		}
		if e := h.fast[bb&(1<<fastBits-1)]; e != 0 {
			nb := uint(e >> 16)
			bb >>= nb
			bc -= nb
			tb += int64(nb)
			return uint16(e), nil
		}
		sync()
		sym, err := s.decode(h)
		reload()
		return sym, err
	}

	hlit, err := readBits(5)
	if err != nil {
		sync()
		return err
	}
	hdist, err := readBits(5)
	if err != nil {
		sync()
		return err
	}
	hclen, err := readBits(4)
	if err != nil {
		sync()
		return err
	}
	nlit := int(hlit) + 257
	ndist := int(hdist) + 1
	ncode := int(hclen) + 4

	s.clLengths = s.clLengths[:19]
	for i := range s.clLengths {
		s.clLengths[i] = 0
	}
	for i := 0; i < ncode; i++ {
		v, err := readBits(3)
		if err != nil {
			sync()
			return err
		}
		s.clLengths[codeLengthOrder[i]] = uint8(v)
	}
	if err := s.clTable.build(s.clLengths); err != nil {
		sync()
		return err
	}

	if cap(s.lengths) < nlit+ndist {
		s.lengths = make([]uint8, 0, nlit+ndist)
	}
	lengths := s.lengths[:0]
	for len(lengths) < nlit+ndist {
		sym, err := decodeSym(&s.clTable)
		if err != nil {
			sync()
			return err
		}
		switch {
		case sym < 16:
			lengths = append(lengths, uint8(sym))
		case sym == 16:
			if len(lengths) == 0 {
				sync()
				return fmt.Errorf("%w: code-length repeat with nothing to repeat", errCorrupt)
			}
			rep, err := readBits(2)
			if err != nil {
				sync()
				return err
			}
			last := lengths[len(lengths)-1]
			for i := 0; i < int(rep)+3; i++ {
				lengths = append(lengths, last)
			}
		case sym == 17:
			rep, err := readBits(3)
			if err != nil {
				sync()
				return err
			}
			for i := 0; i < int(rep)+3; i++ {
				lengths = append(lengths, 0)
			}
		case sym == 18:
			rep, err := readBits(7)
			if err != nil {
				sync()
				return err
			}
			for i := 0; i < int(rep)+11; i++ {
				lengths = append(lengths, 0)
			}
		}
		if len(lengths) > nlit+ndist {
			sync()
			return fmt.Errorf("%w: code lengths overrun the table (%d > %d)", errCorrupt, len(lengths), nlit+ndist)
		}
	}
	sync()
	s.lengths = lengths
	if err := s.lit.build(lengths[:nlit]); err != nil {
		return err
	}
	return s.dist.build(lengths[nlit:])
}

// huffmanBlock decodes the symbols of one huffman-coded block. It is the hot
// loop of the whole scanner — on tar layers it runs once per deflate symbol,
// hundreds of millions of times — so it keeps the bit buffer, the input
// position and the output length in locals, refills the bit buffer in whole
// words once per symbol group instead of per code, decodes codes longer than
// the fast table straight out of the bit register, and writes literals and
// matches into the output buffer directly after a single capacity check.
//
// Invariant: an iteration starts with at least groupBits bits buffered and
// with at least matchMax bytes of output capacity, so no check inside the
// group is needed.
func (s *scanner) huffmanBlock() error {
	bb, bc := s.bitBuf, s.bitCnt
	in, inPos, inN := s.in, s.inPos, s.inN
	out := s.out
	lit, dst := &s.lit, &s.dist

	// Bit accounting without per-symbol counters: the bits consumed since the
	// block started are exactly the bits loaded into the register minus the
	// ones still buffered, so a bias computed once here turns the total into
	// one multiply and subtract per block instead of one add per symbol.
	bias := s.totalBits + int64(bc)
	loaded := 0 // bytes pulled into the bit register in this block

	sync := func() {
		s.totalBits = bias + int64(loaded)*8 - int64(bc)
		s.bitBuf, s.bitCnt, s.inPos, s.inN = bb, bc, inPos, inN
		s.out = out
	}
	reload := func() {
		bb, bc, in, inPos, inN = s.bitBuf, s.bitCnt, s.in, s.inPos, s.inN
		out = s.out
	}

	for {
		// Refill in a word-sized load, taking as many whole bytes as the
		// register has room for. The mask drops the bytes of the word that
		// were not taken, keeping the invariant the rest of the loop relies
		// on: bits at and above bitCnt are zero.
		for bc < groupBits {
			if inPos+8 <= inN {
				bb |= binary.LittleEndian.Uint64(in[inPos:]) << bc
				k := (63 - bc) >> 3
				inPos += int(k)
				loaded += int(k)
				bc += k * 8
				bb &= 1<<bc - 1
				continue
			}
			if inPos < inN {
				bb |= uint64(in[inPos]) << bc
				inPos++
				loaded++
				bc += 8
				continue
			}
			// The buffer is drained: read on. At the end of the stream the
			// remaining symbols are finished conservatively, since they may
			// need fewer bits than groupBits.
			sync()
			if err := s.refill(); err != nil {
				return s.huffmanBlockTail()
			}
			reload()
		}

		// A literal symbol, or the end of the block.
		sym := uint16(0)
		if e := lit.fast[bb&(1<<fastBits-1)]; e != 0 {
			sym = uint16(e)
			bb >>= uint(e >> 16)
			bc -= uint(e >> 16)
		} else {
			got, nb, ok := decodeCanonical(lit, bb)
			if !ok {
				sync()
				return fmt.Errorf("%w: no literal/length code for %#x", errCorrupt, bb&(1<<fastBits-1))
			}
			sym = got
			bb >>= nb
			bc -= nb
		}
		if sym < 256 {
			if len(out) >= flushThreshold {
				s.out = out
				if err := s.flushOut(); err != nil {
					sync()
					return err
				}
				out = s.out
			}
			out = append(out, byte(sym))
			continue
		}
		if sym == 256 {
			sync()
			return nil
		}

		// A match: length code, length extra bits, distance code, distance
		// extra bits. The group refill above guarantees all of them fit.
		idx := int(sym) - 257
		if idx >= len(lengthBase) {
			sync()
			return fmt.Errorf("%w: length symbol %d out of range", errCorrupt, sym)
		}
		length := int32(lengthBase[idx])
		if ne := lengthExtra[idx]; ne > 0 {
			length += int32(bb & (1<<ne - 1))
			bb >>= ne
			bc -= ne
		}

		dsym := uint16(0)
		if e := dst.fast[bb&(1<<fastBits-1)]; e != 0 {
			dsym = uint16(e)
			bb >>= uint(e >> 16)
			bc -= uint(e >> 16)
		} else {
			got, nb, ok := decodeCanonical(dst, bb)
			if !ok {
				sync()
				return fmt.Errorf("%w: no distance code for %#x", errCorrupt, bb&(1<<fastBits-1))
			}
			dsym = got
			bb >>= nb
			bc -= nb
		}
		if int(dsym) >= len(distBase) {
			sync()
			return fmt.Errorf("%w: distance symbol %d out of range", errCorrupt, dsym)
		}
		distance := int32(distBase[dsym])
		if de := distExtra[dsym]; de > 0 {
			distance += int32(bb & (1<<de - 1))
			bb >>= de
			bc -= de
		}

		if len(out) >= flushThreshold {
			s.out = out
			if err := s.flushOut(); err != nil {
				sync()
				return err
			}
			out = s.out
		}
		start := len(out)
		if int(distance) > start || int(distance) > maxWindow {
			sync()
			return fmt.Errorf("%w: distance %d with %d bytes of history", errCorrupt, distance, start)
		}
		out = out[:start+int(length)]
		src := start - int(distance)
		switch {
		case distance >= 8 && length <= 8:
			// Most matches are short: one 8-byte load and store beats a
			// memmove call. Slicing past the match is fine (it stays inside
			// the allocated buffer) and the extra bytes are overwritten by
			// the next write; src+8 <= start because distance >= 8.
			binary.LittleEndian.PutUint64(out[start:start+8], binary.LittleEndian.Uint64(out[src:src+8]))
		case distance >= 16 && length <= 16:
			binary.LittleEndian.PutUint64(out[start:start+8], binary.LittleEndian.Uint64(out[src:src+8]))
			binary.LittleEndian.PutUint64(out[start+8:start+16], binary.LittleEndian.Uint64(out[src+8:src+16]))
		case distance >= length:
			copy(out[start:], out[src:src+int(length)])
		default:
			// Overlapping match: copy in chunks that double the available
			// run, each a memmove the runtime can vectorize.
			for i := 0; i < int(length); {
				n := int(length) - i
				if n > int(distance) {
					n = int(distance)
				}
				copy(out[start+i:start+i+n], out[src+i:src+i+n])
				i += n
			}
		}
	}
}

// huffmanBlockTail finishes a huffman block whose input ran out mid-block.
// It runs at most once per stream, for the last handful of symbols, and uses
// the conservative helpers that tolerate peeking past the final byte.
func (s *scanner) huffmanBlockTail() error {
	for {
		sym, err := s.decode(&s.lit)
		if err != nil {
			return err
		}
		if sym < 256 {
			if err := s.emitByte(byte(sym)); err != nil {
				return err
			}
			continue
		}
		if sym == 256 {
			return nil
		}
		idx := int(sym) - 257
		if idx >= len(lengthBase) {
			return errCorrupt
		}
		length := int(lengthBase[idx])
		if ne := lengthExtra[idx]; ne > 0 {
			v, err := s.readBits(ne)
			if err != nil {
				return err
			}
			length += int(v)
		}
		dsym, err := s.decode(&s.dist)
		if err != nil {
			return err
		}
		if int(dsym) >= len(distBase) {
			return errCorrupt
		}
		distance := int(distBase[dsym])
		if de := distExtra[dsym]; de > 0 {
			v, err := s.readBits(de)
			if err != nil {
				return err
			}
			distance += int(v)
		}
		if err := s.copyMatch(distance, length); err != nil {
			return err
		}
	}
}

// decodeCanonical decodes one symbol longer than the fast table covers,
// walking the canonical code tables directly off the bit register. The caller
// guarantees at least groupBits valid bits, so no refill is needed.
func decodeCanonical(h *huffmanTable, bits uint64) (sym uint16, nbits uint, ok bool) {
	code := int32(0)
	first := int32(0)
	index := int32(0)
	for l := uint(1); l <= 15; l++ {
		code |= int32(bits & 1)
		bits >>= 1
		count := h.counts[l]
		if code-first < count {
			return h.symbols[index+(code-first)], l, true
		}
		index += count
		first += count
		first <<= 1
		code <<= 1
	}
	return 0, 0, false
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
	dst := s.out[start:]
	if dist >= length {
		copy(dst[:length], s.out[src:src+length])
	} else {
		// Overlapping match: copy in chunks that double the available run,
		// each a memmove the runtime can vectorize.
		for i := 0; i < length; {
			n := length - i
			if n > dist {
				n = dist
			}
			copy(dst[i:i+n], s.out[src+i:src+i+n])
			i += n
		}
	}
	return s.maybeFlush()
}
