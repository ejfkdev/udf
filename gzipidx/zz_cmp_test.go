package gzipidx

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"testing"

	kgzip "github.com/klauspost/compress/gzip"
)

func TestRestartComparison(t *testing.T) {
	path := "/Users/e/Downloads/safeline.image.tar.gz"
	if _, err := os.Stat(path); err != nil {
		t.Skip("no sample")
	}
	ix, err := Load("/tmp/udfperf/safeline.idx")
	if err != nil {
		t.Skip("no index")
	}
	cp := &ix.Checks[0]
	const span = 48 << 20

	// reference
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := kgzip.NewReader(bufio.NewReaderSize(f, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.CopyN(io.Discard, zr, cp.OutPos); err != nil {
		t.Fatal(err)
	}
	want := make([]byte, span)
	if _, err := io.ReadFull(zr, want); err != nil {
		t.Fatal(err)
	}
	zr.Close()
	f.Close()

	divergence := func(name string, got []byte, err error) {
		d := -1
		for i := range got {
			if i < len(want) && got[i] != want[i] {
				d = i
				break
			}
		}
		t.Logf("%s: %d bytes, err=%v, first divergence=%d", name, len(got), err, d)
	}

	// 1) klauspost restart
	r, err := newRestartReader(path, ix.DeflateStart, cp.BitPos, cp.Window)
	if err != nil {
		t.Fatal(err)
	}
	gotK := make([]byte, span)
	nK, errK := io.ReadFull(r, gotK)
	r.Close()
	divergence("klauspost", gotK[:nK], errK)

	// 2) own scanner restart (window preloaded)
	f2, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f2.Close()
	base := ix.DeflateStart + cp.BitPos/8
	section := io.NewSectionReader(f2, base, 1<<62)
	shifted := newBitShiftReader(section, uint8(cp.BitPos%8))
	var own bytes.Buffer
	sc := newScanner(shifted, nil, func(_ int64, p []byte) error {
		_, e := own.Write(p)
		return e
	})
	sc.out = append(sc.out, cp.Window...)
	sc.outBase = cp.OutPos - int64(len(cp.Window))
	sc.flushed = sc.outBase
	scanErr := sc.scan()
	divergence("own scanner", own.Bytes(), scanErr)
}
