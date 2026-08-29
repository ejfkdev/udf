package qcow

import (
	"bytes"
	"compress/zlib"
	"container/list"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
)

// QCOW version 1 constants. All on-disk fields are big-endian.
const (
	qcowMagic           = 0x514649FB // "QFI\xfb"
	qcowVersion         = 1
	qcowCryptNone       = 0
	qcowCryptAES        = 1
	qcowOflagCompressed = uint64(1) << 63
)

// maxL1Entries bounds the L1 table size derived from the image size so a
// malformed header cannot demand an unbounded allocation.
const maxL1Entries = 1 << 22

// Disk is a read-only QCOW version 1 image. It implements io.ReaderAt.
type Disk struct {
	ra                io.ReaderAt
	clusterSize       int64
	clusterBits       int
	l2Bits            int
	l2Size            int64
	shift             int // clusterBits + l2Bits
	diskSize          int64
	clusterOffsetMask uint64
	l1                []uint64 // L2 table offsets in bytes, 0 = unallocated

	mu      sync.Mutex
	l2LRU   *list.List
	l2Max   int
	l2Cache map[uint64]*list.Element
}

type l2Entry struct {
	offset uint64
	table  []uint64
}

// Open parses the QCOW v1 header and L1 table at the start of ra.
func Open(ra io.ReaderAt) (*Disk, error) {
	var hdr [48]byte
	if _, err := ra.ReadAt(hdr[:], 0); err != nil {
		return nil, fmt.Errorf("read qcow header: %w", err)
	}
	be := binary.BigEndian
	if be.Uint32(hdr[0:4]) != qcowMagic || be.Uint32(hdr[4:8]) != qcowVersion {
		return nil, errors.New("not a QCOW version 1 image")
	}

	clusterBits := int(hdr[32])
	if clusterBits < 9 {
		return nil, fmt.Errorf("invalid QCOW cluster_bits %d", clusterBits)
	}
	if crypt := be.Uint32(hdr[36:40]); crypt != qcowCryptNone {
		return nil, errors.New("encrypted QCOW images are not supported")
	}
	if be.Uint32(hdr[16:20]) != 0 {
		return nil, errors.New("QCOW images with a backing file are not supported")
	}

	d := &Disk{ra: ra}
	d.clusterBits = clusterBits
	d.clusterSize = 1 << clusterBits
	d.l2Bits = int(hdr[33])
	d.l2Size = 1 << d.l2Bits
	d.shift = clusterBits + d.l2Bits
	d.diskSize = int64(be.Uint64(hdr[24:32]))
	d.clusterOffsetMask = (uint64(1) << (63 - clusterBits)) - 1
	if d.diskSize <= 0 {
		return nil, errors.New("invalid QCOW image size")
	}

	l1Size := (d.diskSize + (int64(1) << d.shift) - 1) >> d.shift
	if l1Size > maxL1Entries {
		return nil, fmt.Errorf("QCOW L1 table too large (%d entries)", l1Size)
	}
	l1Offset := be.Uint64(hdr[40:48])
	d.l1 = make([]uint64, l1Size)
	l1Raw := make([]byte, l1Size*8)
	for i := int64(0); i < l1Size; i++ {
		if _, err := ra.ReadAt(l1Raw[i*8:i*8+8], int64(l1Offset)+i*8); err != nil {
			return nil, fmt.Errorf("read QCOW L1 table: %w", err)
		}
		d.l1[i] = be.Uint64(l1Raw[i*8 : i*8+8])
	}

	d.l2LRU = list.New()
	d.l2Cache = make(map[uint64]*list.Element)
	d.l2Max = 512
	return d, nil
}

// Size returns the virtual disk size in bytes.
func (d *Disk) Size() int64 { return d.diskSize }

// Close releases no resources; the caller owns ra.
func (d *Disk) Close() error { return nil }

