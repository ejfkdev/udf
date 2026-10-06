// Package zstdidx gives a zstd-compressed tar the same random access a gzip one
// gets from gzipidx: one pass records every member's offset and header fields,
// and reads then start at a frame boundary instead of at byte zero.
//
// The two formats differ in one decisive way. A deflate stream restarts at a
// block boundary because its whole state is a 32 KiB window, so gzipidx can put
// checkpoints anywhere. A zstd block needs the decoder's window — up to 128 MiB
// — which is not worth storing, so the only restart points are frame
// boundaries, and a frame decodes independently of the others. Writers that
// split their output into frames therefore give full random access: pzstd, the
// zstd seekable format and containerd's zstd-chunked layers all do. The `zstd`
// CLI and `tar --zstd` write a single frame, where the frame table has one
// entry: the member directory still makes every listing instant and reading a
// member costs one decode instead of two, because the directory is known
// without walking the stream again.
package zstdidx

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/klauspost/compress/zstd"

	"github.com/ejfkdev/udf/gzipidx"
)

// frameMagic starts a zstd frame; 0x184D2A5? starts a skippable one, which is
// stepped over (the seek table of the seekable format lives in one).
const (
	frameMagic    = 0xFD2FB528
	skippableBase = 0x184D2A50
	skippableMask = 0xFFFFFFF0
)

// maxFrames bounds a corrupt stream's frame table.
const maxFrames = 1 << 20

// Frame is one zstd frame: where its compressed bytes are and how much output
// it produces.
type Frame struct {
	CompOff  int64
	CompSize int64
	OutOff   int64
	OutSize  int64
}

// Index is a built index: the frame table plus the tar directory inside the
// stream.
type Index struct {
	FileSize int64
	TotalOut int64
	Frames   []Frame
	Entries  []gzipidx.Entry
}

// Seekable reports whether a read can start away from the first frame: with
// more than one frame it can, because frames decode independently.
func (ix *Index) Seekable() bool { return len(ix.Frames) > 1 }

// Usable reports whether the index can serve a listing.
func (ix *Index) Usable() bool { return len(ix.Entries) > 0 }

// Lookup finds a tar member by exact name.
func (ix *Index) Lookup(name string) (gzipidx.Entry, bool) {
	i := sort.Search(len(ix.Entries), func(i int) bool { return ix.Entries[i].Name >= name })
	if i < len(ix.Entries) && ix.Entries[i].Name == name {
		return ix.Entries[i], true
	}
	return gzipidx.Entry{}, false
}

// frameAt returns the index of the frame holding an output offset.
func (ix *Index) frameAt(outOff int64) int {
	i := sort.Search(len(ix.Frames), func(i int) bool { return ix.Frames[i].OutOff > outOff })
	if i == 0 {
		return 0
	}
	return i - 1
}

// --- building ---

// countingReader tracks how many bytes have been consumed, so frame extents
// come out of the walk itself rather than from arithmetic on the side.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (c *countingReader) readByte() (byte, error) {
	var b [1]byte
	if _, err := io.ReadFull(c.r, b[:]); err != nil {
		return 0, err
	}
	c.n++
	return b[0], nil
}

func (c *countingReader) readFull(buf []byte) error {
	if _, err := io.ReadFull(c.r, buf); err != nil {
		return err
	}
	c.n += int64(len(buf))
	return nil
}

func (c *countingReader) skip(n int64) error {
	if n <= 0 {
		return nil
	}
	m, err := io.CopyN(io.Discard, c.r, n)
	c.n += m
	return err
}

