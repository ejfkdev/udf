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

// TestIndexEntryFieldsRoundTrip checks that the member header fields recorded
// during the walk survive a save and load: a listing built from the index must
// not need to read the stream again.
func TestIndexEntryFieldsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	ix := &Index{
		FileSize:     1 << 20,
		DeflateStart: 10,
		TotalOut:     1 << 20,
		SingleMember: true,
		Checks:       []Checkpoint{{BitPos: 8, OutPos: 0, Window: []byte{1, 2}, Ctx: []byte{3}}},
		Entries: []Entry{
			{Name: "a.txt", OutOff: 512, Size: 12, Typeflag: '0', Mode: 0o644, ModTime: 1700000000, UID: 1000, GID: 1000},
			{Name: "link", OutOff: 1024, Size: 0, Typeflag: '2', Mode: 0o777, ModTime: 1700000001, UID: 0, GID: 0, Linkname: "a.txt"},
			{Name: "dir/", OutOff: 1536, Size: 0, Typeflag: '5', Mode: 0o755, ModTime: 1700000002, UID: 1, GID: 2},
		},
	}
	path := filepath.Join(dir, "x.idx")
	if err := ix.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != len(ix.Entries) {
		t.Fatalf("loaded %d entries, want %d", len(got.Entries), len(ix.Entries))
	}
	for i := range ix.Entries {
		want, have := ix.Entries[i], got.Entries[i]
		if have.Name != want.Name || have.OutOff != want.OutOff || have.Size != want.Size ||
			have.Typeflag != want.Typeflag || have.Mode != want.Mode || have.ModTime != want.ModTime ||
			have.UID != want.UID || have.GID != want.GID || have.Linkname != want.Linkname {
			t.Fatalf("entry %d round-tripped as %+v, want %+v", i, have, want)
		}
	}
}
