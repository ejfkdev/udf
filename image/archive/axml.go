package archive

import (
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Binary Android XML (AXML) decoder: turns the compiled form of
// AndroidManifest.xml, layout and resource XML files found inside APKs back
// into readable text XML. Covers the chunk types aapt2 emits: string pool
// (UTF-8 and UTF-16), resource map, namespace and element start/end, and
// CDATA. Typed attribute values are rendered the way apktool does; enum and
// flag names require the resource table and are printed as integers.

// AXML chunk types (ResourceTypes.h).
const (
	axmlResXMLType      = 0x0003
	axmlStringPoolType  = 0x0001
	axmlResourceMapType = 0x0180
	axmlNsStartType     = 0x0100
	axmlNsEndType       = 0x0101
	axmlElemStartType   = 0x0102
	axmlElemEndType     = 0x0103
	axmlCDataType       = 0x0104
)

// Res_value data types.
const (
	axmlTypeNull       = 0x00
	axmlTypeReference  = 0x01
	axmlTypeAttribute  = 0x02
	axmlTypeString     = 0x03
	axmlTypeFloat      = 0x04
	axmlTypeDimension  = 0x05
	axmlTypeFraction   = 0x06
	axmlTypeIntDec     = 0x10
	axmlTypeIntHex     = 0x11
	axmlTypeIntBool    = 0x12
	axmlTypeColorFirst = 0x1c
	axmlTypeColorLast  = 0x1f
)

const axmlUndefinedStr = 0xffffffff

// isAXML reports whether data starts with the binary XML file magic
// (chunk type 0x0003, header size 8).
func isAXML(data []byte) bool {
	return len(data) >= 8 &&
		binary.LittleEndian.Uint16(data[0:2]) == axmlResXMLType &&
		binary.LittleEndian.Uint16(data[2:4]) == 8
}

// axmlPool is a decoded string pool: the shared resource string pool plus the
// binary-XML convention that a 0xFFFFFFFF index means "absent".
type axmlPool struct {
	resStringPool
}

func (p *axmlPool) get(idx uint32) string {
	if idx == axmlUndefinedStr {
		return ""
	}
	return p.resStringPool.get(idx)
}

// axmlDecoder walks the chunk stream and writes text XML.
type axmlDecoder struct {
	data []byte
	pos  int
	pool *axmlPool

	out  strings.Builder
	ns   map[string]string // uri -> prefix
	open []int             // out length when each open element started
}

// DecodeAXML converts binary Android XML into indented-free text XML.
func DecodeAXML(data []byte) (string, error) {
	if !isAXML(data) {
		return "", fmt.Errorf("not a binary android XML file")
	}
	d := &axmlDecoder{data: data, pos: 8, ns: map[string]string{}}
	if err := d.decode(); err != nil {
		return "", err
	}
	return d.out.String(), nil
}

func (d *axmlDecoder) u16(off int) uint16 { return binary.LittleEndian.Uint16(d.data[off:]) }
func (d *axmlDecoder) u32(off int) uint32 { return binary.LittleEndian.Uint32(d.data[off:]) }

func (d *axmlDecoder) decode() error {
	pendingNs := make(map[string]string) // prefix -> uri, flushed into the next start tag
	for d.pos+8 <= len(d.data) {
		chunkType := d.u16(d.pos)
		headerSize := int(d.u16(d.pos + 2))
		chunkSize := int(d.u32(d.pos + 4))
		if chunkSize < 8 || d.pos+chunkSize > len(d.data) {
			return fmt.Errorf("axml chunk %#x size %d out of range at %d", chunkType, chunkSize, d.pos)
		}
		if headerSize < 8 || headerSize > chunkSize {
			return fmt.Errorf("axml chunk %#x implausible header size %d", chunkType, headerSize)
		}
		body := d.pos + headerSize
		end := d.pos + chunkSize

		switch chunkType {
		case axmlStringPoolType:
			pool, err := d.parseStringPool(d.pos, body, end)
			if err != nil {
				return err
			}
			d.pool = pool
		case axmlNsStartType:
			if end-body >= 8 {
				prefix := d.pool.get(d.u32(body))
				uri := d.pool.get(d.u32(body + 4))
				if prefix != "" {
					pendingNs[prefix] = uri
					d.ns[uri] = prefix
				}
			}
		case axmlElemStartType:
			if err := d.element(body, end, pendingNs); err != nil {
				return err
			}
		case axmlElemEndType:
			if end-body >= 8 {
				d.closeElement()
			}
		case axmlCDataType:
			if end-body >= 4 {
				d.out.WriteString(xmlEscape(d.pool.get(d.u32(body))))
			}
		}
		d.pos = end
	}
	return nil
}

// element renders one start tag with its attributes. body points at the
// attrExt structure: ns(4), name(4), attributeStart(2), attributeSize(2),
// attributeCount(2), id/class/style indices(6). attributeStart and the
// attribute records are relative to body.
func (d *axmlDecoder) element(body, end int, pendingNs map[string]string) error {
	if end-body < 20 {
		return fmt.Errorf("truncated element chunk")
	}
	name := d.pool.get(d.u32(body + 4))
	attrStart := int(d.u16(body + 8))
	attrSize := int(d.u16(body + 10))
	attrCount := int(d.u16(body + 12))
	if attrSize == 0 {
		attrSize = 20
	}
	if attrStart == 0 {
		attrStart = 20
	}
	base := body + attrStart
	if base+attrSize*attrCount > end {
		attrCount = (end - base) / attrSize
		if attrCount < 0 {
			attrCount = 0
		}
	}

	d.out.WriteByte('<')
	d.out.WriteString(name)
	for p, u := range pendingNs {
		d.out.WriteString(fmt.Sprintf(" xmlns:%s=%q", p, u))
	}
	for k := range pendingNs {
		delete(pendingNs, k)
	}

	for i := 0; i < attrCount; i++ {
		off := base + i*attrSize
		if off+20 > end {
			break
		}
		nsIdx := d.u32(off)
		nameIdx := d.u32(off + 4)
		rawValue := d.u32(off + 8)
		dt := d.data[off+15]
		data := d.u32(off + 16)

		attrName := d.pool.get(nameIdx)
		if attrName == "" {
			continue
		}
		prefix := ""
		if ns := d.pool.get(nsIdx); ns != "" {
			if p := d.ns[ns]; p != "" {
				prefix = p + ":"
			}
		}
		d.out.WriteString(fmt.Sprintf(" %s%s=%q", prefix, attrName, d.formatValue(rawValue, dt, data)))
	}

	d.open = append(d.open, d.out.Len())
	d.out.WriteByte('>')
	return nil
}

// closeElement collapses an empty element into a self-closing tag.
func (d *axmlDecoder) closeElement() {
	n := len(d.open)
	if n == 0 {
		return
	}
	openLen := d.open[n-1]
	d.open = d.open[:n-1]
	if d.out.Len() == openLen+1 {
		// Nothing was written between '>' and here: self-close.
		s := d.out.String()
		d.out.Reset()
		d.out.WriteString(s[:openLen])
		d.out.WriteString(" />")
	} else {
		// The element name was not kept on a stack; recover it by scanning
		// back to the matching '<' of the open tag.
		s := d.out.String()
		name := axmlTagNameAt(s, openLen)
		d.out.WriteString("</" + name + ">")
	}
}

// axmlTagNameAt extracts the tag name of the open tag whose '>' sits at index
// openLen in s.
func axmlTagNameAt(s string, openLen int) string {
	i := strings.LastIndexByte(s[:openLen], '<')
	if i < 0 {
		return ""
	}
	rest := s[i+1 : openLen]
	if j := strings.IndexAny(rest, " >"); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

func (d *axmlDecoder) formatValue(rawValue uint32, dt byte, data uint32) string {
	switch dt {
	case axmlTypeString:
		if rawValue != axmlUndefinedStr {
			return d.pool.get(rawValue)
		}
		return ""
	case axmlTypeReference:
		if data == 0 {
			return "@null"
		}
		if data>>24 == 0x01 {
			return fmt.Sprintf("@android:%#x", data)
		}
		return fmt.Sprintf("@%#08x", data)
	case axmlTypeAttribute:
		return fmt.Sprintf("?%#08x", data)
	case axmlTypeFloat:
		return strconv.FormatFloat(float64(math.Float32frombits(data)), 'g', -1, 32)
	case axmlTypeDimension:
		return axmlDimension(data)
	case axmlTypeFraction:
		return axmlFraction(data)
	case axmlTypeIntBool:
		if data != 0 {
			return "true"
		}
		return "false"
	case axmlTypeIntHex:
		return fmt.Sprintf("%#x", data)
	case axmlTypeIntDec:
		return fmt.Sprintf("%d", int32(data))
	case axmlTypeNull:
		return ""
	}
	if dt >= axmlTypeColorFirst && dt <= axmlTypeColorLast {
		return axmlColor(dt, data)
	}
	if dt >= 0x07 && dt <= 0x0f {
		return fmt.Sprintf("%d", int32(data))
	}
	return fmt.Sprintf("%#08x", data)
}

// axmlComplexToFloat decodes the 24-bit signed mantissa + radix encoding used
// by dimension and fraction values (ResourceTypes.h complexToFloat).
func axmlComplexToFloat(data uint32) float64 {
	mantissa := float64(int32(data&0xFFFFFF00)) / 256.0
	radix := (data >> 4) & 0x3
	mult := []float64{
		1.0 / float64(1<<7),
		1.0 / float64(1<<15),
		1.0 / float64(1<<23),
		1.0 / float64(uint64(1)<<31),
	}
	return mantissa * mult[radix]
}

func axmlDimension(data uint32) string {
	units := []string{"px", "dp", "sp", "pt", "in", "mm"}
	unit := int(data & 0xf)
	suffix := "px"
	if unit < len(units) {
		suffix = units[unit]
	}
	return strconv.FormatFloat(axmlComplexToFloat(data), 'f', -1, 64) + suffix
}

func axmlFraction(data uint32) string {
	suffix := "%"
	if int(data&0xf) == 1 {
		suffix = "%p"
	}
	return strconv.FormatFloat(axmlComplexToFloat(data)*100, 'f', -1, 64) + suffix
}

// axmlColor renders a color value. The type codes are AOSP's: 0x1c RGB8,
// 0x1d ARGB8, 0x1e RGB4, 0x1f ARGB4 — the four-bit forms expand to eight bits
// per channel, as `aapt dump` and the platform do.
func axmlColor(dt byte, data uint32) string {
	nibble := func(v uint32) uint32 { return v&0xf | (v&0xf)<<4 }
	switch dt {
	case 0x1c: // RGB8
		return fmt.Sprintf("#%06x", data&0xffffff)
	case 0x1d: // ARGB8
		return fmt.Sprintf("#%08x", data)
	case 0x1e: // RGB4
		r, g, b := nibble(data>>8), nibble(data>>4), nibble(data)
		return fmt.Sprintf("#%02x%02x%02x", r, g, b)
	case 0x1f: // ARGB4
		a, r, g, b := nibble(data>>12), nibble(data>>8), nibble(data>>4), nibble(data)
		return fmt.Sprintf("#%02x%02x%02x%02x", a, r, g, b)
	}
	return fmt.Sprintf("#%08x", data)
}

func xmlEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// parseStringPool decodes the string-pool chunk through the shared resource
// string pool parser (the resource table uses the same chunk layout).
func (d *axmlDecoder) parseStringPool(chunkStart, body, end int) (*axmlPool, error) {
	pool, err := parseResStringPool(d.data, chunkStart, body, end)
	if err != nil {
		return nil, err
	}
	return &axmlPool{resStringPool: *pool}, nil
}
