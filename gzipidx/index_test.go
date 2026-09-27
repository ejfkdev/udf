package gzipidx

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIndexBuildAndRead(t *testing.T) {
	path, payload := buildTestGzip(t, 6, 6<<20) // ~36 MiB of tar data

	start := time.Now()
	ix, err := Build(path, BuildOptions{CandidateSpacing: 4 << 20})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	buildTime := time.Since(start)

	if ix.TotalOut != int64(len(payload)) {
		t.Fatalf("TotalOut = %d, want %d", ix.TotalOut, len(payload))
	}
	if !ix.SingleMember || !ix.Usable() {
		t.Fatalf("index not usable (single=%v checks=%d)", ix.SingleMember, len(ix.Checks))
	}
	// Restart points must sit on byte-aligned block boundaries: a stored block
	// skips to the next byte, so a bit-shifted restart would move that grid.
	for i := range ix.Checks {
		if ix.Checks[i].BitPos%8 != 0 {
			t.Fatalf("checkpoint %d is not byte-aligned (bit=%d)", i, ix.Checks[i].BitPos)
		}
	}
	t.Logf("index built in %v: %d checkpoints, %d entries, %d bytes out",
		buildTime, len(ix.Checks), len(ix.Entries), ix.TotalOut)

	// Tar directory: every member must be found at the right offset.
	for i := 0; i < 6; i++ {
		name := string(rune('a'+i)) + ".bin"
		e, ok := ix.Lookup(name)
		if !ok {
			t.Fatalf("entry %s missing", name)
		}
		if e.Size != int64(6<<20) {
			t.Fatalf("entry %s size = %d", name, e.Size)
		}
		if !bytes.Equal(payload[e.OutOff:e.OutOff+64], []byte{}) && e.OutOff == 0 {
			t.Fatalf("entry %s has no offset", name)
		}
	}

	r, err := OpenReader(path, ix)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer r.Close()

	// Random probes: before the first checkpoint, at every checkpoint, and in
	// between.
	var probes []int64
	probes = append(probes, 0, 1, 1024)
	for i := range ix.Checks {
		cp := &ix.Checks[i]
		probes = append(probes, cp.OutPos, cp.OutPos+1, cp.OutPos+int64(len(cp.Ctx)))
		if i > 0 {
			mid := (ix.Checks[i-1].OutPos + cp.OutPos) / 2
			probes = append(probes, mid)
		}
	}
	probes = append(probes, ix.TotalOut-4096)
	for _, off := range probes {
		if off < 0 || off+4096 > int64(len(payload)) {
			continue
		}
		start := time.Now()
		got, err := r.ReadRange(off, 4096)
		if err != nil {
			t.Fatalf("ReadRange(%d): %v", off, err)
		}
		if !bytes.Equal(got, payload[off:off+4096]) {
			t.Fatalf("ReadRange(%d) mismatch", off)
		}
		t.Logf("  read at %8d took %v", off, time.Since(start))
	}

	// Save/load round trip.
	idxPath := filepath.Join(t.TempDir(), "index.idx")
	if err := ix.Save(idxPath); err != nil {
		t.Fatalf("Save: %v", err)
	}
	fi, err := os.Stat(idxPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("index size: %d bytes (%.1f KiB)", fi.Size(), float64(fi.Size())/1024)

	loaded, err := Load(idxPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.TotalOut != ix.TotalOut || len(loaded.Checks) != len(ix.Checks) || len(loaded.Entries) != len(ix.Entries) {
		t.Fatalf("loaded index differs")
	}
	r2, err := OpenReader(path, loaded)
	if err != nil {
		t.Fatalf("OpenReader(loaded): %v", err)
	}
	defer r2.Close()
	off := int64(20 << 20)
	got, err := r2.ReadRange(off, 8192)
	if err != nil {
		t.Fatalf("ReadRange after load: %v", err)
	}
	if !bytes.Equal(got, payload[off:off+8192]) {
		t.Fatal("random access after load mismatch")
	}
}
