package lvm2

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

const sectorSize = 512

// textMarker begins every LVM2 volume-group metadata snapshot.
const textMarker = `contents = "Text Format Volume Group"`

// ErrNotLVM is returned when the region does not hold an LVM2 physical volume.
var ErrNotLVM = errors.New("not an LVM2 physical volume")

// Extent is one physical byte range of a logical volume, relative to the start
// of the physical volume (the partition holding the PV).
type Extent struct {
	Start int64 // byte offset relative to the PV start
	Size  int64 // bytes
}

// LogicalVolume is a single logical volume mapped onto the physical volume
// that was read. Extents are ordered by offset and non-overlapping. An empty
// Extents slice means the volume could not be mapped (mirror/RAID/multi-PV).
type LogicalVolume struct {
	Name    string
	Size    int64
	Extents []Extent
}

// VG is a parsed LVM2 volume group.
type VG struct {
	Name        string
	Seqno       int64
	ExtentBytes int64 // bytes per physical extent
	PEStart     int64 // byte offset of the first physical extent, relative to the PV start
	PECount     int64 // number of physical extents
	Volumes     []LogicalVolume
}

// Open probes ra at origin (the byte offset of the partition holding the PV)
// for an LVM2 physical volume and returns the parsed volume group. The newest
// metadata snapshot (highest sequence number) wins.
func Open(ra io.ReaderAt, origin int64) (*VG, error) {
	if err := checkLabel(ra, origin); err != nil {
		return nil, err
	}

	// The primary metadata area lives before the first physical extent, whose
	// offset (pe_start) defaults to ~1 MiB. Scan a generous window for the
	// text snapshots.
	const scanSize = 8 << 20
	buf := make([]byte, scanSize)
	n, err := ra.ReadAt(buf, origin)
	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("read LVM2 metadata area: %w", err)
	}
	buf = buf[:n]

	var best *VG
	for _, idx := range findAll(buf, []byte(textMarker)) {
		vg, err := parseMetadata(string(stripNUL(buf[idx:])))
		if err != nil {
			continue
		}
		if best == nil || vg.Seqno > best.Seqno {
			best = vg
		}
	}
	if best == nil {
		return nil, fmt.Errorf("LVM2 metadata not found within the first %d bytes", scanSize)
	}
	return best, nil
}

func checkLabel(ra io.ReaderAt, origin int64) error {
	// The PV label header occupies sector 1 (512 bytes in).
	label := make([]byte, sectorSize)
	if _, err := ra.ReadAt(label, origin+sectorSize); err != nil {
		return ErrNotLVM
	}
	if string(label[0:8]) != "LABELONE" || string(label[24:32]) != "LVM2 001" {
		return ErrNotLVM
	}
	return nil
}

func findAll(haystack, needle []byte) []int {
	var out []int
	base := 0
	for {
		i := bytes.Index(haystack, needle)
		if i < 0 {
			return out
		}
		out = append(out, base+i)
		base += i + len(needle)
		haystack = haystack[i+len(needle):]
	}
}

func stripNUL(b []byte) []byte {
	return bytes.ReplaceAll(b, []byte{0}, nil)
}

// ---- metadata text parser ---------------------------------------------------

// parseMetadata parses one metadata snapshot: the key=value control fields
// followed by a single "vgname {" block.
func parseMetadata(text string) (*VG, error) {
	s := &scanner{data: []byte(text)}
	for {
		if err := s.skipSpaceAndComments(); err != nil {
			return nil, errors.New("LVM2 metadata: missing volume group block")
		}
		key := s.readIdent()
		if s.consumeWithSpace('=') {
			_ = s.readValue() // contents, version, description, ...
			continue
		}
		if s.consumeWithSpace('{') {
			// The key followed by '{' is the volume group block.
			return s.parseVGBody(key)
		}
		return nil, fmt.Errorf("LVM2 metadata: unexpected token %q", key)
	}
}

// consumeWithSpace skips inline whitespace and then, if the next byte is b,
// consumes it too.
func (s *scanner) consumeWithSpace(b byte) bool {
	s.skipInlineSpace()
	if s.peek() == b {
		s.advance()
		return true
	}
	return false
}

func (s *scanner) parseVGBody(name string) (*VG, error) {
	vg := &VG{Name: name}
	for {
		if err := s.skipSpaceAndComments(); err != nil {
			return nil, errors.New("LVM2 metadata: unterminated volume group")
		}
		if s.consume('}') {
			return vg, nil
		}
		key := s.readIdent()
		if s.consumeWithSpace('{') {
			switch key {
			case "physical_volumes":
				if err := s.parsePhysicalVolumes(vg); err != nil {
					return nil, err
				}
			case "logical_volumes":
				if err := s.parseLogicalVolumes(vg); err != nil {
					return nil, err
				}
			default:
				if err := s.skipBlock(); err != nil {
					return nil, err
				}
			}
			continue
		}
		if !s.consumeWithSpace('=') {
			return nil, fmt.Errorf("LVM2 metadata: expected '=' or '{' after %q", key)
		}
		val := s.readValue()
		switch key {
		case "seqno":
			vg.Seqno = parseInt64(val)
		case "extent_size":
			vg.ExtentBytes = parseInt64(val) * sectorSize
		}
	}
}

