package gzipidx

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"runtime"
	"sync"
)

// A docker-save or OCI archive is a tar whose members are themselves tars (the
// layers). Indexing such an archive already walks every member header, and the
// layer directories are wanted immediately afterwards — to list or extract the
// merged rootfs — but the only other way to them is to decompress every layer
// again through the index. NestedOptions captures the header bytes of selected
// members during the scan, so those layers are parsed from bytes that were
// already in hand, with file contents skipped rather than decoded a second
// time.
type NestedOptions struct {
	// Members decides whether a tar member is parsed as a nested tar stream.
	// It is called once per member with the member's name, size and the first
	// 512 bytes of its payload (nil when the member is shorter than a block).
	// It must be cheap: it runs inside the scan.
	Members func(name string, size int64, head []byte) bool
	// Headers is called once per captured member, after the scan and in member
	// order, with the headers of its nested tar stream. Members that were not
	// selected, or whose stream could not be replayed faithfully (PAX size
	// overrides, sparse files, a broken header), are simply absent — the
	// caller reads those through the index instead.
	Headers func(name string, headers []*tar.Header) error
	// Budget caps the header bytes kept in memory across all captured members.
	// Zero means 256 MiB.
	Budget int64
	// Workers bounds how many captured members are parsed at once; zero means
	// one per core.
	Workers int
}

func (o *NestedOptions) budget() int64 {
	if o.Budget > 0 {
		return o.Budget
	}
	return 256 << 20
}

// LooksLikeTarHeader reports whether a 512-byte block is a plausible tar
// member header (its checksum matches), which is how callers recognise a
// member of an outer tar that is itself a tar stream.
func LooksLikeTarHeader(block []byte) bool {
	return len(block) == 512 && tarChecksumOK(block)
}

// captureRun is one piece of a replayed member: data bytes served from the
// capture buffer, followed by hole bytes recorded but not copied.
type captureRun struct {
	off  int
	n    int
	hole int64
}

// nestedCapture records what replaying one tar member needs: its 512-byte
// header blocks and the bodies of extension members that carry header data
// (PAX records, GNU long names). The bodies of file members become holes, so
// capturing a multi-gigabyte layer costs only its headers.
type nestedCapture struct {
	name  string
	runs  []captureRun
	data  []byte
	abort bool
	done  bool

	// walking state over the member's payload
	state  int // 0 header, 1 extension body, 2 extension padding, 3 file body
	remain int64
	padRem int64
	hdr    [512]byte
	hdrLen int
}

// The replayed stream is a series of runs, each holding captured bytes
// followed by a hole: a member header block, then the hole its body and
// padding occupy, then the next header block, and so on. Extension members
// (PAX records, GNU long names) are the exception — their bodies are captured
// as data because archive/tar reads them.
func (c *nestedCapture) feed(p []byte) {
	if c.done || c.abort {
		return
	}
	for len(p) > 0 {
		switch c.state {
		case 0:
			n := copy(c.hdr[c.hdrLen:], p)
			c.hdrLen += n
			p = p[n:]
			if c.hdrLen < 512 {
				return
			}
			hdr := c.hdr[:]
			c.hdrLen = 0
			if isZeroBlock(hdr) {
				// The end-of-archive marker: replay it so the replayed stream
				// terminates the same way, then stop.
				c.appendData(hdr)
				c.done = true
				return
			}
			_, size, typeflag, ok := parseTarHeader(hdr)
			if !ok {
				c.abort = true
				return
			}
			c.appendData(hdr)
			pad := (512 - size%512) % 512
			switch typeflag {
			case 'x', 'g', 'L', 'K':
				c.state = 1
				c.remain = size
				c.padRem = pad
			case 'S':
				// GNU sparse files store a different number of physical bytes
				// than the size field declares, so holes cannot be computed.
				c.abort = true
				return
			default:
				c.runs[len(c.runs)-1].hole = size + pad
				c.state = 3
				c.remain = size + pad
			}
		case 1:
			n := int64(len(p))
			if n > c.remain {
				n = c.remain
			}
			if n > 0 {
				body := p[:n]
				if bytes.Contains(body, paxSizePrefix) ||
					bytes.HasPrefix(body, []byte("size=")) ||
					bytes.Contains(body, paxSparsePrefix) {
					// A size override or GNU sparse map would make the holes
					// recorded here wrong.
					c.abort = true
					return
				}
				c.appendData(body)
				c.runs[len(c.runs)-1].hole = c.padRem
			}
			c.remain -= n
			p = p[n:]
			if c.remain == 0 {
				c.state = 2
			}
		case 2:
			n := int64(len(p))
			if n > c.padRem {
				n = c.padRem
			}
			c.padRem -= n
			p = p[n:]
			if c.padRem == 0 {
				c.state = 0
			}
		case 3:
			n := int64(len(p))
			if n > c.remain {
				n = c.remain
			}
			c.remain -= n
			p = p[n:]
			if c.remain == 0 {
				c.state = 0
			}
		}
	}
}

