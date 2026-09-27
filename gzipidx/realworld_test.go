package gzipidx

import (
	"bufio"
	"os"
	"testing"
	"time"
)

// The tests in this file need a real layer-sized gzip member and report
// timings rather than asserting them. Point UDF_BIGGZ at a large .tar.gz
// (e.g. a docker-save image) to run them; without it they skip.
//
// Timing on this workload has run-to-run noise of several percent, so any
// change to the symbol loop should be judged by alternating runs of
// TestRealWorldSpeed (best of three) for each version, not by single runs.

func realGzip(t *testing.T) (string, int64, int64) {
	t.Helper()
	path := os.Getenv("UDF_BIGGZ")
	if path == "" {
		t.Skip("set UDF_BIGGZ to a large .tar.gz to measure scan throughput")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Skip("no sample")
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	hdr, err := gzipHeaderSize(bufio.NewReaderSize(f, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	return path, hdr, st.Size()
}

// TestRealWorldSpeed decodes the whole stream three times through a mapped
// file and reports the best run.
func TestRealWorldSpeed(t *testing.T) {
	path, hdr, size := realGzip(t)
	best := time.Hour
	var total int64
	for i := 0; i < 3; i++ {
		t0 := time.Now()
		n, err := scanDeflate(path, hdr, size, nil, nil)
		d := time.Since(t0)
		if err != nil {
			t.Fatal(err)
		}
		total = n
		if d < best {
			best = d
		}
	}
	t.Logf("best of 3: out=%d in=%.0fMB %.2fs -> %.0f MB/s out, %.0f MB/s in",
		total, float64(size-hdr)/1e6, best.Seconds(),
		float64(total)/best.Seconds()/1e6, float64(size-hdr)/best.Seconds()/1e6)
}

// TestRealWorldBuild measures a full index build (scan, tar directory,
// checkpoint capture and verification) on a real layer-sized gzip member.
func TestRealWorldBuild(t *testing.T) {
	path, _, _ := realGzip(t)
	t0 := time.Now()
	ix, err := Build(path, BuildOptions{})
	d := time.Since(t0)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("build=%v out=%d checks=%d entries=%d single=%v",
		d, ix.TotalOut, len(ix.Checks), len(ix.Entries), ix.SingleMember)
}