// scanFrames walks the compressed stream and records every frame's extent and,
// when the header carries it, its output size. It reads frame and block headers
// only — no decompression.
func scanFrames(path string) ([]Frame, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}

	cr := &countingReader{r: bufio.NewReaderSize(f, 1<<20)}
	var frames []Frame
	var outOff int64

	for cr.n < st.Size() {
		if len(frames) >= maxFrames {
			return nil, fmt.Errorf("more than %d frames", maxFrames)
		}
		var magicBuf [4]byte
		if err := cr.readFull(magicBuf[:]); err != nil {
			return nil, fmt.Errorf("read frame magic at %d: %w", cr.n-4, err)
		}
		magic := binary.LittleEndian.Uint32(magicBuf[:])
		if magic&skippableMask == skippableBase {
			var sizeBuf [4]byte
			if err := cr.readFull(sizeBuf[:]); err != nil {
				return nil, err
			}
			if err := cr.skip(int64(binary.LittleEndian.Uint32(sizeBuf[:]))); err != nil {
				return nil, err
			}
			continue
		}
		if magic != frameMagic {
			// Something other than a frame here: stop rather than guess, and
			// keep the frames already found.
			break
		}

		start := cr.n - 4
		fhd, err := cr.readByte()
		if err != nil {
			return nil, err
		}
		contentFlag := fhd >> 6
		singleSegment := (fhd>>5)&1 == 1
		checksum := (fhd>>2)&1 == 1
		dictFlag := fhd & 3

		if !singleSegment {
			if _, err := cr.readByte(); err != nil { // window descriptor
				return nil, err
			}
		}
		switch dictFlag {
		case 1:
			if err := cr.skip(1); err != nil {
				return nil, err
			}
		case 2:
			if err := cr.skip(2); err != nil {
				return nil, err
			}
		case 3:
			if err := cr.skip(4); err != nil {
				return nil, err
			}
		}
		fcsSize := 0
		switch contentFlag {
		case 0:
			if singleSegment {
				fcsSize = 1
			}
		case 1:
			fcsSize = 2
		case 2:
			fcsSize = 4
		case 3:
			fcsSize = 8
		}
		var outSize int64
		if fcsSize > 0 {
			var buf [8]byte
			if err := cr.readFull(buf[:fcsSize]); err != nil {
				return nil, err
			}
			for i := fcsSize - 1; i >= 0; i-- {
				outSize = outSize<<8 | int64(buf[i])
			}
			// The two-byte form stores the size minus 256: everything below
			// 256 fits the one-byte form, so the range starts there (RFC 8878
			// §3.1.1.1, and the encoder does the same subtraction).
			if fcsSize == 2 {
				outSize += 256
			}
		}

		for {
			var bh [3]byte
			if err := cr.readFull(bh[:]); err != nil {
				return nil, fmt.Errorf("read block header at %d: %w", cr.n-3, err)
			}
			last := bh[0]&1 == 1
			blockType := (bh[0] >> 1) & 3
			size := int64(bh[0]>>3) | int64(bh[1])<<5 | int64(bh[2])<<13
			if blockType == 1 {
				if err := cr.skip(1); err != nil { // RLE: one content byte
					return nil, err
				}
			} else {
				if err := cr.skip(size); err != nil {
					return nil, err
				}
			}
			if last {
				break
			}
		}
		if checksum {
			if err := cr.skip(4); err != nil {
				return nil, err
			}
		}
		frames = append(frames, Frame{
			CompOff:  start,
			CompSize: cr.n - start,
			OutOff:   outOff,
			OutSize:  outSize,
		})
		outOff += outSize
	}
	if len(frames) == 0 {
		return nil, errors.New("no zstd frames found")
	}
	return frames, nil
}

// buildOptions tunes a build.
type buildOptions struct {
	// noDirectory skips the tar walk, for callers that only want the frames.
	noDirectory bool
}

// Build scans a zstd file once: the compressed pass records the frame table,
// the decode pass records the tar directory. Decoding frame by frame doubles as
// a check on the frame table — a wrong extent fails to decode — and falls back
// to one unseekable frame covering the file when that happens.
func Build(path string) (*Index, error) {
	return build(path, buildOptions{})
}

func build(path string, opts buildOptions) (*Index, error) {
	frames, err := scanFrames(path)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	ix := &Index{FileSize: st.Size(), Frames: frames}

	// When every frame header carries its output size — what pzstd and the
	// seekable format write — the offsets are known without decoding, and the
	// directory pass can decode the stream in one go, which is faster than
	// starting a decoder per frame.
	known := make([]bool, len(frames))
	allKnown := true
	for i := range frames {
		known[i] = frames[i].OutSize > 0
		if !known[i] {
			allKnown = false
		}
	}
	if allKnown {
		var outOff int64
		for i := range ix.Frames {
			ix.Frames[i].OutOff = outOff
			outOff += ix.Frames[i].OutSize
		}
		if err := walkTarWhole(path, ix, opts); err == nil && ix.TotalOut == outOff {
			sort.Slice(ix.Entries, func(i, j int) bool { return ix.Entries[i].Name < ix.Entries[j].Name })
			return ix, nil
		}
		// A size was wrong (or the stream did not decode): fall through to the
		// per-frame pass, which measures rather than trusts.
		for i := range ix.Frames {
			ix.Frames[i].OutOff = 0
		}
	}

	if err := walkTarFrameByFrame(path, ix, opts); err != nil {
		// The frame table did not hold up: scan the stream as one frame.
		ix.Frames = []Frame{{CompOff: 0, CompSize: st.Size()}}
		ix.TotalOut = 0
		if err := walkTarWhole(path, ix, opts); err != nil {
			return nil, err
		}
		return ix, nil
	}
	sort.Slice(ix.Entries, func(i, j int) bool { return ix.Entries[i].Name < ix.Entries[j].Name })
	return ix, nil
}

