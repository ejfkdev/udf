package gzipidx

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Magic identifies an index blob.
var Magic = [8]byte{'u', 'd', 'f', 'g', 'z', 'i', 'd', 'x'}

// Version is the index format version.
const Version = 1

// checkpointCtxLen is how many output bytes each checkpoint records for
// verifying a restart. A short context can pass by luck (or because the first
// symbols only touch nearby history), so verification uses a long window.
const checkpointCtxLen = 64 << 10

// Checkpoint is a restart point at a deflate block boundary: decoding the
// stream from BitPos (relative to the deflate stream start) with Window as
// the initial history reproduces the stream from OutPos.
type Checkpoint struct {
	BitPos int64
	OutPos int64
	Window []byte
	Ctx    []byte
}

// Entry is one tar member of the decompressed stream.
type Entry struct {
	Name   string
	OutOff int64
	Size   int64
}

// Index is the persisted result of a scan.
type Index struct {
	FileSize     int64
	DeflateStart int64
	TotalOut     int64
	Checks       []Checkpoint
	Entries      []Entry
	SingleMember bool
}

// BuildOptions tunes a build.
type BuildOptions struct {
	// Checkpoints is the target number of restart points (default 48).
	Checkpoints int
	// CandidateSpacing overrides how often a candidate checkpoint is taken
	// (default 64 MiB of output).
	CandidateSpacing int64
	// NoDirectory skips recording tar member offsets.
	NoDirectory bool
	// Progress, when set, receives the decompressed offset.
	Progress func(int64)
}

// Build scans a gzip file once and returns its index.
func Build(path string, opts BuildOptions) (*Index, error) {
	if opts.Checkpoints <= 0 {
		opts.Checkpoints = 48
	}
	spacing := opts.CandidateSpacing
	if spacing <= 0 {
		spacing = 64 << 20
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}

	br := bufio.NewReaderSize(f, 1<<20)
	headerSize, err := gzipHeaderSize(br)
	if err != nil {
		return nil, err
	}

	ix := &Index{FileSize: st.Size(), DeflateStart: headerSize}
	scanner := &dirScanner{enabled: !opts.NoDirectory}

	var (
		cands    []candidate
		nextCP   int64 = spacing
		crc      uint32
		fileSize = st.Size()
	)

	toSink := func(outPos int64, p []byte) error {
		if n := len(cands); n > 0 {
			c := &cands[n-1]
			if c.need > 0 {
				// The context starts at the checkpoint offset; chunks are not
				// aligned to it, so slice by absolute position.
				from := c.cp.OutPos + int64(len(c.cp.Ctx)) - outPos
				if from < 0 {
					from = 0
				}
				if from < int64(len(p)) {
					take := int64(len(p)) - from
					if take > int64(c.need) {
						take = int64(c.need)
					}
					c.cp.Ctx = append(c.cp.Ctx, p[from:from+take]...)
					c.need -= int(take)
				}
			}
		}
		if scanner.enabled {
			scanner.feed(p)
		}
		crc = crc32.Update(crc, crc32.IEEETable, p)
		if opts.Progress != nil {
			opts.Progress(ix.TotalOut)
		}
		return nil
	}

	src := io.NewSectionReader(f, headerSize, fileSize-headerSize)
	total, err := ScanBlockBoundaries(src, func(bitPos, outPos int64, window func() []byte) bool {
		// A restart point must be byte-aligned: a stored block consumes the
		// rest of its byte, and a bit-shifted view of the stream would move
		// that grid, so byte-aligned boundaries are the only ones that resume
		// both huffman and stored blocks correctly.
		if outPos >= nextCP && bitPos%8 == 0 {
			nextCP = outPos + spacing
			cands = append(cands, candidate{
				cp:   Checkpoint{BitPos: bitPos, OutPos: outPos, Window: window()},
				need: checkpointCtxLen,
			})
		}
		return true
	}, toSink)
	if err != nil {
		return nil, err
	}
	ix.TotalOut = total
	scanner.finish(&ix.Entries)

	// Space the captured candidates evenly over the stream.
	ix.Checks = resampleCandidates(cands, ix.TotalOut, opts.Checkpoints)

	// Verify each checkpoint by restarting the stock decompressor.
	keep := ix.Checks[:0]
	for i := range ix.Checks {
		cp := &ix.Checks[i]
		if cp.len() == 0 {
			continue
		}
		if verifyCheckpoint(path, ix.DeflateStart, cp) {
			keep = append(keep, *cp)
		}
	}
	ix.Checks = keep
	if len(ix.Checks) == 0 {
		return nil, fmt.Errorf("no checkpoint could be verified")
	}

	ix.SingleMember = trailerMatches(f, fileSize, ix.TotalOut, crc)
	return ix, nil
}