func (s *scanner) parsePhysicalVolumes(vg *VG) error {
	for {
		if err := s.skipSpaceAndComments(); err != nil {
			return err
		}
		if s.consume('}') {
			return nil
		}
		name := s.readIdent()
		if !s.consumeWithSpace('{') {
			return fmt.Errorf("LVM2 metadata: expected block after %q", name)
		}
		// Capture only the first (and, for our single-PV support, only) PV.
		if vg.PEStart == 0 && vg.PECount == 0 {
			peStart, peCount, err := s.parsePVBody()
			if err != nil {
				return err
			}
			vg.PEStart = peStart * sectorSize
			vg.PECount = peCount
		} else if err := s.skipBlock(); err != nil {
			return err
		}
	}
}

func (s *scanner) parsePVBody() (peStart, peCount int64, err error) {
	for {
		if err := s.skipSpaceAndComments(); err != nil {
			return 0, 0, err
		}
		if s.consume('}') {
			return peStart, peCount, nil
		}
		key := s.readIdent()
		if s.consumeWithSpace('{') {
			if err := s.skipBlock(); err != nil {
				return 0, 0, err
			}
			continue
		}
		if !s.consumeWithSpace('=') {
			return 0, 0, fmt.Errorf("LVM2 metadata: expected '=' after %q", key)
		}
		val := s.readValue()
		switch key {
		case "pe_start":
			peStart = parseInt64(val)
		case "pe_count":
			peCount = parseInt64(val)
		}
	}
}

func (s *scanner) parseLogicalVolumes(vg *VG) error {
	for {
		if err := s.skipSpaceAndComments(); err != nil {
			return err
		}
		if s.consume('}') {
			return nil
		}
		name := s.readIdent()
		if !s.consumeWithSpace('{') {
			return fmt.Errorf("LVM2 metadata: expected block after %q", name)
		}
		lv := s.parseLVBody(vg, name)
		vg.Volumes = append(vg.Volumes, lv)
	}
}

type segment struct {
	stripeOffset int64 // physical-extent offset on pv0
	extentCount  int64
	stripeCount  int
	pvIndex      int // -1 when not mapped to pv0
}

func (s *scanner) parseLVBody(vg *VG, name string) LogicalVolume {
	var segments []segment
	for {
		_ = s.skipSpaceAndComments()
		if s.eof() || s.consume('}') {
			return buildLogicalVolume(vg, name, segments)
		}
		_ = s.readIdent()
		if s.consumeWithSpace('{') {
			segments = append(segments, s.parseSegmentBody())
			continue
		}
		if s.consumeWithSpace('=') {
			_ = s.readValue() // id, status, flags, segment_count, ...
			continue
		}
		// Malformed line; skip defensively.
		s.skipToLineEnd()
	}
}

func (s *scanner) parseSegmentBody() segment {
	seg := segment{pvIndex: 0}
	for {
		_ = s.skipSpaceAndComments()
		if s.eof() || s.consume('}') {
			return seg
		}
		key := s.readIdent()
		if s.consumeWithSpace('{') {
			_ = s.skipBlock()
			continue
		}
		if !s.consumeWithSpace('=') {
			s.skipToLineEnd()
			continue
		}
		val := s.readValue()
		switch key {
		case "start_extent":
			// Logical offset within the volume; not needed to map bytes.
		case "extent_count":
			seg.extentCount = parseInt64(val)
		case "stripe_count":
			seg.stripeCount = int(parseInt64(val))
		case "stripes":
			pv, off, err := parseStripes(val)
			if err == nil {
				seg.pvIndex = pv
				seg.stripeOffset = off
			} else {
				seg.pvIndex = -1
			}
		}
	}
}

// buildLogicalVolume resolves segments into byte extents relative to the PV
// start. A single-PV linear layout (stripes pointing at pv0, one stripe) maps
// each segment's physical extents directly; anything else leaves the volume
// unmappable.
func buildLogicalVolume(vg *VG, name string, segments []segment) LogicalVolume {
	lv := LogicalVolume{Name: name}
	var exts []Extent
	for _, seg := range segments {
		if seg.pvIndex != 0 || seg.stripeCount > 1 {
			return LogicalVolume{Name: name}
		}
		exts = append(exts, Extent{
			Start: vg.PEStart + seg.stripeOffset*vg.ExtentBytes,
			Size:  seg.extentCount * vg.ExtentBytes,
		})
	}
	exts = mergeExtents(exts)
	for _, e := range exts {
		lv.Size += e.Size
	}
	lv.Extents = exts
	return lv
}

