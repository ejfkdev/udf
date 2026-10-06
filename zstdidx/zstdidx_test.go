package zstdidx

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// frameBytes compresses a payload as one frame (the encoder writes the content
// size, as a parallel writer does).
func frameBytes(t *testing.T, payload []byte) []byte {
	t.Helper()
	zw, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer zw.Close()
	return zw.EncodeAll(payload, nil)
}

// frameBytesStreaming compresses a payload as one frame without recording its
// size, the way a streaming writer (tar --zstd) does.
func frameBytesStreaming(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// skippableFrame wraps a payload in a skippable frame, which a decoder steps
// over (the seekable format's table lives in one).
func skippableFrame(payload []byte) []byte {
	var out bytes.Buffer
	var hdr [8]byte
	binary.LittleEndian.PutUint32(hdr[0:4], skippableBase)
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(len(payload)))
	out.Write(hdr[:])
	out.Write(payload)
	return out.Bytes()
}

func writeTemp(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stream.zst")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestScanFrames(t *testing.T) {
	a := bytes.Repeat([]byte("first frame payload "), 4096)
	b := bytes.Repeat([]byte("second frame payload "), 1024)
	first, second := frameBytes(t, a), frameBytes(t, b)

	// Two frames, with a skippable frame between them.
	stream := append(append(append([]byte{}, first...), skippableFrame([]byte("seek table"))...), second...)
	path := writeTemp(t, stream)

	frames, err := scanFrames(path)
	if err != nil {
		t.Fatalf("scanFrames: %v", err)
	}
	if len(frames) != 2 {
		t.Fatalf("found %d frames, want 2", len(frames))
	}
	if frames[0].CompOff != 0 || frames[0].CompSize != int64(len(first)) {
		t.Fatalf("first frame extent %+v, want 0..%d", frames[0], len(first))
	}
	wantOff := int64(len(first) + 8 + len("seek table"))
	if frames[1].CompOff != wantOff || frames[1].CompSize != int64(len(second)) {
		t.Fatalf("second frame extent %+v, want %d..%d", frames[1], wantOff, wantOff+int64(len(second)))
	}
	if frames[0].OutSize != int64(len(a)) || frames[1].OutSize != int64(len(b)) {
		t.Fatalf("output sizes %d/%d, want %d/%d", frames[0].OutSize, frames[1].OutSize, len(a), len(b))
	}

	// The extents must be exactly what a decoder needs: each slice has to
	// decode to the payload it was built from.
	for i, want := range [][]byte{a, b} {
		dec, err := zstd.NewReader(bytes.NewReader(stream[frames[i].CompOff : frames[i].CompOff+frames[i].CompSize]))
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(dec)
		dec.Close()
		if err != nil {
			t.Fatalf("frame %d does not decode on its own: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("frame %d decoded %d bytes, want %d", i, len(got), len(want))
		}
	}
}

// buildTar returns a tar with the given members and the offset of each.
func buildTar(t *testing.T, files map[string][]byte) ([]byte, map[string]int64) {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	offsets := map[string]int64{}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	// deterministic order
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	for _, name := range names {
		body := files[name]
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
		offsets[name] = int64(buf.Len() - len(body))
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), offsets
}

func TestBuildAndReadAt(t *testing.T) {
	body := bytes.Repeat([]byte("0123456789abcdef"), 1<<16) // 1 MiB
	files := map[string][]byte{
		"first.bin":  body,
		"README.txt": []byte("hello zstd index\n"),
		"last.bin":   bytes.Repeat([]byte("z"), 1<<18),
	}
	raw, offsets := buildTar(t, files)

	// Single frame: the index lists members but cannot seek inside the frame.
	single := writeTemp(t, frameBytes(t, raw))
	ix, err := Build(single)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if ix.Seekable() {
		t.Fatal("a single-frame stream should not claim to be seekable")
	}
	if ix.TotalOut != int64(len(raw)) {
		t.Fatalf("total output %d, want %d", ix.TotalOut, len(raw))
	}
	if len(ix.Entries) != 3 {
		t.Fatalf("indexed %d members, want 3: %+v", len(ix.Entries), ix.Entries)
	}
	r, err := OpenReader(single, ix)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for name, want := range files {
		e, ok := ix.Lookup(name)
		if !ok {
			t.Fatalf("%s not in the index", name)
		}
		got, err := readRange(r, e.OutOff, e.Size)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s came back as %d bytes, want %d", name, len(got), len(want))
		}
	}

	// Two frames, split inside the tar: reads then start at the frame that
	// holds the member, and each frame decodes on its own.
	cut := len(raw) / 2
	multi := writeTemp(t, append(frameBytes(t, raw[:cut]), frameBytes(t, raw[cut:])...))
	ix2, err := Build(multi)
	if err != nil {
		t.Fatalf("Build (multi-frame): %v", err)
	}
	if !ix2.Seekable() {
		t.Fatal("a two-frame stream should be seekable")
	}
	if ix2.TotalOut != int64(len(raw)) {
		t.Fatalf("multi-frame total output %d, want %d", ix2.TotalOut, len(raw))
	}
	if len(ix2.Frames) != 2 {
		t.Fatalf("frame table: %+v", ix2.Frames)
	}
	if ix2.Frames[0].OutSize != int64(cut) || ix2.Frames[1].OutOff != int64(cut) {
		t.Fatalf("frame offsets: %+v (cut at %d)", ix2.Frames, cut)
	}
	r2, err := OpenReader(multi, ix2)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	for name, want := range files {
		e, ok := ix2.Lookup(name)
		if !ok {
			t.Fatalf("%s not in the multi-frame index", name)
		}
		got, err := readRange(r2, e.OutOff, e.Size)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s (multi-frame) came back as %d bytes, want %d", name, len(got), len(want))
		}
		// The member must be readable from the frame that contains it: reading
		// it must not depend on earlier frames.
		frame := ix2.Frames[ix2.frameAt(e.OutOff)]
		if e.OutOff < frame.OutOff || e.OutOff >= frame.OutOff+frame.OutSize {
			t.Fatalf("%s at %d does not lie in frame %+v", name, e.OutOff, frame)
		}
	}

	// A stream whose frames record no size (a streaming writer) is still
	// indexed, by measuring each frame while decoding it.
	streamed := writeTemp(t, frameBytesStreaming(t, raw))
	ix3, err := Build(streamed)
	if err != nil {
		t.Fatalf("Build (streaming frame): %v", err)
	}
	if len(ix3.Entries) != 3 {
		t.Fatalf("streaming frame indexed %d members", len(ix3.Entries))
	}
	r3, err := OpenReader(streamed, ix3)
	if err != nil {
		t.Fatal(err)
	}
	defer r3.Close()
	e, _ := ix3.Lookup("README.txt")
	got, err := readRange(r3, e.OutOff, e.Size)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, files["README.txt"]) {
		t.Fatalf("README.txt = %q", got)
	}
	_ = offsets
}