// candidate is a checkpoint captured mid-scan whose context is still being
// collected.
type candidate struct {
	cp   Checkpoint
	need int // context bytes still to capture
}

// len reports how many context bytes the checkpoint holds.
func (c *Checkpoint) len() int { return len(c.Ctx) }

func resampleCandidates(cands []candidate, totalOut int64, want int) []Checkpoint {
	if len(cands) == 0 || totalOut <= 0 || want <= 0 {
		return nil
	}
	step := totalOut / int64(want)
	if step < 1<<20 {
		step = 1 << 20
	}
	var out []Checkpoint
	idx := 0
	for target := step; target < totalOut; target += step {
		for idx+1 < len(cands) && cands[idx+1].cp.OutPos <= target {
			idx++
		}
		cp := cands[idx].cp
		if len(out) > 0 && out[len(out)-1].OutPos == cp.OutPos {
			continue
		}
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OutPos < out[j].OutPos })
	return out
}

// verifyCheckpoint restarts the stream at the checkpoint and compares the
// decoded bytes with the recorded context.
func verifyCheckpoint(path string, deflateStart int64, cp *Checkpoint) bool {
	r, err := newRestartReader(path, deflateStart, cp.BitPos, cp.Window)
	if err != nil {
		return false
	}
	defer r.Close()
	got := make([]byte, len(cp.Ctx))
	if _, err := io.ReadFull(r, got); err != nil {
		return false
	}
	return bytes.Equal(got, cp.Ctx)
}

// trailerMatches reports whether the file ends with a gzip trailer that
// matches the decoded content (a single member ending at EOF).
func trailerMatches(f *os.File, fileSize, totalOut int64, crc uint32) bool {
	if fileSize < 8 {
		return false
	}
	var tail [8]byte
	if _, err := f.ReadAt(tail[:], fileSize-8); err != nil {
		return false
	}
	if binary.LittleEndian.Uint32(tail[4:]) != uint32(totalOut) {
		return false
	}
	return binary.LittleEndian.Uint32(tail[:4]) == crc
}

// CheckpointFor returns the last checkpoint at or before outPos, or nil for
// the start of the stream.
func (ix *Index) CheckpointFor(outPos int64) *Checkpoint {
	i := sort.Search(len(ix.Checks), func(i int) bool { return ix.Checks[i].OutPos > outPos })
	if i == 0 {
		return nil
	}
	return &ix.Checks[i-1]
}

// Usable reports whether the index can serve random access.
func (ix *Index) Usable() bool { return ix.SingleMember && len(ix.Checks) > 0 }

// Lookup finds a tar member by exact name.
func (ix *Index) Lookup(name string) (Entry, bool) {
	i := sort.Search(len(ix.Entries), func(i int) bool { return ix.Entries[i].Name >= name })
	if i < len(ix.Entries) && ix.Entries[i].Name == name {
		return ix.Entries[i], true
	}
	return Entry{}, false
}

// --- persistence ---

// Save writes the index in a compact binary form.
func (ix *Index) Save(path string) error {
	tmp, err := os.CreateTemp(dirOf(path), ".idx-*")
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

func dirOf(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[:i]
		}
	}
	return "."
}

