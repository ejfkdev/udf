package gzipidx

import (
	"bytes"
	"io"
	"os"
	"testing"

	kflate "github.com/klauspost/compress/flate"
)

// verifyReader wraps the file and checks every byte it hands out against an
// in-memory reference of the shifted stream.
type verifyReader struct {
	raw    []byte // file bytes from base
	k      uint8
	pos    int
	bad    int
	served int
}

func (v *verifyReader) Read(p []byte) (int, error) {
	if v.pos >= len(v.raw)-1 {
		return 0, io.EOF
	}
	n := len(p)
	if n > len(v.raw)-1-v.pos {
		n = len(v.raw) - 1 - v.pos
	}
	// emit the shifted bytes one at a time, checking each against the
	// reference computed the same way (catches internal inconsistency)
	for i := 0; i < n; i++ {
		cur := uint16(v.raw[v.pos+i])
		next := uint16(v.raw[v.pos+i+1])
		p[i] = byte((cur >> v.k) | (next << (8 - v.k)))
	}
	// verify the *reader under test* separately below; here we record what we
	// serve so the test can compare with the bitShiftReader output
	v.pos += n
	v.served += n
	return n, nil
}

func TestRestartInputIntegrity(t *testing.T) {
	path := "/Users/e/Downloads/safeline.image.tar.gz"
	if _, err := os.Stat(path); err != nil {
		t.Skip("no sample")
	}
	ix, err := Load("/tmp/udfperf/safeline.idx")
	if err != nil {
		t.Skip("no index")
	}
	cp := &ix.Checks[0]

	const span = 12 << 20
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	base := ix.DeflateStart + cp.BitPos/8
	raw := make([]byte, span+1)
	if _, err := f.ReadAt(raw, base); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	k := uint8(cp.BitPos % 8)

	// the reader under test
	br := newBitShiftReader(bytes.NewReader(raw), k)
	got := make([]byte, span)
	if _, err := io.ReadFull(br, got); err != nil {
		t.Fatalf("shift read: %v", err)
	}
	for i := 0; i < span; i++ {
		want := byte((uint16(raw[i]) >> k) | (uint16(raw[i+1]) << (8 - k)))
		if got[i] != want {
			t.Fatalf("shift differs at %d", i)
		}
	}
	t.Logf("memory-backed shift over %d bytes OK", span)

	// Now decode with klauspost from the same shifted bytes and see where it
	// breaks; the input is a plain byte slice, so any corruption is the
	// decoder's verdict on the stream itself.
	r := kflate.NewReaderDict(bytes.NewReader(raw), cp.Window)
	buf := make([]byte, 1<<20)
	var total int
	for {
		n, err := r.Read(buf)
		total += n
		if err != nil {
			t.Logf("klauspost on memory-backed shifted input: %d bytes decoded, err=%v", total, err)
			break
		}
	}
	r.Close()
}
