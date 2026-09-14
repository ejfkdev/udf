package archive

import (
	"encoding/binary"
	"fmt"
)

// A minimal reader for CPython's internal "marshal" serialization, covering
// the value types that appear in a PyInstaller PYZ table of contents:
// dicts, lists, tuples, strings, bytes, ints, bools and None. Code objects
// and other exotic types are rejected — udf treats PYZ members as opaque
// zlib-compressed code blobs and never needs to decode them.

type marshalReader struct {
	buf   []byte
	pos   int
	depth int
	refs  []any // object reference table (version >= 4 streams)
}

const marshalMaxDepth = 64

func (r *marshalReader) need(n int) error {
	if r.pos+n > len(r.buf) {
		return fmt.Errorf("marshal data truncated")
	}
	return nil
}

func (r *marshalReader) readByte() (byte, error) {
	if err := r.need(1); err != nil {
		return 0, err
	}
	b := r.buf[r.pos]
	r.pos++
	return b, nil
}

func (r *marshalReader) readInt32() (int32, error) {
	if err := r.need(4); err != nil {
		return 0, err
	}
	v := int32(binary.LittleEndian.Uint32(r.buf[r.pos : r.pos+4]))
	r.pos += 4
	return v, nil
}

func (r *marshalReader) readUint32() (uint32, error) {
	if err := r.need(4); err != nil {
		return 0, err
	}
	v := binary.LittleEndian.Uint32(r.buf[r.pos : r.pos+4])
	r.pos += 4
	return v, nil
}

func (r *marshalReader) readBytes(n int) ([]byte, error) {
	if err := r.need(n); err != nil {
		return nil, err
	}
	b := r.buf[r.pos : r.pos+n]
	r.pos += n
	return b, nil
}

// longDigits reads a marshal long: an int32 digit count followed by 15-bit
// little-endian digits.
func (r *marshalReader) readLong() (int64, error) {
	n, err := r.readInt32()
	if err != nil {
		return 0, err
	}
	neg := false
	if n < 0 {
		neg, n = true, -n
	}
	if n == 0 {
		return 0, nil
	}
	if n > 4 {
		return 0, fmt.Errorf("marshal integer too large (%d digits)", n)
	}
	var v int64
	for i := int32(0); i < n; i++ {
		if err := r.need(2); err != nil {
			return 0, err
		}
		d := int64(binary.LittleEndian.Uint16(r.buf[r.pos : r.pos+2]))
		r.pos += 2
		v |= d << (15 * uint(i))
	}
	if neg {
		v = -v
	}
	return v, nil
}

// value reads one marshal value. Only the types listed above are understood;
// anything else returns an error.
func (r *marshalReader) value() (any, error) {
	if r.depth > marshalMaxDepth {
		return nil, fmt.Errorf("marshal nesting too deep")
	}
	code, err := r.readByte()
	if err != nil {
		return nil, err
	}
	flagRef := code&0x80 != 0
	code &^= 0x80
	v, err := r.decode(code)
	if err != nil {
		return nil, err
	}
	if flagRef {
		r.refs = append(r.refs, v)
	}
	return v, nil
}

func (r *marshalReader) decode(code byte) (any, error) {
	switch code {
	case 'N': // None
		return nil, nil
	case 'F': // False
		return false, nil
	case 'T': // True
		return true, nil
	case 'S', '.': // StopIteration, Ellipsis
		return nil, nil
	case 'i': // int32
		v, err := r.readInt32()
		return int64(v), err
	case 'I': // int64 (3.4+)
		lo, err := r.readUint32()
		if err != nil {
			return nil, err
		}
		hi, err := r.readInt32()
		if err != nil {
			return nil, err
		}
		return int64(lo) | int64(hi)<<32, nil
	case 'l': // long
		return r.readLong()
	case 'y': // bytes
		n, err := r.readInt32()
		if err != nil {
			return nil, err
		}
		if n < 0 {
			return nil, fmt.Errorf("negative marshal bytes length")
		}
		b, err := r.readBytes(int(n))
		if err != nil {
			return nil, err
		}
		cp := make([]byte, len(b))
		copy(cp, b)
		return cp, nil
	case 'u', 'a', 's': // unicode / legacy string: NUL-free UTF-8 bytes
		n, err := r.readInt32()
		if err != nil {
			return nil, err
		}
		if n < 0 {
			return nil, fmt.Errorf("negative marshal string length")
		}
		b, err := r.readBytes(int(n))
		if err != nil {
			return nil, err
		}
		return string(b), nil
	case 'z': // short ascii
		n, err := r.readByte()
		if err != nil {
			return nil, err
		}
		b, err := r.readBytes(int(n))
		if err != nil {
			return nil, err
		}
		return string(b), nil
	case 'Z': // short ascii interned
		n, err := r.readByte()
		if err != nil {
			return nil, err
		}
		b, err := r.readBytes(int(n))
		if err != nil {
			return nil, err
		}
		return string(b), nil
	case 'A': // short ascii interned (3.4+)
		n, err := r.readByte()
		if err != nil {
			return nil, err
		}
		b, err := r.readBytes(int(n))
		if err != nil {
			return nil, err
		}
		return string(b), nil
	case 'r': // reference to an earlier flagged object
		idx, err := r.readInt32()
		if err != nil {
			return nil, err
		}
		if idx < 0 || int(idx) >= len(r.refs) {
			return nil, fmt.Errorf("marshal reference %d out of range (%d objects)", idx, len(r.refs))
		}
		return r.refs[idx], nil
	case '{': // dict
		r.depth++
		defer func() { r.depth-- }()
		m := map[any]any{}
		for {
			key, err := r.value()
			if err != nil {
				return nil, err
			}
			if key == nil {
				// TYPE_NULL ('0') terminates the dict; it is already consumed.
				return m, nil
			}
			val, err := r.value()
			if err != nil {
				return nil, err
			}
			m[key] = val
		}
	case '[': // list
		r.depth++
		defer func() { r.depth-- }()
		n, err := r.readInt32()
		if err != nil {
			return nil, err
		}
		if n < 0 || n > 1<<22 {
			return nil, fmt.Errorf("implausible marshal list length %d", n)
		}
		out := make([]any, 0, n)
		for i := int32(0); i < n; i++ {
			v, err := r.value()
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case '(': // tuple
		r.depth++
		defer func() { r.depth-- }()
		n, err := r.readInt32()
		if err != nil {
			return nil, err
		}
		if n < 0 || n > 1<<22 {
			return nil, fmt.Errorf("implausible marshal tuple length %d", n)
		}
		out := make([]any, 0, n)
		for i := int32(0); i < n; i++ {
			v, err := r.value()
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case ')': // small tuple: 1-byte length (3.4+)
		r.depth++
		defer func() { r.depth-- }()
		n, err := r.readByte()
		if err != nil {
			return nil, err
		}
		out := make([]any, 0, n)
		for i := 0; i < int(n); i++ {
			v, err := r.value()
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case '0': // NULL (dict terminator in some positions)
		return nil, nil
	}
	return nil, fmt.Errorf("unsupported marshal type %q", code)
}

// marshalLoad decodes the first value in buf.
func marshalLoad(buf []byte) (any, error) {
	r := &marshalReader{buf: buf}
	return r.value()
}
