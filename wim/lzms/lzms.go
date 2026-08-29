package lzms

import (
	"encoding/binary"
	"errors"
)

// ErrInvalid is returned when the LZMS data is malformed.
var ErrInvalid = errors.New("lzms: invalid compressed data")

const (
	numLZReps    = 3
	numDeltaReps = 3

	numMainProbs     = 16
	numMatchProbs    = 32
	numLZProbs       = 64
	numLZRepProbs    = 64
	numDeltaProbs    = 64
	numDeltaRepProbs = 64

	probBits        = 6
	probDenominator = 1 << probBits
	initialProb     = 48
	initialRecent   = 0x0000000055555555
)

// probEntry is an adaptive binary probability: the count of zero bits among the
// most recent probDenominator bits coded with it, and those bits themselves.
type probEntry struct {
	numRecentZeroBits uint32
	recentBits        uint64
}

func (e *probEntry) probability() uint32 {
	prob := e.numRecentZeroBits
	prob += (prob - 1) >> 31 // if prob == 0 -> 1 (unsigned shift)
	prob -= prob >> probBits // if prob == denominator -> denominator-1
	return prob
}

func (e *probEntry) update(bit uint32) {
	deltaZero := int32(e.recentBits>>(probDenominator-1)) - int32(bit)
	e.numRecentZeroBits = uint32(int32(e.numRecentZeroBits) + deltaZero)
	e.recentBits = (e.recentBits << 1) | uint64(bit)
}

func newProbs(n int) []probEntry {
	p := make([]probEntry, n)
	for i := range p {
		p[i] = probEntry{numRecentZeroBits: initialProb, recentBits: initialRecent}
	}
	return p
}

// rangeDecoder reads binary range-coded bits from the front of the input.
type rangeDecoder struct {
	rng, code uint32
	in        []byte
	next, end int
}

func newRangeDecoder(in []byte) *rangeDecoder {
	return &rangeDecoder{
		rng:  0xffffffff,
		code: uint32(le16(in[0:]))<<16 | uint32(le16(in[2:])),
		in:   in,
		next: 4,
		end:  len(in),
	}
}

func (rd *rangeDecoder) decodeBit(state *uint32, numStates uint32, probs []probEntry) int {
	pe := &probs[*state]
	*state = (*state << 1) & (numStates - 1)
	prob := pe.probability()

	if rd.rng&0xFFFF0000 == 0 {
		rd.rng <<= 16
		rd.code <<= 16
		if rd.next != rd.end {
			rd.code |= uint32(le16(rd.in[rd.next:]))
			rd.next += 2
		}
	}

	bound := (rd.rng >> probBits) * prob
	if rd.code < bound {
		rd.rng = bound
		pe.update(0)
		return 0
	}
	rd.rng -= bound
	rd.code -= bound
	pe.update(1)
	*state |= 1
	return 1
}

// bitstream reads Huffman codewords and verbatim bits from the back of the
// input, 16-bit little-endian units at a time, high bit first.
type bitstream struct {
	bitbuf      uint64
	bitsleft    uint
	in          []byte
	next, begin int
}

func newBitstream(in []byte) *bitstream {
	return &bitstream{in: in, next: len(in)}
}

func (b *bitstream) ensureBits(n uint) {
	if b.bitsleft >= n {
		return
	}
	avail := uint(64) - b.bitsleft
	if b.next != b.begin {
		b.next -= 2
		b.bitbuf |= uint64(le16(b.in[b.next:])) << (avail - 16)
	}
	if b.next != b.begin {
		b.next -= 2
		b.bitbuf |= uint64(le16(b.in[b.next:])) << (avail - 32)
	}
	b.bitsleft += 32
}

func (b *bitstream) peekBits(n uint) uint64 {
	return (b.bitbuf >> 1) >> (63 - n)
}

func (b *bitstream) removeBits(n uint) {
	b.bitbuf <<= n
	b.bitsleft -= n
}

func (b *bitstream) readBits(n uint) uint32 {
	if n == 0 {
		return 0
	}
	b.ensureBits(n)
	v := b.peekBits(n)
	b.removeBits(n)
	return uint32(v)
}

func le16(b []byte) uint16 { return binary.LittleEndian.Uint16(b) }

// decoder holds the five adaptive Huffman codes and the probability tables.
type decoder struct {
	literal     *huffDecoder
	lzOffset    *huffDecoder
	length      *huffDecoder
	deltaOffset *huffDecoder
	deltaPower  *huffDecoder

	main     []probEntry
	match    []probEntry
	lz       []probEntry
	delta    []probEntry
	lzRep    [numLZReps - 1][]probEntry
	deltaRep [numDeltaReps - 1][]probEntry
}

func (d *decoder) decodeLZOffset(bs *bitstream) uint32 {
	slot := d.lzOffset.decode(bs)
	return offsetSlotBase[slot] + bs.readBits(uint(extraOffsetBits[slot]))
}

func (d *decoder) decodeDeltaOffset(bs *bitstream) uint32 {
	slot := d.deltaOffset.decode(bs)
	return offsetSlotBase[slot] + bs.readBits(uint(extraOffsetBits[slot]))
}

func (d *decoder) decodeLength(bs *bitstream) uint32 {
	slot := d.length.decode(bs)
	length := lengthSlotBase[slot]
	if nb := extraLengthBits[slot]; nb != 0 {
		length += bs.readBits(uint(nb))
	}
	return length
}

