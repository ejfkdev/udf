package vdi

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const vdiSignature = 0xbeda107f

const (
	unallocated = 0xffffffff
	discarded   = 0xfffffffe
)

// Disk is a read-only VDI image. It implements io.ReaderAt.
type Disk struct {
	ra         io.ReaderAt
	blockSize  uint32
	diskSize   int64
	offsetData uint32
	bmap       []uint32
}

// Open parses the VDI header and block map at the start of ra.
func Open(ra io.ReaderAt) (*Disk, error) {
	hdr := make([]byte, 512)
	if _, err := ra.ReadAt(hdr, 0); err != nil {
		return nil, fmt.Errorf("read vdi header: %w", err)
	}
	le := binary.LittleEndian
	if le.Uint32(hdr[0x40:0x44]) != vdiSignature {
		return nil, errors.New("not a VDI image")
	}

	blockExtra := le.Uint32(hdr[0x17c:0x180])
	if blockExtra != 0 {
		return nil, fmt.Errorf("compressed VDI blocks (block_extra=%d) are not supported", blockExtra)
	}

	blockSize := le.Uint32(hdr[0x178:0x17c])
	if blockSize == 0 {
		return nil, errors.New("invalid VDI block size 0")
	}
	blocksInImage := le.Uint32(hdr[0x180:0x184])
	offsetBmap := int64(le.Uint32(hdr[0x154:0x158]))

	bmapBytes := int64(blocksInImage) * 4
	bmapRaw := make([]byte, bmapBytes)
	if _, err := ra.ReadAt(bmapRaw, offsetBmap); err != nil {
		return nil, fmt.Errorf("read vdi block map: %w", err)
	}
	bmap := make([]uint32, blocksInImage)
	for i := range bmap {
		bmap[i] = le.Uint32(bmapRaw[i*4 : i*4+4])
	}

	return &Disk{
		ra:         ra,
		blockSize:  blockSize,
		diskSize:   int64(le.Uint64(hdr[0x170:0x178])),
		offsetData: le.Uint32(hdr[0x158:0x15c]),
		bmap:       bmap,
	}, nil
}

// Size returns the virtual disk size in bytes.
func (d *Disk) Size() int64 { return d.diskSize }

// Close releases nothing; the caller owns ra.
func (d *Disk) Close() error { return nil }

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

	blockSize := int64(d.blockSize)
	total := 0
	for len(p) > 0 {
		block := off / blockSize
		inBlock := off % blockSize
		n := int64(len(p))
		if blockSize-inBlock < n {
			n = blockSize - inBlock
		}

		var entry uint32 = unallocated
		if block < int64(len(d.bmap)) {
			entry = d.bmap[block]
		}

		if entry >= discarded {
			clear(p[:n])
		} else {
			dataOff := int64(d.offsetData) + int64(entry)*blockSize + inBlock
			if _, err := d.ra.ReadAt(p[:n], dataOff); err != nil {
				return total, err
			}
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
