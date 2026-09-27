package gzipidx

import (
	"bufio"
	"bytes"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	kflate "github.com/klauspost/compress/flate"
	kgzip "github.com/klauspost/compress/gzip"
)

// benchPayload builds a tar-like payload with a realistic mix: mostly
// compressible text/zero runs with incompressible chunks (which make the
// encoder emit stored blocks).
func benchPayload(size int) []byte {
	rng := rand.New(rand.NewSource(99))
	out := make([]byte, 0, size)
	for len(out) < size {
		switch rng.Intn(10) {
		case 0, 1, 2: // incompressible
			chunk := make([]byte, 64<<10)
			rng.Read(chunk)
			out = append(out, chunk...)
		case 3: // long zero run
			out = append(out, make([]byte, 256<<10)...)
		default: // text-ish
			line := []byte("the quick brown fox jumps over the lazy dog 0123456789\n")
			for i := 0; i < 4096 && len(out) < size; i++ {
				out = append(out, line...)
			}
		}
	}
	return out[:size]
}

func benchGzip(t testing.TB, payload []byte, level int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bench.tar.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w, err := kgzip.NewWriterLevel(f, level)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// benchPayloadMixed mimics a docker layer: mostly incompressible bytes with
// scattered text and zero runs, which makes the encoder emit stored blocks.
func benchPayloadMixed(size int) []byte {
	rng := rand.New(rand.NewSource(7))
	out := make([]byte, 0, size)
	for len(out) < size {
		switch rng.Intn(10) {
		case 0, 1, 2, 3, 4: // incompressible payload (binaries, archives)
			chunk := make([]byte, 512<<10)
			rng.Read(chunk)
			out = append(out, chunk...)
		case 5:
			out = append(out, make([]byte, 512<<10)...)
		default:
			line := []byte("usr/lib/x86_64-linux-gnu/libfoo.so.1.2.3\n")
			for i := 0; i < 4096 && len(out) < size; i++ {
				out = append(out, line...)
			}
		}
	}
	return out[:size]
}

func benchScan(b *testing.B, name string, payload []byte, level int) {
	b.Helper()
	path := benchGzip(b, payload, level)
	raw, err := os.ReadFile(path)
	if err != nil {
		b.Fatal(err)
	}
	br := bufio.NewReaderSize(bytes.NewReader(raw), 1<<16)
	hdr, err := gzipHeaderSize(br)
	if err != nil {
		b.Fatal(err)
	}
	deflate := raw[hdr:]
	b.Run(name, func(b *testing.B) {
		b.SetBytes(int64(len(payload)))
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := ScanBlockBoundaries(bytes.NewReader(deflate), nil, func(int64, []byte) error { return nil }); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkScanMixed(b *testing.B) {
	payload := benchPayloadMixed(256 << 20)
	benchScan(b, "mixed", payload, kflate.DefaultCompression)
}

func BenchmarkScanBlockBoundaries(b *testing.B) {
	const size = 256 << 20
	payload := benchPayload(size)
	path := benchGzip(b, payload, kflate.DefaultCompression)
	fi, err := os.Stat(path)
	if err != nil {
		b.Fatal(err)
	}
	b.Logf("payload %d MiB, compressed %d MiB", size>>20, fi.Size()>>20)

	raw, err := os.ReadFile(path)
	if err != nil {
		b.Fatal(err)
	}
	br := bufio.NewReaderSize(bytes.NewReader(raw), 1<<16)
	hdr, err := gzipHeaderSize(br)
	if err != nil {
		b.Fatal(err)
	}
	deflate := raw[hdr:]
	b.SetBytes(int64(size))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := ScanBlockBoundaries(bytes.NewReader(deflate), nil, func(int64, []byte) error { return nil })
		if err != nil {
			b.Fatal(err)
		}
	}
}

var _ = io.Discard
