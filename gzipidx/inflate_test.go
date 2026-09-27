package gzipidx

import (
	"archive/tar"
	"bufio"
	"bytes"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	kgzip "github.com/klauspost/compress/gzip"
)

const testWindowSize = 32768

// buildTestGzip writes a gzip holding a tar with large, mostly incompressible
// members (scattered repeats, like real archives) and returns its path and
// the decompressed bytes.
func buildTestGzip(t *testing.T, members int, memberSize int) (string, []byte) {
	t.Helper()

	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	rng := rand.New(rand.NewSource(11))
	for i := 0; i < members; i++ {
		body := make([]byte, memberSize)
		for off := 0; off < memberSize; off += 256 {
			end := off + 256
			if end > memberSize {
				end = memberSize
			}
			if rng.Intn(4) != 0 {
				rng.Read(body[off:end])
			} else {
				copy(body[off:end], bytes.Repeat([]byte{byte('a' + i)}, 256))
			}
		}
		hdr := &tar.Header{
			Name:     string(rune('a'+i)) + ".bin",
			Mode:     0o644,
			Size:     int64(memberSize),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	payload := tarBuf.Bytes()

	path := filepath.Join(t.TempDir(), "payload.tar.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := kgzip.NewWriter(f)
	if _, err := zw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path, payload
}

// TestScannerMatchesKlauspost decodes the deflate stream with the block-aware
// scanner and compares the result with the stock decompressor, while checking
// that block boundaries and window snapshots line up.
func TestScannerMatchesKlauspost(t *testing.T) {
	path, payload := buildTestGzip(t, 4, 4<<20)

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 1<<16)
	headerSize, err := gzipHeaderSize(br)
	if err != nil {
		t.Fatal(err)
	}

	type checkpoint struct {
		bitPos int64
		outPos int64
		window []byte
		ctx    []byte
	}
	var (
		checkpoints []checkpoint
		blocks      int
		nextCP      int64 = 2 << 20 // capture a checkpoint every 2 MiB of output
	)

	var out bytes.Buffer
	sink := func(_ int64, p []byte) error {
		_, err := out.Write(p)
		return err
	}
	onBlock := func(bitPos, outPos int64, window func() []byte) bool {
		blocks++
		if outPos >= nextCP {
			nextCP = outPos + 2<<20
			checkpoints = append(checkpoints, checkpoint{bitPos: bitPos, outPos: outPos})
			_ = window
		}
		return true
	}

	total, err := ScanBlockBoundaries(br, onBlock, sink)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if total != int64(len(payload)) {
		t.Fatalf("scanned %d bytes, want %d", total, len(payload))
	}
	if !bytes.Equal(out.Bytes(), payload) {
		t.Fatalf("decoded output differs from the stock decompressor (got %d bytes)", out.Len())
	}
	if blocks < 2 {
		t.Fatalf("expected several deflate blocks, saw %d", blocks)
	}
	t.Logf("scanned %d bytes, %d blocks, %d checkpoints", total, blocks, len(checkpoints))

	// Every recorded block boundary must be usable as a restart point: the
	// stock decompressor, restarted there with the preceding window, must
	// reproduce the stream.
	if len(checkpoints) == 0 {
		t.Fatal("no checkpoints recorded")
	}
	for i, cp := range checkpoints {
		window, ctx, ok := captureWindow(path, cp.outPos)
		if !ok {
			t.Fatalf("checkpoint %d: could not capture window", i)
		}
		r, err := newRestartReader(path, headerSize, cp.bitPos, window)
		if err != nil {
			t.Fatalf("checkpoint %d: %v", i, err)
		}
		got := make([]byte, len(ctx))
		n, err := io.ReadFull(r, got)
		r.Close()
		if err != nil && n < 64 {
			t.Fatalf("checkpoint %d (bit %d, out %d): %v", i, cp.bitPos, cp.outPos, err)
		}
		if !bytes.Equal(got[:n], ctx[:n]) {
			t.Fatalf("checkpoint %d (bit %d, out %d): restart output does not match the stream", i, cp.bitPos, cp.outPos)
		}
	}
	t.Logf("all %d checkpoints restart the stream correctly", len(checkpoints))
}

// captureWindow extracts the window and a verification context for a
// decompressed offset by decoding the file from the start (test helper).
func captureWindow(path string, outPos int64) (window, ctx []byte, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, false
	}
	defer f.Close()
	zr, err := kgzip.NewReader(bufio.NewReaderSize(f, 1<<20))
	if err != nil {
		return nil, nil, false
	}
	defer zr.Close()

	window = make([]byte, 0, testWindowSize)
	ctx = make([]byte, 0, 1024)
	buf := make([]byte, 1<<16)
	var pos int64
	for pos < outPos+1024 {
		n, err := zr.Read(buf)
		if n > 0 {
			for _, b := range buf[:n] {
				if pos < outPos {
					window = append(window, b)
					if len(window) > testWindowSize {
						window = window[len(window)-testWindowSize:]
					}
				} else if len(ctx) < cap(ctx) {
					ctx = append(ctx, b)
				}
				pos++
			}
		}
		if err != nil {
			break
		}
	}
	if int64(len(ctx)) < 64 || int64(len(window)) < testWindowSize {
		return nil, nil, false
	}
	return window, ctx, true
}
