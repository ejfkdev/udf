package archive

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"

	"github.com/ejfkdev/udf/fsview"
)

// Nix Archive (NAR) support — the serialization produced by `nix-store
// --dump` and used throughout the Nix/NixOS build pipeline. Every field,
// including tokens, entry names and file contents, is a length-prefixed "byte
// packet": an 8-byte little-endian length, the bytes, then null padding to an
// 8-byte boundary. The header magic is the packet encoding "nix-archive-1".

const narMagic = "nix-archive-1"

// narReader reads NAR byte packets from an underlying stream.
type narReader struct {
	r *bufio.Reader
}

// packet reads one length-prefixed byte packet, discarding its 8-byte padding.
func (n *narReader) packet() ([]byte, error) {
	var lb [8]byte
	if _, err := io.ReadFull(n.r, lb[:]); err != nil {
		return nil, err
	}
	length := binary.LittleEndian.Uint64(lb[:])
	if length > 1<<32 {
		return nil, fmt.Errorf("NAR field too large: %d", length)
	}
	if length == 0 {
		return nil, nil
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(n.r, buf); err != nil {
		return nil, err
	}
	if pad := (8 - length%8) % 8; pad > 0 {
		if _, err := io.CopyN(io.Discard, n.r, int64(pad)); err != nil {
			return nil, err
		}
	}
	return buf, nil
}

// token reads a packet and returns it as a string (used for names and tokens).
func (n *narReader) token() (string, error) {
	b, err := n.packet()
	return string(b), err
}

func (n *narReader) expect(s string) error {
	tok, err := n.token()
	if err != nil {
		return err
	}
	if tok != s {
		return fmt.Errorf("expected %q in NAR, got %q", s, tok)
	}
	return nil
}

// readLength reads a file content's 8-byte length without consuming the bytes.
func (n *narReader) readLength() (uint64, error) {
	var b [8]byte
	if _, err := io.ReadFull(n.r, b[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(b[:]), nil
}

// skipContent discards size bytes plus the trailing padding.
func (n *narReader) skipContent(size uint64) error {
	if _, err := io.CopyN(io.Discard, n.r, int64(size)); err != nil {
		return err
	}
	if pad := (8 - size%8) % 8; pad > 0 {
		_, err := io.CopyN(io.Discard, n.r, int64(pad))
		return err
	}
	return nil
}

// narWalk carries the per-node callbacks for a traversal. hdr is called for
// every node except the root; content is called for a regular file with its
// uncompressed size, after which the caller must consume exactly that many
// bytes (plus padding) — either streaming or skipping them. Returning stop
// aborts the traversal without consuming those bytes.
type narWalk struct {
	hdr     func(path string, kind fsview.Kind, size int64, link string, exec bool) error
	content func(size uint64) (stop bool, err error)
}

// walk parses one NAR node rooted at dir (empty for the top-level node).
func (n *narReader) walk(dir string, root bool, w narWalk) (bool, error) {
	if err := n.expect("("); err != nil {
		return false, err
	}
	if err := n.expect("type"); err != nil {
		return false, err
	}
	typ, err := n.token()
	if err != nil {
		return false, err
	}

	switch typ {
	case "regular":
		tok, err := n.token()
		if err != nil {
			return false, err
		}
		exec := false
		if tok == "executable" {
			exec = true
			if _, err := n.packet(); err != nil { // empty placeholder string
				return false, err
			}
			tok, err = n.token()
			if err != nil {
				return false, err
			}
		}
		if tok != "contents" {
			return false, fmt.Errorf("expected 'contents' in NAR, got %q", tok)
		}
		size, err := n.readLength()
		if err != nil {
			return false, err
		}
		if !root && w.hdr != nil {
			if err := w.hdr(dir, fsview.KindFile, int64(size), "", exec); err != nil {
				return false, err
			}
		}
		if w.content != nil {
			stop, err := w.content(size)
			if err != nil {
				return false, err
			}
			if stop {
				return true, nil
			}
		}
		return false, n.expect(")")

	case "symlink":
		if err := n.expect("target"); err != nil {
			return false, err
		}
		target, err := n.token()
		if err != nil {
			return false, err
		}
		if !root && w.hdr != nil {
			if err := w.hdr(dir, fsview.KindSymlink, 0, target, false); err != nil {
				return false, err
			}
		}
		return false, n.expect(")")

	case "directory":
		if !root && w.hdr != nil {
			if err := w.hdr(dir, fsview.KindDir, 0, "", false); err != nil {
				return false, err
			}
		}
		for {
			tok, err := n.token()
			if err != nil {
				return false, err
			}
			if tok == ")" {
				return false, nil
			}
			if tok != "entry" {
				return false, fmt.Errorf("expected 'entry' or ')' in NAR, got %q", tok)
			}
			if err := n.expect("("); err != nil {
				return false, err
			}
			if err := n.expect("name"); err != nil {
				return false, err
			}
			name, err := n.token()
			if err != nil {
				return false, err
			}
			if err := n.expect("node"); err != nil {
				return false, err
			}
			child := name
			if dir != "" {
				child = dir + "/" + name
			}
			stop, err := n.walk(child, false, w)
			if err != nil {
				return false, err
			}
			if stop {
				return true, nil
			}
			if err := n.expect(")"); err != nil {
				return false, err
			}
		}

	default:
		return false, fmt.Errorf("unknown NAR node type %q", typ)
	}
}

type narArchive struct {
	path string
}

func (a *narArchive) open() (*os.File, *narReader, func(), error) {
	f, err := os.Open(a.path)
	if err != nil {
		return nil, nil, nil, err
	}
	n := &narReader{r: bufio.NewReader(f)}
	m, err := n.packet()
	if err != nil || string(m) != narMagic {
		_ = f.Close()
		return nil, nil, nil, fmt.Errorf("not a NAR archive")
	}
	return f, n, func() { _ = f.Close() }, nil
}

func (a *narArchive) List() ([]Entry, error) {
	_, n, clean, err := a.open()
	if err != nil {
		return nil, err
	}
	defer clean()

	var entries []Entry
	_, err = n.walk("", true, narWalk{
		hdr: func(p string, k fsview.Kind, size int64, link string, exec bool) error {
			mode := int64(0o644)
			switch k {
			case fsview.KindDir:
				mode = 0o755
			case fsview.KindSymlink:
				mode = 0o777
			default:
				if exec {
					mode = 0o755
				}
			}
			entries = append(entries, Entry{Name: p, Size: size, Kind: k, Mode: mode, Linkname: link})
			return nil
		},
		content: func(size uint64) (bool, error) {
			return false, n.skipContent(size)
		},
	})
	return entries, err
}

func (a *narArchive) Open(name string) (io.ReadCloser, int64, error) {
	f, n, clean, err := a.open()
	if err != nil {
		return nil, 0, err
	}

	var current string
	var foundSize int64
	var found bool
	_, werr := n.walk("", true, narWalk{
		hdr: func(p string, k fsview.Kind, size int64, link string, exec bool) error {
			current = p
			return nil
		},
		content: func(size uint64) (bool, error) {
			if current == name && !found {
				found = true
				foundSize = int64(size)
				return true, nil // stop before consuming the content
			}
			return false, n.skipContent(size)
		},
	})
	if found && werr == nil {
		return &narEntryReadCloser{Reader: io.LimitReader(n.r, foundSize), f: f}, foundSize, nil
	}
	clean()
	return nil, 0, fmt.Errorf("entry %s not found in archive", name)
}

type narEntryReadCloser struct {
	io.Reader
	f *os.File
}

func (r *narEntryReadCloser) Close() error { return r.f.Close() }