// walkTarFrameByFrame decompresses each frame on its own, learning the output
// size the header may have omitted and feeding the tar directory scanner.
func walkTarFrameByFrame(path string, ix *Index, opts buildOptions) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var sink *tarSink
	if !opts.noDirectory {
		sink = newTarSink()
	}
	var dst io.Writer = io.Discard
	if sink != nil {
		dst = sink
	}
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return err
	}
	defer dec.Close()

	var outOff int64
	for i := range ix.Frames {
		fr := &ix.Frames[i]
		src := io.NewSectionReader(f, fr.CompOff, fr.CompSize)
		if err := dec.Reset(src); err != nil {
			return err
		}
		n, err := io.Copy(dst, dec)
		if err != nil {
			return fmt.Errorf("frame %d: %w", i, err)
		}
		if fr.OutSize != 0 && fr.OutSize != n {
			return fmt.Errorf("frame %d: header says %d bytes, decoded %d", i, fr.OutSize, n)
		}
		fr.OutSize = n
		fr.OutOff = outOff
		outOff += n
	}
	ix.TotalOut = outOff
	if sink != nil {
		entries, err := sink.finish()
		if err != nil {
			return err
		}
		ix.Entries = entries
	}
	return nil
}

// walkTarWhole decompresses the stream in one go, for a stream whose frames
// could not be trusted on their own.
func walkTarWhole(path string, ix *Index, opts buildOptions) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var sink *tarSink
	if !opts.noDirectory {
		sink = newTarSink()
	}
	dec, err := zstd.NewReader(f)
	if err != nil {
		return err
	}
	defer dec.Close()
	var dst io.Writer = io.Discard
	if sink != nil {
		dst = sink
	}
	n, err := io.Copy(dst, dec)
	if err != nil {
		return err
	}
	ix.TotalOut = n
	if sink != nil {
		entries, err := sink.finish()
		if err != nil {
			return err
		}
		ix.Entries = entries
	}
	return nil
}

// tarSink feeds decompressed bytes to the shared tar directory walk.
type tarSink struct {
	reader *io.PipeReader // the scanner reads this side
	writer *io.PipeWriter // decompressed bytes are written here
	done   chan []gzipidx.Entry
	err    error
}

func newTarSink() *tarSink {
	pr, pw := io.Pipe()
	s := &tarSink{reader: pr, writer: pw, done: make(chan []gzipidx.Entry, 1)}
	go func() {
		entries, err := gzipidx.ScanTarDirectory(pr, nil)
		s.err = err
		s.done <- entries
	}()
	return s
}

func (s *tarSink) Write(p []byte) (int, error) { return s.writer.Write(p) }

func (s *tarSink) finish() ([]gzipidx.Entry, error) {
	if err := s.writer.Close(); err != nil {
		return nil, err
	}
	entries := <-s.done
	if s.err != nil {
		return nil, s.err
	}
	_ = s.reader.Close()
	return entries, nil
}

// --- persistence ---

var magic = [8]byte{'u', 'd', 'f', 'z', 's', 'i', 'd', 'x'}

// Version of the on-disk index format.
const Version = 1