// l2Table returns the L2 table stored at the given file offset, caching it in
// a bounded LRU.
func (d *Disk) l2Table(offset uint64) ([]uint64, error) {
	d.mu.Lock()
	if e, ok := d.l2Cache[offset]; ok {
		d.l2LRU.MoveToFront(e)
		t := e.Value.(*l2Entry).table
		d.mu.Unlock()
		return t, nil
	}
	d.mu.Unlock()

	raw := make([]byte, d.l2Size*8)
	if _, err := d.ra.ReadAt(raw, int64(offset)); err != nil {
		return nil, fmt.Errorf("read QCOW L2 table at %d: %w", offset, err)
	}
	be := binary.BigEndian
	tab := make([]uint64, d.l2Size)
	for i := range tab {
		tab[i] = be.Uint64(raw[i*8 : i*8+8])
	}

	d.mu.Lock()
	e := d.l2LRU.PushFront(&l2Entry{offset: offset, table: tab})
	d.l2Cache[offset] = e
	for d.l2LRU.Len() > d.l2Max {
		if last := d.l2LRU.Back(); last != nil {
			d.l2LRU.Remove(last)
			delete(d.l2Cache, last.Value.(*l2Entry).offset)
		}
	}
	d.mu.Unlock()
	return tab, nil
}

// decompressCluster reads and inflates a compressed data cluster.
func (d *Disk) decompressCluster(coffset uint64, csize int64) ([]byte, error) {
	comp := make([]byte, csize)
	if _, err := d.ra.ReadAt(comp, int64(coffset)); err != nil && err != io.EOF {
		return nil, fmt.Errorf("read compressed QCOW cluster: %w", err)
	}
	zr, err := zlib.NewReader(bytes.NewReader(comp))
	if err != nil {
		return nil, fmt.Errorf("open compressed QCOW cluster: %w", err)
	}
	defer zr.Close()
	out := make([]byte, d.clusterSize)
	if _, err := io.ReadFull(zr, out); err != nil {
		return nil, fmt.Errorf("inflate QCOW cluster: %w", err)
	}
	return out, nil
}

// ReadAt reads len(p) bytes at off, matching io.ReaderAt semantics.
func (d *Disk) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("negative offset")
	}
	if off >= d.diskSize {
		return 0, io.EOF
	}
	if int64(len(p)) > d.diskSize-off {
		p = p[:d.diskSize-off]
	}

	clusterSize := d.clusterSize
	total := 0
	for len(p) > 0 {
		cluster := off / clusterSize
		inCluster := off % clusterSize
		n := int64(len(p))
		if clusterSize-inCluster < n {
			n = clusterSize - inCluster
		}

		allocated := false
		compressed := false
		var dataOff int64
		var full []byte

		if l1Index := int(off >> uint(d.shift)); l1Index < len(d.l1) {
			if l2Offset := d.l1[l1Index]; l2Offset != 0 {
				l2, err := d.l2Table(l2Offset)
				if err != nil {
					return total, err
				}
				if l2Index := int(uint64(cluster) & uint64(d.l2Size-1)); l2Index < len(l2) {
					switch e := l2[l2Index]; {
					case e == 0: // unallocated -> zeros
					case e&qcowOflagCompressed != 0:
						coffset := e & d.clusterOffsetMask
						csize := int64((e >> uint(63-d.clusterBits)) & uint64(clusterSize-1))
						full, err = d.decompressCluster(coffset, csize)
						if err != nil {
							return total, err
						}
						allocated = true
						compressed = true
					default:
						dataOff = int64(e) + inCluster
						allocated = true
					}
				}
			}
		}

		switch {
		case compressed:
			copy(p[:n], full[inCluster:inCluster+n])
		case allocated:
			if _, err := d.ra.ReadAt(p[:n], dataOff); err != nil && err != io.EOF {
				return total, err
			}
		default:
			clear(p[:n])
		}

		p = p[n:]
		off += n
		total += int(n)
	}
	if total == 0 {
		return 0, io.EOF
	}
	return total, nil
}