func (ix *Index) writeTo(w io.Writer) error {
	if _, err := w.Write(Magic[:]); err != nil {
		return err
	}
	var flags uint32
	if ix.SingleMember {
		flags |= 1
	}
	for _, v := range []any{uint32(Version), ix.FileSize, ix.DeflateStart, ix.TotalOut, flags, uint32(len(ix.Checks)), uint32(len(ix.Entries))} {
		if err := binary.Write(w, binary.LittleEndian, v); err != nil {
			return err
		}
	}
	for i := range ix.Checks {
		c := &ix.Checks[i]
		for _, v := range []any{c.BitPos, c.OutPos} {
			if err := binary.Write(w, binary.LittleEndian, v); err != nil {
				return err
			}
		}
		if err := writeBlob(w, c.Window); err != nil {
			return err
		}
		if err := writeBlob(w, c.Ctx); err != nil {
			return err
		}
	}
	for i := range ix.Entries {
		e := &ix.Entries[i]
		if err := writeString(w, e.Name); err != nil {
			return err
		}
		for _, v := range []any{e.OutOff, e.Size} {
			if err := binary.Write(w, binary.LittleEndian, v); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeString(w io.Writer, s string) error {
	if err := binary.Write(w, binary.LittleEndian, uint32(len(s))); err != nil {
		return err
	}
	_, err := io.WriteString(w, s)
	return err
}

func writeBlob(w io.Writer, b []byte) error {
	if err := binary.Write(w, binary.LittleEndian, uint32(len(b))); err != nil {
		return err
	}
	_, err := w.Write(b)
	return err
}

// Load reads an index written by Save.
func Load(path string) (*Index, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ReadIndex(bufio.NewReaderSize(f, 1<<20))
}

// ReadIndex decodes an index from r.
func ReadIndex(r io.Reader) (*Index, error) {
	var magic [8]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return nil, err
	}
	if magic != Magic {
		return nil, fmt.Errorf("not a gzip index blob")
	}
	ix := &Index{}
	var version, flags, nChecks, nEntries uint32
	for _, v := range []any{&version, &ix.FileSize, &ix.DeflateStart, &ix.TotalOut, &flags, &nChecks, &nEntries} {
		if err := binary.Read(r, binary.LittleEndian, v); err != nil {
			return nil, err
		}
	}
	if version != Version {
		return nil, fmt.Errorf("unsupported index version %d", version)
	}
	ix.SingleMember = flags&1 != 0
	ix.Checks = make([]Checkpoint, nChecks)
	for i := range ix.Checks {
		c := &ix.Checks[i]
		for _, v := range []any{&c.BitPos, &c.OutPos} {
			if err := binary.Read(r, binary.LittleEndian, v); err != nil {
				return nil, err
			}
		}
		win, err := readBlob(r, maxWindow)
		if err != nil {
			return nil, err
		}
		ctx, err := readBlob(r, 1<<20)
		if err != nil {
			return nil, err
		}
		c.Window, c.Ctx = win, ctx
	}
	ix.Entries = make([]Entry, nEntries)
	for i := range ix.Entries {
		e := &ix.Entries[i]
		name, err := readString(r)
		if err != nil {
			return nil, err
		}
		e.Name = name
		for _, v := range []any{&e.OutOff, &e.Size} {
			if err := binary.Read(r, binary.LittleEndian, v); err != nil {
				return nil, err
			}
		}
	}
	return ix, nil
}

func readString(r io.Reader) (string, error) {
	var n uint32
	if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
		return "", err
	}
	if n > 1<<24 {
		return "", fmt.Errorf("implausible string length %d", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", err
	}
	return string(b), nil
}

func readBlob(r io.Reader, max uint32) ([]byte, error) {
	var n uint32
	if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
		return nil, err
	}
	if n > max {
		return nil, fmt.Errorf("implausible blob length %d", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	return b, nil
}

// --- tar directory scanning ---

// dirScanner walks tar headers in the decompressed stream to record member
// offsets. It understands plain, GNU long-name and PAX extended headers.
type dirScanner struct {
	enabled bool
	entries []Entry
	pos     int64
	state   int // 0 header, 1 data, 2 padding
	remain  int64
	padRem  int64
	hdr     [512]byte
	hdrLen  int
	done    bool

	skipEntry bool
	accept    bool
	gnuLong   string
	capture   []byte
}

var paxPathPrefix = []byte("path=")

func (s *dirScanner) feed(p []byte) {
	if !s.enabled {
		return
	}
	for len(p) > 0 {
		switch s.state {
		case 0:
			n := copy(s.hdr[s.hdrLen:], p)
			s.hdrLen += n
			p = p[n:]
			s.pos += int64(n)
			if s.hdrLen < 512 {
				return
			}
			hdr := s.hdr[:]
			s.hdrLen = 0
			if isZeroBlock(hdr) {
				s.done = true
				return
			}
			name, size, typeflag, ok := parseTarHeader(hdr)
			if !ok {
				s.enabled = false
				return
			}
			s.remain = size
			s.padRem = (512 - size%512) % 512
			s.state = 1
			switch typeflag {
			case 'x', 'g', 'L', 'K':
				s.skipEntry = true
				s.accept = false
			default:
				s.skipEntry = false
				s.accept = true
			}
			cur := name
			if s.gnuLong != "" {
				cur = s.gnuLong
				s.gnuLong = ""
			}
			if s.accept {
				s.entries = append(s.entries, Entry{Name: cur, OutOff: s.pos, Size: size})
			}
		case 1:
			n := int64(len(p))
			if n > s.remain {
				n = s.remain
			}
			if s.skipEntry {
				s.capture = append(s.capture, p[:n]...)
			}
			s.remain -= n
			s.pos += n
			p = p[n:]
			if s.remain == 0 {
				s.state = 2
			}
		case 2:
			n := int64(len(p))
			if n > s.padRem {
				n = s.padRem
			}
			s.padRem -= n
			s.pos += n
			p = p[n:]
			if s.padRem == 0 {
				s.finishHeader()
				s.state = 0
			}
		}
	}
}

func (s *dirScanner) finishHeader() {
	if !s.skipEntry {
		s.capture = s.capture[:0]
		return
	}
	data := s.capture
	switch {
	case bytes.HasPrefix(data, paxPathPrefix):
		s.gnuLong = paxValue(data)
	case len(data) > 0 && !bytes.Contains(data, []byte{'='}):
		s.gnuLong = strings.TrimRight(string(data), "\x00")
	}
	s.capture = s.capture[:0]
}

func (s *dirScanner) finish(out *[]Entry) {
	*out = append(*out, s.entries...)
	sort.Slice(*out, func(i, j int) bool { return (*out)[i].Name < (*out)[j].Name })
}

func paxValue(data []byte) string {
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		var line []byte
		if i < 0 {
			line, data = data, nil
		} else {
			line, data = data[:i], data[i+1:]
		}
		if bytes.HasPrefix(line, paxPathPrefix) {
			return string(line[len(paxPathPrefix):])
		}
	}
	return ""
}

func isZeroBlock(hdr []byte) bool {
	for _, b := range hdr {
		if b != 0 {
			return false
		}
	}
	return true
}

// parseTarHeader extracts name, size and typeflag, verifying the checksum.
func parseTarHeader(hdr []byte) (string, int64, byte, bool) {
	size, ok := parseTarNumber(hdr[124:136])
	if !ok {
		return "", 0, 0, false
	}
	typeflag := hdr[156]
	if typeflag == 0 {
		typeflag = '0'
	}
	if !tarChecksumOK(hdr) {
		return "", 0, 0, false
	}
	return cstring(hdr[0:100]), size, typeflag, true
}

func parseTarNumber(b []byte) (int64, bool) {
	if len(b) == 0 {
		return 0, false
	}
	if b[0]&0x80 != 0 {
		var v int64
		for _, c := range b[1:] {
			v = v<<8 | int64(c)
		}
		return v, true
	}
	s := strings.Trim(string(b), " \x00")
	if s == "" {
		return 0, true
	}
	v, err := strconv.ParseInt(s, 8, 64)
	return v, err == nil
}

func tarChecksumOK(hdr []byte) bool {
	s := strings.Trim(string(hdr[148:156]), " \x00")
	if s == "" {
		return false
	}
	want, err := strconv.ParseInt(s, 8, 64)
	if err != nil {
		return false
	}
	var got int64
	for i, b := range hdr {
		if i >= 148 && i < 156 {
			got += int64(' ')
			continue
		}
		got += int64(b)
	}
	return got == want
}

func cstring(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}
