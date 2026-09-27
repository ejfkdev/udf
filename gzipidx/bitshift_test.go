package gzipidx

import (
	"bytes"
	"io"
	"math/rand"
	"testing"
)

// TestBitShiftReader checks the bit-shifted view of the compressed bytes a
// restart uses: with shift k the decoder must see byte i of the stream as
// (raw[i] >> k) | (raw[i+1] << (8-k)), i.e. the stream starting at bit k.
func TestBitShiftReader(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	raw := make([]byte, 5000)
	rng.Read(raw)

	for k := uint8(0); k < 8; k++ {
		for _, chunk := range []int{1, 7, 64, 4096} {
			src := &chunkReader{data: raw, max: chunk}
			r := newBitShiftReader(src, k)

			var got []byte
			buf := make([]byte, chunk)
			for {
				n, err := r.Read(buf)
				got = append(got, buf[:n]...)
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatalf("k=%d chunk=%d: %v", k, chunk, err)
				}
			}

			if k == 0 {
				if !bytes.Equal(got, raw) {
					t.Fatalf("k=0 chunk=%d: passthrough altered the bytes", chunk)
				}
				continue
			}
			// The view has the same length as the input: every byte takes
			// its low bits from the next byte, and the final byte has no
			// successor, so it keeps only its high bits.
			want := make([]byte, len(raw))
			for i := range want {
				var next byte
				if i+1 < len(raw) {
					next = raw[i+1]
				}
				want[i] = raw[i]>>k | next<<(8-k)
			}
			if len(got) != len(want) {
				t.Fatalf("k=%d chunk=%d: got %d bytes, want %d", k, chunk, len(got), len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("k=%d chunk=%d: byte %d = %#02x, want %#02x", k, chunk, i, got[i], want[i])
				}
			}
		}
	}
}

// chunkReader serves at most max bytes per Read, so the reader under test has
// to handle refills at every chunk boundary.
type chunkReader struct {
	data []byte
	pos  int
	max  int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if c.pos >= len(c.data) {
		return 0, io.EOF
	}
	n := min(len(p), c.max, len(c.data)-c.pos)
	copy(p, c.data[c.pos:c.pos+n])
	c.pos += n
	return n, nil
}