func mergeExtents(in []Extent) []Extent {
	if len(in) == 0 {
		return nil
	}
	sort.SliceStable(in, func(i, j int) bool { return in[i].Start < in[j].Start })
	out := []Extent{in[0]}
	for _, e := range in[1:] {
		last := &out[len(out)-1]
		if e.Start <= last.Start+last.Size {
			if end := e.Start + e.Size; end > last.Start+last.Size {
				last.Size = end - last.Start
			}
			continue
		}
		out = append(out, e)
	}
	return out
}

// parseStripes parses a "stripes" list such as ["pv0", 0] into the PV index
// and its physical-extent offset.
func parseStripes(val string) (pv int, offset int64, err error) {
	val = strings.TrimSpace(val)
	val = strings.TrimPrefix(val, "[")
	val = strings.TrimSuffix(val, "]")
	parts := strings.FieldsFunc(val, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '"'
	})
	if len(parts) < 2 {
		return 0, 0, fmt.Errorf("unexpected stripes value %q", val)
	}
	if parts[0] != "pv0" {
		return -1, 0, nil // single-PV mapping required
	}
	offset, err = strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid stripe offset in %q: %w", val, err)
	}
	return 0, offset, nil
}

func parseInt64(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n
}

// ---- scanner ----------------------------------------------------------------

type scanner struct {
	data []byte
	pos  int
}

func (s *scanner) eof() bool { return s.pos >= len(s.data) }

func (s *scanner) peek() byte {
	if s.eof() {
		return 0
	}
	return s.data[s.pos]
}

func (s *scanner) advance() byte {
	c := s.data[s.pos]
	s.pos++
	return c
}

func (s *scanner) consume(b byte) bool {
	if s.peek() == b {
		s.advance()
		return true
	}
	return false
}

// skipSpaceAndComments consumes whitespace and '#' comment lines.
func (s *scanner) skipSpaceAndComments() error {
	for !s.eof() {
		c := s.peek()
		switch {
		case c == '#':
			s.skipToLineEnd()
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			s.advance()
		default:
			return nil
		}
	}
	return io.EOF
}

// skipInlineSpace skips only spaces and tabs.
func (s *scanner) skipInlineSpace() {
	for !s.eof() && (s.peek() == ' ' || s.peek() == '\t') {
		s.advance()
	}
}

func (s *scanner) skipToLineEnd() {
	for !s.eof() && s.peek() != '\n' {
		s.advance()
	}
}

func isIdentByte(c byte) bool {
	return c > 0 && c != ' ' && c != '\t' && c != '\n' && c != '\r' && c != '=' && c != '{' && c != '}'
}

func (s *scanner) readIdent() string {
	start := s.pos
	for !s.eof() && isIdentByte(s.peek()) {
		s.advance()
	}
	return string(s.data[start:s.pos])
}

// readValue reads one key value: a quoted string, an inline [...] list, or a
// bare token. Leading whitespace is skipped.
func (s *scanner) readValue() string {
	_ = s.skipSpaceAndComments()
	if s.eof() {
		return ""
	}
	switch s.peek() {
	case '"':
		s.advance()
		var b strings.Builder
		for !s.eof() {
			c := s.advance()
			if c == '\\' && !s.eof() {
				b.WriteByte(s.advance())
				continue
			}
			if c == '"' {
				return b.String()
			}
			b.WriteByte(c)
		}
		return b.String()
	case '[':
		start := s.pos
		depth := 0
		for !s.eof() {
			c := s.advance()
			switch c {
			case '[':
				depth++
			case ']':
				depth--
				if depth == 0 {
					return strings.TrimSpace(string(s.data[start:s.pos]))
				}
			}
		}
		return strings.TrimSpace(string(s.data[start:s.pos]))
	default:
		start := s.pos
		for !s.eof() && isIdentByte(s.peek()) {
			s.advance()
		}
		return string(s.data[start:s.pos])
	}
}

// skipBlock consumes an entire unknown {...} block, assuming the '{' was
// already consumed.
func (s *scanner) skipBlock() error {
	depth := 1
	for !s.eof() {
		c := s.peek()
		switch c {
		case '{':
			depth++
			s.advance()
		case '}':
			depth--
			s.advance()
			if depth == 0 {
				return nil
			}
		default:
			s.advance()
		}
	}
	return errors.New("LVM2 metadata: unterminated block")
}