// Decompress decompresses a single raw LZMS block whose original size is
// outSize bytes.
func Decompress(in []byte, outSize int) ([]byte, error) {
	if len(in)&1 != 0 || len(in) < 4 {
		return nil, ErrInvalid
	}
	out := make([]byte, outSize)

	rd := newRangeDecoder(in)
	bs := newBitstream(in)

	numOffsetSlots := getNumOffsetSlots(outSize)
	d := &decoder{
		literal:     newHuffDecoder(numLiteralSyms, literalRebuildFreq),
		lzOffset:    newHuffDecoder(numOffsetSlots, lzOffsetRebuildFreq),
		length:      newHuffDecoder(numLengthSyms, lengthRebuildFreq),
		deltaOffset: newHuffDecoder(numOffsetSlots, deltaOffsetRebuildFreq),
		deltaPower:  newHuffDecoder(numDeltaPowerSyms, deltaPowerRebuildFreq),
		main:        newProbs(numMainProbs),
		match:       newProbs(numMatchProbs),
		lz:          newProbs(numLZProbs),
		delta:       newProbs(numDeltaProbs),
	}
	for i := range d.lzRep {
		d.lzRep[i] = newProbs(numLZRepProbs)
	}
	for i := range d.deltaRep {
		d.deltaRep[i] = newProbs(numDeltaRepProbs)
	}

	// LRU queues (with an overflow slot) for match sources.
	var recentLZ [numLZReps + 1]uint32
	var recentDelta [numDeltaReps + 1]uint64
	for i := range recentLZ {
		recentLZ[i] = uint32(i + 1)
	}
	for i := range recentDelta {
		recentDelta[i] = uint64(i + 1)
	}

	// prevItemType: 0 = literal, 1 = LZ match, 2 = delta match (for the delayed
	// LRU-queue update quirk).
	prevItemType := 0

	var mainState, matchState, lzState, deltaState uint32
	var lzRepStates [numLZReps - 1]uint32
	var deltaRepStates [numDeltaReps - 1]uint32

	pos := 0
	for pos < outSize {
		if rd.decodeBit(&mainState, numMainProbs, d.main) == 0 {
			out[pos] = byte(d.literal.decode(bs))
			pos++
			prevItemType = 0
			continue
		}

		if rd.decodeBit(&matchState, numMatchProbs, d.match) == 0 {
			// LZ match.
			var offset uint32
			if rd.decodeBit(&lzState, numLZProbs, d.lz) == 0 {
				offset = d.decodeLZOffset(bs)
				recentLZ[3] = recentLZ[2]
				recentLZ[2] = recentLZ[1]
				recentLZ[1] = recentLZ[0]
			} else {
				adj := prevItemType & 1
				switch {
				case rd.decodeBit(&lzRepStates[0], numLZRepProbs, d.lzRep[0]) == 0:
					offset = recentLZ[0+adj]
					recentLZ[0+adj] = recentLZ[0]
				case rd.decodeBit(&lzRepStates[1], numLZRepProbs, d.lzRep[1]) == 0:
					offset = recentLZ[1+adj]
					recentLZ[1+adj] = recentLZ[1]
					recentLZ[1] = recentLZ[0]
				default:
					offset = recentLZ[2+adj]
					recentLZ[2+adj] = recentLZ[2]
					recentLZ[2] = recentLZ[1]
					recentLZ[1] = recentLZ[0]
				}
			}
			recentLZ[0] = offset
			prevItemType = 1

			length := int(d.decodeLength(bs))
			if int(offset) > pos || length > outSize-pos {
				return nil, ErrInvalid
			}
			src := pos - int(offset)
			for range length {
				out[pos] = out[src]
				pos++
				src++
			}
			continue
		}

		// Delta match.
		var pair uint64
		if rd.decodeBit(&deltaState, numDeltaProbs, d.delta) == 0 {
			power := uint64(d.deltaPower.decode(bs))
			rawOffset := uint64(d.decodeDeltaOffset(bs))
			pair = (power << 32) | rawOffset
			recentDelta[3] = recentDelta[2]
			recentDelta[2] = recentDelta[1]
			recentDelta[1] = recentDelta[0]
		} else {
			adj := prevItemType >> 1
			switch {
			case rd.decodeBit(&deltaRepStates[0], numDeltaRepProbs, d.deltaRep[0]) == 0:
				pair = recentDelta[0+adj]
				recentDelta[0+adj] = recentDelta[0]
			case rd.decodeBit(&deltaRepStates[1], numDeltaRepProbs, d.deltaRep[1]) == 0:
				pair = recentDelta[1+adj]
				recentDelta[1+adj] = recentDelta[1]
				recentDelta[1] = recentDelta[0]
			default:
				pair = recentDelta[2+adj]
				recentDelta[2+adj] = recentDelta[2]
				recentDelta[2] = recentDelta[1]
				recentDelta[1] = recentDelta[0]
			}
		}
		recentDelta[0] = pair
		prevItemType = 2

		length := int(d.decodeLength(bs))
		power := uint32(pair >> 32)
		rawOffset := uint32(pair)
		span := uint32(1) << power
		offset := rawOffset << power

		if offset>>power != rawOffset || offset+span < offset ||
			int(offset+span) > pos || length > outSize-pos {
			return nil, ErrInvalid
		}
		match := pos - int(offset)
		for range length {
			out[pos] = out[match] + out[pos-int(span)] - out[match-int(span)]
			pos++
			match++
		}
	}

	x86Filter(out)
	return out, nil
}
