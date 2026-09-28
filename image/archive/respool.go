package archive

import (
	"encoding/binary"
	"fmt"
	"unicode/utf16"
)

// resStringPool is an Android resource string pool, the chunk both binary XML
// (AXML) and the resource table (resources.arsc) draw their strings from. The
// layout is a chunk header, the string offsets, and the string data — UTF-8 or
// UTF-16 depending on a flag, each string length-prefixed with a one- or
// two-unit varint.
type resStringPool struct {
	strings []string
}

func (p *resStringPool) get(idx uint32) string {
	if p == nil || int(idx) >= len(p.strings) {
		return ""
	}
	return p.strings[idx]
}

// parseResStringPool decodes a string-pool chunk. chunkStart is the offset of
// the ResChunk_header; body points past the pool header, directly at the string
// offset array; end bounds the pool's data.
func parseResStringPool(data []byte, chunkStart, body, end int) (*resStringPool, error) {
	headerSize := body - chunkStart
	if headerSize < 28 {
		return nil, fmt.Errorf("string pool header too small: %d", headerSize)
	}
	u32 := func(off int) uint32 { return binary.LittleEndian.Uint32(data[off:]) }
	stringCount := int(u32(chunkStart + 8))
	flags := u32(chunkStart + 16)
	stringsStart := int(u32(chunkStart + 20))
	utf8 := flags&(1<<8) != 0

	if stringCount < 0 || stringCount > 1<<20 {
		return nil, fmt.Errorf("implausible string count %d", stringCount)
	}
	offsetsBase := chunkStart + headerSize
	if offsetsBase+stringCount*4 > end {
		return nil, fmt.Errorf("string pool offsets out of range")
	}
	dataBase := chunkStart + stringsStart
	if dataBase < offsetsBase || dataBase > end {
		return nil, fmt.Errorf("string pool data start out of range")
	}

	pool := &resStringPool{strings: make([]string, stringCount)}
	for i := 0; i < stringCount; i++ {
		off := int(u32(offsetsBase + i*4))
		p := dataBase + off
		if off < 0 || p < dataBase || p >= end {
			continue
		}
		// A malformed string must not kill the whole decode; lenient
		// decoders leave it empty.
		if s, err := decodeResPoolString(data, p, end, utf8); err == nil {
			pool.strings[i] = s
		}
	}
	return pool, nil
}

func decodeResPoolString(data []byte, p, end int, utf8 bool) (string, error) {
	u16 := func(off int) uint16 { return binary.LittleEndian.Uint16(data[off:]) }
	if utf8 {
		// One or two bytes of character count, then one or two bytes of
		// byte length, then the UTF-8 data and a NUL.
		if p+2 > end {
			return "", fmt.Errorf("truncated utf8 string")
		}
		q := p
		n := int(data[q])
		q++
		if n&0x80 != 0 {
			if q >= end {
				return "", fmt.Errorf("truncated utf8 string length")
			}
			n = (n&0x7f)<<8 | int(data[q])
			q++
		}
		_ = n // character count; the byte length follows
		blen := int(data[q])
		q++
		if blen&0x80 != 0 {
			if q >= end {
				return "", fmt.Errorf("truncated utf8 string byte length")
			}
			blen = (blen&0x7f)<<8 | int(data[q])
			q++
		}
		if blen < 0 || q+blen > end {
			return "", fmt.Errorf("utf8 string data out of range")
		}
		return string(data[q : q+blen]), nil
	}
	if p+2 > end {
		return "", fmt.Errorf("truncated utf16 string")
	}
	q := p
	n := int(u16(q))
	q += 2
	if n&0x8000 != 0 {
		if q+2 > end {
			return "", fmt.Errorf("truncated utf16 string length")
		}
		n = (n&0x7fff)<<16 | int(u16(q))
		q += 2
	}
	if n < 0 || q+n*2 > end {
		return "", fmt.Errorf("utf16 string data out of range")
	}
	// UTF-16, including surrogate pairs.
	units := make([]uint16, n)
	for i := 0; i < n; i++ {
		units[i] = u16(q + i*2)
	}
	return string(utf16.Decode(units)), nil
}