// Save writes the index in a compact binary form.
func (ix *Index) Save(path string) error {
	tmp, err := os.CreateTemp(dirOf(path), ".zsidx-*")
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(tmp, 1<<20)
	if err := ix.writeTo(w); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := w.Flush(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (ix *Index) writeTo(w io.Writer) error {
	if _, err := w.Write(magic[:]); err != nil {
		return err
	}
	for _, v := range []any{uint32(Version), ix.FileSize, ix.TotalOut, uint32(len(ix.Frames)), uint32(len(ix.Entries))} {
		if err := binary.Write(w, binary.LittleEndian, v); err != nil {
			return err
		}
	}
	for i := range ix.Frames {
		fr := &ix.Frames[i]
		for _, v := range []any{fr.CompOff, fr.CompSize, fr.OutOff, fr.OutSize} {
			if err := binary.Write(w, binary.LittleEndian, v); err != nil {
				return err
			}
		}
	}
	for i := range ix.Entries {
		if err := gzipidx.WriteEntry(w, &ix.Entries[i]); err != nil {
			return err
		}
	}
	return nil
}

// Load reads an index written by Save.
func Load(path string) (*Index, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 1<<20)
	var m [8]byte
	if _, err := io.ReadFull(br, m[:]); err != nil {
		return nil, err
	}
	if m != magic {
		return nil, errors.New("not a zstd index")
	}
	var version, nFrames, nEntries uint32
	ix := &Index{}
	for _, v := range []any{&version, &ix.FileSize, &ix.TotalOut, &nFrames, &nEntries} {
		if err := binary.Read(br, binary.LittleEndian, v); err != nil {
			return nil, err
		}
	}
	if version != Version {
		return nil, fmt.Errorf("zstd index version %d, want %d", version, Version)
	}
	if nFrames > maxFrames {
		return nil, fmt.Errorf("implausible frame count %d", nFrames)
	}
	ix.Frames = make([]Frame, nFrames)
	for i := range ix.Frames {
		fr := &ix.Frames[i]
		for _, v := range []any{&fr.CompOff, &fr.CompSize, &fr.OutOff, &fr.OutSize} {
			if err := binary.Read(br, binary.LittleEndian, v); err != nil {
				return nil, err
			}
		}
	}
	ix.Entries = make([]gzipidx.Entry, nEntries)
	for i := range ix.Entries {
		e, err := gzipidx.ReadEntry(br)
		if err != nil {
			return nil, err
		}
		ix.Entries[i] = e
	}
	return ix, nil
}

func dirOf(path string) string {
	dir := path
	for i := len(dir) - 1; i >= 0; i-- {
		if dir[i] == '/' {
			return dir[:i]
		}
	}
	return "."
}

// --- reading ---

// Reader serves decompressed bytes from an output offset, restarting at the
// frame that holds it.
type Reader struct {
	f  *os.File
	ix *Index
}

// OpenReader opens path with a loaded index.
func OpenReader(path string, ix *Index) (*Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return &Reader{f: f, ix: ix}, nil
}

// Index returns the loaded index.
func (r *Reader) Index() *Index { return r.ix }

// Close releases the file.
func (r *Reader) Close() error { return r.f.Close() }

// ReadAt returns a reader positioned at an output offset. With several frames
// the decompressor starts at the frame holding that offset and walks on through
// the following ones, because a tar member can straddle a frame boundary; a
// single-frame stream starts at the beginning, which is still one pass.
func (r *Reader) ReadAt(outOff int64) (io.ReadCloser, error) {
	if outOff < 0 || outOff > r.ix.TotalOut {
		return nil, fmt.Errorf("offset %d outside the stream (0..%d)", outOff, r.ix.TotalOut)
	}
	idx := r.ix.frameAt(outOff)
	frame := r.ix.Frames[idx]
	dec, err := zstd.NewReader(io.NewSectionReader(r.f, frame.CompOff, frame.CompSize))
	if err != nil {
		return nil, err
	}
	skip := outOff - frame.OutOff
	if skip < 0 {
		skip = 0
	}
	if skip > 0 {
		if _, err := io.CopyN(io.Discard, dec, skip); err != nil {
			dec.Close()
			return nil, err
		}
	}
	return &frameReader{f: r.f, ix: r.ix, idx: idx, dec: dec}, nil
}

// frameReader reads across frames: the decoder is limited to one frame at a
// time, so reaching its end resets it onto the next.
type frameReader struct {
	f   *os.File
	ix  *Index
	idx int
	dec *zstd.Decoder
}

func (f *frameReader) Read(p []byte) (int, error) {
	for {
		n, err := f.dec.Read(p)
		if n > 0 {
			return n, nil
		}
		if err == nil {
			if len(p) == 0 {
				return 0, nil
			}
			continue
		}
		if err != io.EOF {
			return n, err
		}
		if f.idx+1 >= len(f.ix.Frames) {
			return 0, io.EOF
		}
		f.idx++
		next := f.ix.Frames[f.idx]
		if err := f.dec.Reset(io.NewSectionReader(f.f, next.CompOff, next.CompSize)); err != nil {
			return 0, err
		}
	}
}

func (f *frameReader) Close() error {
	f.dec.Close()
	return nil
}
