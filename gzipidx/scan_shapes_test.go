package gzipidx

import (
	"bufio"
	"bytes"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	kgzip "github.com/klauspost/compress/gzip"
)

// The symbol loop has separate paths for literals, short and long matches,
// stored blocks, the end of a stream and buffer refills. These cases cover
// them from the outside: several payload shapes (incompressible, long repeats,
// text-like, short repeats, mixed) at several compression levels and sizes
// around the scanner's internal buffer sizes, decoded both through a reader
// and through a memory mapping exactly like the index build does.

func gzipFileFor(t *testing.T, payload []byte, level, chunk int) (path string, deflate []byte, headerSize int64) {
	t.Helper()

	var buf bytes.Buffer
	zw, err := kgzip.NewWriterLevel(&buf, level)
	if err != nil {
		t.Fatal(err)
	}
	if chunk <= 0 {
		if _, err := zw.Write(payload); err != nil {
			t.Fatal(err)
		}
	} else {
		// Flush between chunks so the encoder closes blocks: this is what
		// exercises block boundaries, refills and restart points.
		for off := 0; off < len(payload); off += chunk {
			end := off + chunk
			if end > len(payload) {
				end = len(payload)
			}
			if _, err := zw.Write(payload[off:end]); err != nil {
				t.Fatal(err)
			}
			if err := zw.Flush(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := zw.Write([]byte{}); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	all := buf.Bytes()
	br := bufio.NewReader(bytes.NewReader(all))
	headerSize, err = gzipHeaderSize(br)
	if err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(t.TempDir(), "payload.gz")
	if err := os.WriteFile(path, all, 0o644); err != nil {
		t.Fatal(err)
	}
	return path, all[headerSize:], headerSize
}

func payloadShape(kind string, size int) []byte {
	rng := rand.New(rand.NewSource(7))
	out := make([]byte, size)
	switch kind {
	case "random": // incompressible: the encoder emits stored blocks
		rng.Read(out)
	case "zeros": // one enormous match
		out = out[:0]
		for len(out) < size {
			out = append(out, make([]byte, min(size-len(out), 1<<20))...)
		}
	case "text": // literal-heavy with short matches
		words := []string{"the ", "quick ", "brown ", "fox ", "jumps ", "over ", "lazy ", "dog ", "and ", "runs ", "away "}
		out = out[:0]
		for len(out) < size {
			out = append(out, words[rng.Intn(len(words))]...)
		}
		out = out[:size]
	case "shortrepeat": // many matches shorter than eight bytes
		out = out[:0]
		unit := []byte("ab12")
		for len(out) < size {
			out = append(out, unit...)
		}
		out = out[:size]
	default: // mixed: scattered repeats, like a real tar layer
		for off := 0; off < size; off += 256 {
			end := min(off+256, size)
			if rng.Intn(4) != 0 {
				rng.Read(out[off:end])
			} else {
				for i := off; i < end; i++ {
					out[i] = byte('a' + rng.Intn(4))
				}
			}
		}
	}
	if len(out) != size {
		panic("shape size mismatch")
	}
	return out
}

func TestScannerShapes(t *testing.T) {
	// 1 MiB is the scanner's read buffer and flush threshold, so the sizes
	// around it hit refills, buffer drains and flush boundaries.
	sizes := []int{1, 1000, 1 << 20, (1 << 20) + 12345, 3 << 20}
	levels := []int{kgzip.NoCompression, kgzip.BestSpeed, kgzip.DefaultCompression, kgzip.BestCompression}
	chunks := []int{0, 64 << 10}

	for _, kind := range []string{"random", "zeros", "text", "shortrepeat", "mixed"} {
		for _, size := range sizes {
			payload := payloadShape(kind, size)
			for _, level := range levels {
				for _, chunk := range chunks {
					path, deflate, headerSize := gzipFileFor(t, payload, level, chunk)
					name := kind + "/" + itoa(size) + "/l" + itoa(level) + "/c" + itoa(chunk)

					// Reader path, positioned at the deflate stream.
					var got bytes.Buffer
					br := bufio.NewReaderSize(bytes.NewReader(deflate), 1<<16)
					total, err := ScanBlockBoundaries(br, nil, func(_ int64, p []byte) error {
						_, werr := got.Write(p)
						return werr
					})
					if err != nil {
						t.Fatalf("%s: scan: %v", name, err)
					}
					if total != int64(len(payload)) || !bytes.Equal(got.Bytes(), payload) {
						t.Fatalf("%s: reader path mismatch (total=%d want=%d, equal=%v)",
							name, total, len(payload), bytes.Equal(got.Bytes(), payload))
					}

					// Mapped path (what index builds use).
					var got2 bytes.Buffer
					total2, err := ScanMapped(deflate, nil, func(_ int64, p []byte) error {
						_, werr := got2.Write(p)
						return werr
					})
					if err != nil {
						t.Fatalf("%s: mapped scan: %v", name, err)
					}
					if total2 != total || !bytes.Equal(got2.Bytes(), payload) {
						t.Fatalf("%s: mapped path mismatch (total=%d want=%d)", name, total2, total)
					}

					// Byte-aligned block boundaries must be usable as restart
					// points: the stock decompressor restarted there with the
					// preceding window has to reproduce the stream.
					var checks []Checkpoint
					next := int64(size) / 2
					_, err = ScanMapped(deflate, func(bitPos, outPos int64, window func() []byte) bool {
						if bitPos%8 == 0 && outPos >= next && outPos < int64(len(payload)) {
							next = outPos + int64(size)/2 + 1
							checks = append(checks, Checkpoint{BitPos: bitPos, OutPos: outPos, Window: window()})
						}
						return true
					}, nil)
					if err != nil {
						t.Fatalf("%s: boundary scan: %v", name, err)
					}
					for i, cp := range checks {
						r, err := newRestartReader(path, headerSize, cp.BitPos, cp.Window)
						if err != nil {
							t.Fatalf("%s: restart %d: %v", name, i, err)
						}
						n := min(int64(checkpointCtxLen), int64(len(payload))-cp.OutPos)
						got := make([]byte, n)
						m, err := io.ReadFull(r, got)
						_ = r.Close()
						if err != nil || int64(m) != n || !bytes.Equal(got, payload[cp.OutPos:cp.OutPos+n]) {
							t.Fatalf("%s: restart %d at bit %d diverged (read %d of %d, err=%v)",
								name, i, cp.BitPos, m, n, err)
						}
					}
				}
			}
		}
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
