package lzms

import "encoding/binary"

const (
	x86IDWindowSize      = 65535
	x86MaxTranslationOff = 1023
)

// x86Filter undoes LZMS's x86 relative-address translation that the compressor
// applied before LZ factorization. It is a port of wimlib's lzms_x86_filter
// with undo=true.
func x86Filter(data []byte) {
	size := len(data)
	if size <= 17 {
		return
	}

	var lastTargetUsages [65536]int
	for i := range lastTargetUsages {
		lastTargetUsages[i] = -x86IDWindowSize - 1
	}
	lastX86Pos := -x86MaxTranslationOff - 1

	// Sentinel: guarantee findNextOpcode terminates without per-byte bounds
	// checks, then restore the byte afterwards.
	sentinel := size - 8
	saved := data[sentinel]
	data[sentinel] = 0xE8
	tail := size - 16

	p := 1
	for {
		p = findNextOpcode(data, p)
		if p >= tail {
			break
		}
		p, lastX86Pos = translateIfNeeded(data, p, lastX86Pos, lastTargetUsages[:])
	}

	data[sentinel] = saved
}

func isPotentialOpcode(b byte) bool {
	switch b {
	case 0x48, 0x4C, 0xE8, 0xE9, 0xF0, 0xFF:
		return true
	}
	return false
}

func findNextOpcode(data []byte, p int) int {
	for !isPotentialOpcode(data[p]) {
		p++
	}
	return p
}

func translateIfNeeded(data []byte, p, lastX86Pos int, lastTargetUsages []int) (int, int) {
	maxTransOffset := x86MaxTranslationOff
	opcodeNbytes := 0

	switch {
	case data[p] >= 0xF0:
		if data[p]&0x0F != 0 {
			// 0xFF: call indirect relative
			if data[p+1] == 0x15 {
				opcodeNbytes = 2
				break
			}
			return p + 1, lastX86Pos
		}
		// 0xF0: lock add relative
		if data[p+1] == 0x83 && data[p+2] == 0x05 {
			opcodeNbytes = 3
			break
		}
		return p + 1, lastX86Pos
	case data[p] <= 0x4C:
		// 0x48/0x4C REX prefix: LEA or MOV with RIP-relative addressing
		if data[p+2]&0x07 == 0x05 &&
			(data[p+1] == 0x8D ||
				(data[p+1] == 0x8B && data[p]&0x04 == 0 && data[p+2]&0xF0 == 0)) {
			opcodeNbytes = 3
			break
		}
		return p + 1, lastX86Pos
	default:
		// 0xE8 (call) or 0xE9 (jump)
		if data[p]&0x01 != 0 {
			return p + 4, lastX86Pos // 0xE9 is excluded from translation
		}
		opcodeNbytes = 1
		maxTransOffset >>= 1
	}

	i := p
	p += opcodeNbytes
	if i-lastX86Pos <= maxTransOffset {
		n := binary.LittleEndian.Uint32(data[p:])
		binary.LittleEndian.PutUint32(data[p:], n-uint32(i))
	}
	target16 := uint16(uint32(i) + uint32(binary.LittleEndian.Uint16(data[p:])))

	i += opcodeNbytes + 4 - 1
	if i-lastTargetUsages[target16] <= x86IDWindowSize {
		lastX86Pos = i
	}
	lastTargetUsages[target16] = i

	return p + 4, lastX86Pos
}
