package gzipidx

import (
	"bufio"
	"os"
	"testing"
	"time"
)

// TestRealWorldSpeed measures the scanner on a real layer-sized gzip member.
// Point UDF_BIGGZ at a large .tar.gz (defaults to a local sample if present).
func TestRealWorldSpeed(t *testing.T) {
	path := os.Getenv("UDF_BIGGZ")
	if path == "" {
		t.Skip("set UDF_BIGGZ to a large .tar.gz to measure scan throughput")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Skip("no sample")
	}
	defer f.Close()
	st, _ := f.Stat()
	br := bufio.NewReaderSize(f, 1<<20)
	hdr, err := gzipHeaderSize(br)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	total, err := scanDeflate(path, hdr, st.Size(), nil, nil)
	d := time.Since(t0)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("out=%d in=%.0fMB %.2fs -> %.0f MB/s out, %.0f MB/s in",
		total, float64(st.Size()-hdr)/1e6, d.Seconds(),
		float64(total)/d.Seconds()/1e6, float64(st.Size()-hdr)/d.Seconds()/1e6)
}

// TestRealWorldBuild measures a full index build (scan, tar directory,
// checkpoint capture and verification) on a real layer-sized gzip member.
func TestRealWorldBuild(t *testing.T) {
	path := os.Getenv("UDF_BIGGZ")
	if path == "" {
		t.Skip("set UDF_BIGGZ to a large .tar.gz to measure index build time")
	}
	t0 := time.Now()
	ix, err := Build(path, BuildOptions{})
	d := time.Since(t0)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("build=%v out=%d checks=%d entries=%d single=%v", d, ix.TotalOut, len(ix.Checks), len(ix.Entries), ix.SingleMember)
}
