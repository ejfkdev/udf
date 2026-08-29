// Package xpress decompresses the XPRESS (LZ77 + Huffman) compression format
// used by some WIM resources, per [MS-XCA] "LZ77+Huffman". It is a clean-room
// port of wimlib's xpress_decompress.c.
//
// Each compressed chunk begins with 256 bytes encoding 512 four-bit Huffman
// codeword lengths (symbols 0-255 are literals; 256-511 are match headers),
// followed by a 16-bit-little-endian bitstream of Huffman-coded symbols.
package xpress

import "errors"

const (
	numChars       = 256
	numSymbols     = 512
	maxCodewordLen = 15
	minMatchLen    = 3
	lensBytes      = numSymbols / 2 // 256 bytes hold 512 nibbles
)

// ErrCorrupt indicates malformed XPRESS data.
var ErrCorrupt = errors.New("xpress: corrupt data")

// Decompress decompresses a single XPRESS chunk into exactly outSize bytes.
func Decompress(in []byte, outSize int) ([]byte, error) {
	if len(in) < lensBytes {
		return nil, ErrCorrupt
	}
	var lens [numSymbols]byte
	for i := range lensBytes {
		lens[2*i] = in[i] & 0xf
		lens[2*i+1] = in[i] >> 4
	}
	table, ok := buildDecodeTable(lens)
	if !ok {
		return nil, ErrCorrupt
	}

	br := &bitReader{data: in[lensBytes:]}
	out := make([]byte, 0, outSize)

	for len(out) < outSize {
		sym, ok := decodeSym(br, table)
		if !ok {
			return nil, ErrCorrupt
		}
		if sym < numChars {
			out = append(out, byte(sym))
			continue
		}

		length := sym & 0xf
		log2Offset := (sym >> 4) & 0xf

		br.ensure(16)
		offset := (1 << log2Offset) | int(br.pop(uint(log2Offset)))

		if length == 0xf {
			length += int(br.readByte())
			if length == 0xf+0xff {
				length = int(br.readU16())
			}
		}
		length += minMatchLen

		if offset > len(out) || len(out)+length > outSize {
			return nil, ErrCorrupt
		}
		for range length {
			out = append(out, out[len(out)-offset])
		}
	}
	return out, nil
}

// decodeSym reads one Huffman symbol. It returns false on an invalid codeword.
func decodeSym(br *bitReader, table []uint16) (int, bool) {
	br.ensure(maxCodewordLen)
	entry := table[br.peek(maxCodewordLen)]
	length := entry & 0xf
	if length == 0 {
		return 0, false
	}
	br.remove(uint(length))
	return int(entry >> 4), true
}

// buildDecodeTable builds a flat 2^15-entry canonical-Huffman decode table.
// Each entry packs (symbol << 4 | codeword length); unused slots stay zero.
func buildDecodeTable(lens [numSymbols]byte) ([]uint16, bool) {
	const bits = maxCodewordLen
	table := make([]uint16, 1<<bits)
	code := 0
	for length := 1; length <= maxCodewordLen; length++ {
		for sym := range numSymbols {
			if int(lens[sym]) != length {
				continue
			}
			start := code << (bits - length)
			end := start + (1 << (bits - length))
			if end > len(table) {
				return nil, false // over-subscribed code
			}
			entry := uint16(sym<<4) | uint16(length)
			for i := start; i < end; i++ {
				table[i] = entry
			}
			code++
		}
		code <<= 1
	}
	return table, true
}

// bitReader mirrors wimlib's input_bitstream: a 32-bit MSB-justified buffer
// filled from 16-bit little-endian words. readByte/readU16 read raw bytes from
// the stream position past the buffered words (the XPRESS interleaving quirk).
type bitReader struct {
	bitbuf   uint32
	bitsleft uint
	data     []byte
	next     int
}

// ensure makes at least n bits (n <= 16) available, padding with zero bits past
// end of input.
func (b *bitReader) ensure(n uint) {
	if b.bitsleft >= n {
		return
	}
	if len(b.data)-b.next < 2 {
		b.bitsleft = 32 // overflow: treat remaining input as zero bits
		return
	}
	word := uint32(b.data[b.next]) | uint32(b.data[b.next+1])<<8
	b.bitbuf |= word << (16 - b.bitsleft)
	b.next += 2
	b.bitsleft += 16
}

func (b *bitReader) peek(n uint) uint32 {
	if n == 0 {
		return 0
	}
	return b.bitbuf >> (32 - n)
}

func (b *bitReader) remove(n uint) {
	b.bitbuf <<= n
	b.bitsleft -= n
}

func (b *bitReader) pop(n uint) uint32 {
	v := b.peek(n)
	b.remove(n)
	return v
}

func (b *bitReader) readByte() byte {
	if b.next >= len(b.data) {
		return 0
	}
	v := b.data[b.next]
	b.next++
	return v
}

func (b *bitReader) readU16() int {
	if len(b.data)-b.next < 2 {
		return 0
	}
	v := int(b.data[b.next]) | int(b.data[b.next+1])<<8
	b.next += 2
	return v
}