// appendData adds a run of captured bytes; the run's hole is filled in when the
// member's body is accounted for.
func (c *nestedCapture) appendData(p []byte) {
	c.runs = append(c.runs, captureRun{off: len(c.data), n: len(p)})
	c.data = append(c.data, p...)
}

// PAX records read "<len> key=value\n", so a key is preceded by a space (or
// starts the body). Matching a little too eagerly is fine — it only makes a
// capture fall back to the index.
var paxSizePrefix = []byte(" size=")

// GNU sparse maps are announced under keys with this prefix, and change how the
// body is laid out.
var paxSparsePrefix = []byte("GNU.sparse.")

// replayReader serves a captured tar stream: data runs come from the capture
// buffer, holes read as zeros. archive/tar skips the body of a member it does
// not read by seeking past it, which this reader supports, so a file body is
// never copied nor materialised — only the padding (a few hundred bytes per
// member) is read out.
type replayReader struct {
	data []byte
	runs []captureRun
	i    int
	off  int64
	hole int64
	pos  int64
}

func (r *replayReader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) && r.i < len(r.runs) {
		run := &r.runs[r.i]
		if r.off < int64(run.n) {
			k := copy(p[n:], r.data[run.off+int(r.off):run.off+run.n])
			r.off += int64(k)
			r.pos += int64(k)
			n += k
			continue
		}
		if r.hole < run.hole {
			k := int64(len(p) - n)
			if k > run.hole-r.hole {
				k = run.hole - r.hole
			}
			for i := int64(0); i < k; i++ {
				p[n+int(i)] = 0
			}
			r.hole += k
			r.pos += k
			n += int(k)
			continue
		}
		r.i++
		r.off, r.hole = 0, 0
	}
	if n == 0 {
		return 0, io.EOF
	}
	return n, nil
}

func (r *replayReader) Seek(offset int64, whence int) (int64, error) {
	var target int64
	switch whence {
	case io.SeekStart:
		target = offset
	case io.SeekCurrent:
		target = r.pos + offset
	case io.SeekEnd:
		target = r.length() + offset
	default:
		return r.pos, errors.New("invalid whence")
	}
	if target < 0 {
		return r.pos, errors.New("negative position")
	}
	if target < r.pos {
		r.i, r.off, r.hole, r.pos = 0, 0, 0, 0
	}
	for r.pos < target {
		if r.i >= len(r.runs) {
			break
		}
		run := &r.runs[r.i]
		step := int64(run.n) - r.off
		if left := target - r.pos; left < step {
			step = left
		}
		if step > 0 {
			r.off += step
			r.pos += step
			continue
		}
		step = run.hole - r.hole
		if left := target - r.pos; left < step {
			step = left
		}
		r.hole += step
		r.pos += step
		if r.hole >= run.hole && r.off >= int64(run.n) {
			r.i++
			r.off, r.hole = 0, 0
		}
	}
	return r.pos, nil
}

func (r *replayReader) length() int64 {
	var n int64
	for _, run := range r.runs {
		n += int64(run.n) + run.hole
	}
	return n
}

// parseCapture replays a captured member through archive/tar, which applies the
// same PAX and GNU extension semantics the layer was written with.
func parseCapture(c *nestedCapture) ([]*tar.Header, bool) {
	if c.abort {
		return nil, false
	}
	tr := tar.NewReader(&replayReader{data: c.data, runs: c.runs})
	var headers []*tar.Header
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return headers, true
		}
		if err != nil {
			return nil, false
		}
		headers = append(headers, hdr)
	}
}

// reportNested parses the captures taken during a scan and hands the headers to
// the caller, in member order. Parsing runs in parallel: the captures are
// independent and the caller is otherwise idle at this point.
func reportNested(scanner *dirScanner, opts *NestedOptions) error {
	caps := make([]*nestedCapture, 0, len(scanner.caps))
	for _, c := range scanner.caps {
		if !c.abort && len(c.runs) > 0 {
			caps = append(caps, c)
		}
	}
	if len(caps) == 0 {
		return nil
	}

	parsed := make([][]*tar.Header, len(caps))
	ok := make([]bool, len(caps))
	workers := opts.Workers
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	if workers > len(caps) {
		workers = len(caps)
	}
	if workers < 1 {
		workers = 1
	}
	var wg sync.WaitGroup
	var next int64
	var mu sync.Mutex
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				i := int(next)
				next++
				mu.Unlock()
				if i >= len(caps) {
					return
				}
				parsed[i], ok[i] = parseCapture(caps[i])
			}
		}()
	}
	wg.Wait()

	for i, c := range caps {
		if !ok[i] {
			continue
		}
		if err := opts.Headers(c.name, parsed[i]); err != nil {
			return err
		}
	}
	return nil
}
