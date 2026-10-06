package gzipidx

import (
	"bytes"
	"compress/flate"
	"io"
	"testing"
)

// writeDeflateStream hand-builds a deflate stream whose block types exercise
// the table switching: a dynamic block, then an empty fixed block, then
// another fixed block. A scanner that builds the fixed tables once and reuses
// them after a dynamic block decodes the fixed blocks with the dynamic block's
// lengths — which is what made a 26 GB upgrade bundle look corrupt right after
// its first dynamic→fixed transition.
func writeDeflateStream(t *testing.T) []byte {
	t.Helper()

	type bits struct {
		buf  []byte
		bit  uint
		cur  byte
		full bool
	}
	var w bits
	put := func(v uint32, n uint) {
		for i := uint(0); i < n; i++ {
			if v&(1<<i) != 0 {
				w.cur |= 1 << w.bit
			}
			w.bit++
			if w.bit == 8 {
				w.buf = append(w.buf, w.cur)
				w.cur, w.bit = 0, 0
			}
		}
	}
	// A canonical code of the given lengths, emitted bit-reversed as the
	// format stores them.
	codeOf := func(lengths []uint8, sym int) (uint32, uint) {
		var counts [16]int32
		for _, l := range lengths {
			counts[l]++
		}
		code := uint32(0)
		for l := uint(1); l <= 15; l++ {
			for s, sl := range lengths {
				if uint(sl) != l {
					continue
				}
				if s == sym {
					rev := uint32(0)
					for i := uint(0); i < l; i++ {
						rev |= ((code >> i) & 1) << (l - 1 - i)
					}
					return rev, l
				}
				code++
			}
			code <<= 1
		}
		t.Fatalf("symbol %d has no code", sym)
		return 0, 0
	}

	const nlit, ndist = 257, 1
	litLengths := make([]uint8, nlit)
	litLengths['A'] = 1 // one literal
	litLengths[256] = 1 // end of block
	distLengths := make([]uint8, ndist)
	distLengths[0] = 1

	// The header's code-length code: a complete code over the three symbols
	// used here — 17 and 18 (zero runs) and 1 (a code length of one).
	clLengths := make([]uint8, 19)
	clLengths[17] = 1
	clLengths[18] = 2
	clLengths[1] = 2
	clOrdered := []int{16, 17, 18, 0, 8, 7, 9, 6, 10, 5, 11, 4, 12, 3, 13, 2, 14, 1, 15}

	// dynamic block: everything on the wire is the literal table's lengths,
	// written as code-length symbols with plain 2-bit codes.
	put(0, 1) // BFINAL = 0
	put(2, 2) // BTYPE = dynamic
	put(nlit-257, 5)
	put(ndist-1, 5)
	put(15, 4) // HCLEN: 19 code-length codes
	for i := 0; i < 19; i++ {
		put(uint32(clLengths[clOrdered[i]]), 3)
	}
	emitCl := func(sym int, extra uint32, extraBits uint) {
		rev, n := codeOf(clLengths, sym)
		put(rev, n)
		if extraBits > 0 {
			put(extra, extraBits)
		}
	}
	// The lengths array, symbol by symbol: 65 zeroes, 'A' at 65, then zeroes
	// up to the end-of-block at 256, then the single distance code.
	for i := 0; i < 6; i++ {
		emitCl(17, 7, 3) // ten zeroes each
	}
	emitCl(17, 2, 3) // five more, 65 in total
	emitCl(1, 0, 0)  // literal 'A' = length 1
	emitCl(18, 127, 7)
	emitCl(18, 41, 7) // 190 zeroes up to symbol 256
	emitCl(1, 0, 0)   // end-of-block = length 1
	emitCl(1, 0, 0)   // the one distance code = length 1
	// Payload: two literals 'A'.
	rev, n := codeOf(litLengths, 'A')
	put(rev, n)
	put(rev, n)
	rev, n = codeOf(litLengths, 256)
	put(rev, n)

	// empty fixed block, then a fixed block with one literal, then final empty
	put(0, 1)
	put(1, 2)
	fixed := make([]uint8, 288)
	for i := 0; i < 144; i++ {
		fixed[i] = 8
	}
	for i := 144; i < 256; i++ {
		fixed[i] = 9
	}
	for i := 256; i < 280; i++ {
		fixed[i] = 7
	}
	for i := 280; i < 288; i++ {
		fixed[i] = 8
	}
	rev, n = codeOf(fixed, 256)
	put(rev, n) // end of the empty block

	put(0, 1) // BFINAL = 0, fixed block with a literal
	put(1, 2)
	rev, n = codeOf(fixed, 'B')
	put(rev, n)
	rev, n = codeOf(fixed, 256)
	put(rev, n)

	put(1, 1) // BFINAL = 1, empty final fixed block
	put(1, 2)
	rev, n = codeOf(fixed, 256)
	put(rev, n)

	if w.bit > 0 {
		w.buf = append(w.buf, w.cur)
	}
	return w.buf
}

func TestScannerSwitchesBetweenDynamicAndFixedTables(t *testing.T) {
	stream := writeDeflateStream(t)

	// The reference decodes it: whatever the blocks say, the bytes are "AAB".
	ref, err := io.ReadAll(flate.NewReader(bytes.NewReader(stream)))
	if err != nil {
		t.Fatalf("the hand-built stream is not valid deflate: %v", err)
	}
	if string(ref) != "AAB" {
		t.Fatalf("reference output = %q, want \"AAB\"", ref)
	}

	var got []byte
	n, err := ScanMapped(stream, nil, func(_ int64, p []byte) error {
		got = append(got, p...)
		return nil
	})
	if err != nil {
		t.Fatalf("scanner: %v", err)
	}
	if int64(len(got)) != n {
		t.Fatalf("scanner reported %d bytes but produced %d", n, len(got))
	}
	if !bytes.Equal(got, ref) {
		t.Fatalf("scanner output = %q, want %q", got, ref)
	}
}