func readRange(r *Reader, off, size int64) ([]byte, error) {
	rc, err := r.ReadAt(off)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	buf := make([]byte, size)
	if _, err := io.ReadFull(rc, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func TestIndexRoundTrip(t *testing.T) {
	raw, _ := buildTar(t, map[string][]byte{"a.txt": []byte("a"), "d/b.txt": []byte("b")})
	path := writeTemp(t, append(frameBytes(t, raw[:len(raw)/2]), frameBytes(t, raw[len(raw)/2:])...))
	ix, err := Build(path)
	if err != nil {
		t.Fatal(err)
	}
	saved := filepath.Join(t.TempDir(), "x.zsidx")
	if err := ix.Save(saved); err != nil {
		t.Fatal(err)
	}
	got, err := Load(saved)
	if err != nil {
		t.Fatal(err)
	}
	if got.TotalOut != ix.TotalOut || len(got.Frames) != len(ix.Frames) || len(got.Entries) != len(ix.Entries) {
		t.Fatalf("loaded %+v, want %+v", got, ix)
	}
	for i := range ix.Frames {
		if got.Frames[i] != ix.Frames[i] {
			t.Fatalf("frame %d: %+v != %+v", i, got.Frames[i], ix.Frames[i])
		}
	}
	for i := range ix.Entries {
		if got.Entries[i] != ix.Entries[i] {
			t.Fatalf("entry %d: %+v != %+v", i, got.Entries[i], ix.Entries[i])
		}
	}
	if digestOf(t, path) == "" {
		t.Fatal("fixture digest")
	}
}

func digestOf(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}
