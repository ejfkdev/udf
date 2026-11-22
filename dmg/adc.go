package dmg

import (
	"errors"
	"fmt"
)

// adcDecompress decodes Apple Data Compression (ADC) data, the LZSS variant
// old hdiutil versions wrote into UDIF chunks ("UDCO" images; modern hdiutil
// still produces them for -format UDCO). Ported from dmg2img's adc.c.
//
// The stream is a sequence of tokens led by one byte:
//
//	1xxxxxxx  literal run: (b&0x7f)+1 literal bytes follow
//	01xxxxxx  long match:  length (b&0x3f)+4, then a 16-bit big-endian back-offset
//	00xxxxxx  short match: length ((b&0x3f)>>2)+3, high two bits of b join the
//	                     following byte as the high bits of the back-offset
//
// A match's distance is offset+1, and copies advance one byte at a time so
// overlapping copies expand runs, LZ77 style.
func adcDecompress(comp []byte, want int64) ([]byte, error) {
	if want <= 0 {
		return nil, errors.New("adc chunk with non-positive size")
	}
	out := make([]byte, want)
	in, outPos := 0, 0
	for in < len(comp) && outPos < len(out) {
		b := comp[in]
		in++
		switch {
		case b&0x80 != 0:
			// Literal run.
			n := int(b&0x7f) + 1
			if in+n > len(comp) || outPos+n > len(out) {
				return nil, errors.New("adc literal run exceeds the stream")
			}
			copy(out[outPos:outPos+n], comp[in:in+n])
			in += n
			outPos += n
		case b&0x40 != 0:
			if in+2 > len(comp) {
				return nil, errors.New("adc long match truncated")
			}
			length := int(b&0x3f) + 4
			offset := int(comp[in])<<8 | int(comp[in+1])
			in += 2
			var err error
			outPos, err = adcCopy(out, outPos, length, offset)
			if err != nil {
				return nil, err
			}
		default:
			if in+1 > len(comp) {
				return nil, errors.New("adc short match truncated")
			}
			length := int(b&0x3f)>>2 + 3
			offset := int(b&0x03)<<8 | int(comp[in])
			in++
			var err error
			outPos, err = adcCopy(out, outPos, length, offset)
			if err != nil {
				return nil, err
			}
		}
	}
	if outPos != len(out) {
		return nil, fmt.Errorf("adc chunk decoded %d of %d bytes", outPos, len(out))
	}
	return out, nil
}

// adcCopy copies length bytes from out[outPos-offset-1] (offset 0 repeats the
// previous byte) one byte at a time, so overlapping ranges expand.
func adcCopy(out []byte, outPos, length, offset int) (int, error) {
	src := outPos - offset - 1
	if src < 0 {
		return outPos, fmt.Errorf("adc match back-reference %d before output start", offset+1)
	}
	if outPos+length > len(out) {
		return outPos, fmt.Errorf("adc match of %d bytes overruns the output", length)
	}
	for i := 0; i < length; i++ {
		out[outPos] = out[src+i]
		outPos++
	}
	return outPos, nil
}
