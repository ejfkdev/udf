package qed

import (
	"container/list"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"sync"
)

// qedMagic is the QED\0 signature read as a little-endian uint32: the four
// on-disk bytes are 'Q', 'E', 'D', 0x00.
const qedMagic = 0x00444551

// Feature bits in the header.features field.
const (
	featureBackingFile    = 0x01
	featureNeedCheck      = 0x02
	featureNoBackingProbe = 0x04
)

const (
	minClusterSize = 4 * 1024
	maxClusterSize = 64 * 1024 * 1024
	minTableSize   = 1
	maxTableSize   = 16
)

// A two-level page table maps logical cluster numbers to physical cluster
// offsets. l2Entry is stored in the LRU cache.
type l2Entry struct {
	offset uint64
	table  []uint64
}

// Disk is a read-only QED (QEMU Enhanced Disk) image. It implements
// io.ReaderAt. All on-disk fields are little-endian, unlike qcow2.
type Disk struct {
	ra          io.ReaderAt
	clusterSize uint32 // bytes, power of two
	tableSize   uint32 // L1/L2 table size, in clusters
	tableBytes  int64  // tableSize * clusterSize
	tableNelems int64  // entries per table (tableBytes / 8)
	clusterBits int
	l1Shift     int
	l2Mask      uint64
	diskSize    int64
	l1          []uint64 // L1 table: L2 table offsets in bytes, 0 = unallocated

	mu      sync.Mutex
	l2LRU   *list.List
	l2Max   int
	l2Cache map[uint64]*list.Element
}

// Open parses the QED header and L1 table at the start of ra and returns a
// ready-to-read Disk.
func Open(ra io.ReaderAt) (*Disk, error) {
	var hdr [512]byte
	if _, err := ra.ReadAt(hdr[:], 0); err != nil {
		return nil, fmt.Errorf("read qed header: %w", err)
	}
	le := binary.LittleEndian

	if le.Uint32(hdr[0:4]) != qedMagic {
		return nil, errors.New("not a QED image")
	}

	d := &Disk{ra: ra}
	d.clusterSize = le.Uint32(hdr[4:8])
	d.tableSize = le.Uint32(hdr[8:12])
	headerSize := le.Uint32(hdr[12:16])
	features := le.Uint64(hdr[16:24])
	d.diskSize = int64(le.Uint64(hdr[48:56]))

	if d.clusterSize < minClusterSize || d.clusterSize > maxClusterSize {
		return nil, fmt.Errorf("invalid QED cluster size %d", d.clusterSize)
	}
	if d.clusterSize&(d.clusterSize-1) != 0 {
		return nil, fmt.Errorf("QED cluster size %d is not a power of two", d.clusterSize)
	}
	if d.tableSize < minTableSize || d.tableSize > maxTableSize {
		return nil, fmt.Errorf("invalid QED table size %d clusters", d.tableSize)
	}
	if d.tableSize&(d.tableSize-1) != 0 {
		return nil, fmt.Errorf("QED table size %d is not a power of two", d.tableSize)
	}
	if headerSize < 1 {
		return nil, errors.New("invalid QED header size 0")
	}
	if features&featureBackingFile != 0 {
		return nil, errors.New("QED image with a backing file is not supported")
	}

	d.tableBytes = int64(d.tableSize) * int64(d.clusterSize)
	d.tableNelems = d.tableBytes / 8
	d.clusterBits = bits.TrailingZeros32(d.clusterSize)
	tableBits := bits.TrailingZeros64(uint64(d.tableNelems))
	d.l1Shift = d.clusterBits + tableBits
	d.l2Mask = uint64(d.tableNelems - 1)

	l1Offset := le.Uint64(hdr[40:48])
	if l1Offset == 0 {
		return nil, errors.New("invalid QED L1 table offset 0")
	}

	l1 := make([]uint64, d.tableNelems)
	l1Raw := make([]byte, d.tableBytes)
	if _, err := ra.ReadAt(l1Raw, int64(l1Offset)); err != nil {
		return nil, fmt.Errorf("read QED L1 table: %w", err)
	}
	for i := range l1 {
		l1[i] = le.Uint64(l1Raw[i*8 : i*8+8])
	}
	d.l1 = l1

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
// a small LRU. L2 tables are read once and reused across the many small reads
// that filesystem parsing performs.
func (d *Disk) l2Table(offset uint64) ([]uint64, error) {
	d.mu.Lock()
	if e, ok := d.l2Cache[offset]; ok {
		d.l2LRU.MoveToFront(e)
		t := e.Value.(*l2Entry).table
		d.mu.Unlock()
		return t, nil
	}
	d.mu.Unlock()

	raw := make([]byte, d.tableBytes)
	if _, err := d.ra.ReadAt(raw, int64(offset)); err != nil {
		return nil, fmt.Errorf("read QED L2 table at %d: %w", offset, err)
	}
	le := binary.LittleEndian
	tab := make([]uint64, d.tableNelems)
	for i := range tab {
		tab[i] = le.Uint64(raw[i*8 : i*8+8])
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

	clusterSize := int64(d.clusterSize)
	total := 0
	for len(p) > 0 {
		cluster := off / clusterSize
		inCluster := off % clusterSize
		n := int64(len(p))
		if clusterSize-inCluster < n {
			n = clusterSize - inCluster
		}

		allocated := false
		var dataOff int64
		if l1Index := int(off >> uint(d.l1Shift)); l1Index < len(d.l1) {
			if l2Offset := d.l1[l1Index]; l2Offset != 0 {
				l2, err := d.l2Table(l2Offset)
				if err != nil {
					return total, err
				}
				if l2Index := int(uint64(cluster) & d.l2Mask); l2Index < len(l2) {
					switch e := l2[l2Index]; {
					case e == 0: // unallocated -> zeros
					case e == 1: // zero cluster -> zeros
					default:
						dataOff = int64(e) + inCluster
						allocated = true
					}
				}
			}
		}

		if allocated {
			if _, err := d.ra.ReadAt(p[:n], dataOff); err != nil && err != io.EOF {
				return total, err
			}
		} else {
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
