package lzms

import "slices"

// LZMS uses adaptive Huffman codes that both the compressor and decompressor
// rebuild from identical, evolving symbol frequencies. The codeword-length
// computation below mirrors wimlib's make_canonical_huffman_code (a priority-
// queue construction with ties broken toward leaf nodes, length-limited to 15).
// It must match exactly, or the decoded codes diverge.

const (
	numSymbolBits = 10
	symbolMask    = (1 << numSymbolBits) - 1
	freqMask      = ^uint32(symbolMask)
)

// huffDecoder is one adaptive Huffman code: its frequencies, current codeword
// lengths, and a flat decode table indexed by the next 15 bits.
type huffDecoder struct {
	numSyms         int
	rebuildFreq     int
	numUntilRebuild int
	freqs           []uint32
	lens            []byte
	table           []uint16 // index by peek(maxCodewordLen) -> symbol
	scratch         []uint32
}

func newHuffDecoder(numSyms, rebuildFreq int) *huffDecoder {
	h := &huffDecoder{
		numSyms:     numSyms,
		rebuildFreq: rebuildFreq,
		freqs:       make([]uint32, numSyms),
		lens:        make([]byte, numSyms),
		table:       make([]uint16, 1<<maxCodewordLen),
		scratch:     make([]uint32, numSyms),
	}
	for i := range h.freqs {
		h.freqs[i] = 1
	}
	h.buildCode()
	return h
}

func (h *huffDecoder) buildCode() {
	makeCanonicalLens(h.numSyms, h.freqs, h.lens, h.scratch)
	buildDecodeTable(h.table, h.lens)
	h.numUntilRebuild = h.rebuildFreq
}

func (h *huffDecoder) rebuild() {
	h.buildCode()
	for i := range h.freqs {
		h.freqs[i] = (h.freqs[i] >> 1) + 1
	}
}

// decode reads one symbol from the bitstream, updates frequencies, and rebuilds
// the code if the rebuild interval has elapsed.
func (h *huffDecoder) decode(bs *bitstream) int {
	bs.ensureBits(maxCodewordLen)
	sym := int(h.table[bs.peekBits(maxCodewordLen)])
	bs.removeBits(uint(h.lens[sym]))
	h.freqs[sym]++
	h.numUntilRebuild--
	if h.numUntilRebuild == 0 {
		h.rebuild()
	}
	return sym
}

// makeCanonicalLens computes canonical codeword lengths from symbol frequencies.
func makeCanonicalLens(numSyms int, freqs []uint32, lens []byte, a []uint32) {
	// Collect used symbols, packed as (freq<<10 | sym), sorted by frequency
	// then symbol value (ascending). Zero-frequency symbols get length 0.
	numUsed := 0
	for sym := 0; sym < numSyms; sym++ {
		if freqs[sym] == 0 {
			lens[sym] = 0
			continue
		}
		a[numUsed] = uint32(sym) | (freqs[sym] << numSymbolBits)
		numUsed++
	}
	// Packed values are distinct (each holds a unique symbol in its low bits),
	// so a plain ascending sort matches the freq-then-symbol order wimlib uses.
	// slices.Sort avoids sort.Slice's reflection overhead (a decode hot path).
	slices.Sort(a[:numUsed])

	if numUsed == 0 {
		return
	}
	if numUsed == 1 {
		sym := int(a[0] & symbolMask)
		nonzero := sym
		if sym == 0 {
			nonzero = 1
		}
		lens[0] = 1
		lens[nonzero] = 1
		return
	}

	buildTree(a, numUsed)

	var lenCounts [maxCodewordLen + 2]int
	computeLengthCounts(a, numUsed-2, lenCounts[:])
	genLens(a, lens, lenCounts[:], numUsed)
}

// buildTree builds the non-leaf nodes of a Huffman tree in place over the
// frequency-sorted array a (faithful port of wimlib's build_tree).
func buildTree(a []uint32, symCount int) {
	lastIdx := symCount - 1
	i, b, e := 0, 0, 0
	for {
		var newFreq uint32
		switch {
		case i+1 <= lastIdx && (b == e || (a[i+1]&freqMask) <= (a[b]&freqMask)):
			newFreq = (a[i] & freqMask) + (a[i+1] & freqMask)
			i += 2
		case b+2 <= e && (i > lastIdx || (a[b+1]&freqMask) < (a[i]&freqMask)):
			newFreq = (a[b] & freqMask) + (a[b+1] & freqMask)
			a[b] = uint32(e<<numSymbolBits) | (a[b] & symbolMask)
			a[b+1] = uint32(e<<numSymbolBits) | (a[b+1] & symbolMask)
			b += 2
		default:
			newFreq = (a[i] & freqMask) + (a[b] & freqMask)
			a[b] = uint32(e<<numSymbolBits) | (a[b] & symbolMask)
			i++
			b++
		}
		a[e] = newFreq | (a[e] & symbolMask)
		e++
		if e >= lastIdx {
			break
		}
	}
}

// computeLengthCounts determines how many codewords get each length.
func computeLengthCounts(a []uint32, rootIdx int, lenCounts []int) {
	for i := range lenCounts {
		lenCounts[i] = 0
	}
	lenCounts[1] = 2
	a[rootIdx] &= symbolMask // root depth 0
	for node := rootIdx - 1; node >= 0; node-- {
		parent := int(a[node] >> numSymbolBits)
		depth := int(a[parent]>>numSymbolBits) + 1
		a[node] = (a[node] & symbolMask) | (uint32(depth) << numSymbolBits)
		ln := depth
		if ln >= maxCodewordLen {
			ln = maxCodewordLen
			for lenCounts[ln] == 0 {
				ln--
			}
		}
		lenCounts[ln]--
		lenCounts[ln+1] += 2
	}
}

// genLens assigns codeword lengths to symbols (decreasing length to symbols in
// increasing frequency/value order).
func genLens(a []uint32, lens []byte, lenCounts []int, numUsed int) {
	i := 0
	for ln := maxCodewordLen; ln >= 1; ln-- {
		for count := lenCounts[ln]; count > 0; count-- {
			lens[a[i]&symbolMask] = byte(ln)
			i++
		}
	}
	_ = numUsed
}

// buildDecodeTable fills a flat decode table indexed by the next maxCodewordLen
// bits: every bit pattern whose top L bits equal a symbol's canonical codeword
// maps to that symbol.
func buildDecodeTable(table []uint16, lens []byte) {
	var count [maxCodewordLen + 1]int
	for _, l := range lens {
		count[l]++
	}
	var pos [maxCodewordLen + 2]int
	for i := 1; i <= maxCodewordLen; i++ {
		pos[i+1] = pos[i] + count[i]<<(maxCodewordLen-i)
	}
	for sym, l := range lens {
		if l == 0 {
			continue
		}
		start := pos[l]
		next := start + 1<<(maxCodewordLen-l)
		// Fill table[start:next] with sym. Short codewords cover huge runs
		// (a length-1 code fills half the table), so grow the filled region by
		// doubling copies (memmove) instead of a per-element loop.
		table[start] = uint16(sym)
		for filled := 1; start+filled < next; {
			filled += copy(table[start+filled:next], table[start:start+filled])
		}
		pos[l] = next
	}
}
