package parallels

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const sectorSize = 512

const (
	magicPlain = "WithoutFreeSpace"
	magicExt   = "WithouFreSpacExt"
	headerVer  = 2
	headerSize = 64
)

// Disk is a read-only Parallels disk image. It implements io.ReaderAt.
type Disk struct {
	ra       io.ReaderAt
	tracks   uint32 // sectors per cluster
	offMult  uint32 // bat entry -> sector multiplier
	batSize  uint32
	bat      []uint32
	diskSize int64
}

// Open parses the header and block-allocation table at the start of ra.
func Open(ra io.ReaderAt) (*Disk, error) {
	hdr := make([]byte, headerSize)
	if _, err := ra.ReadAt(hdr, 0); err != nil {
		return nil, fmt.Errorf("read parallels header: %w", err)
	}
	le := binary.LittleEndian

	var d Disk
	switch magic := string(hdr[0:16]); magic {
	case magicPlain:
		d.offMult = 1
		d.diskSize = int64(uint32(le.Uint64(hdr[36:44]))) * sectorSize
	case magicExt:
		d.tracks = le.Uint32(hdr[28:32])
		d.offMult = d.tracks
		d.diskSize = int64(le.Uint64(hdr[36:44])) * sectorSize
	default:
		return nil, fmt.Errorf("not a Parallels disk image (magic %q)", magic)
	}

	if le.Uint32(hdr[16:20]) != headerVer {
		return nil, fmt.Errorf("unsupported Parallels version %d", le.Uint32(hdr[16:20]))
	}

	d.tracks = le.Uint32(hdr[28:32])
	d.batSize = le.Uint32(hdr[32:36])

	if d.tracks == 0 {
		return nil, errors.New("invalid Parallels tracks 0")
	}

	d.bat = make([]uint32, d.batSize)
	buf := make([]byte, d.batSize*4)
	if _, err := ra.ReadAt(buf, headerSize); err != nil {
		return nil, fmt.Errorf("read parallels bat: %w", err)
	}
	for i := range d.bat {
		d.bat[i] = le.Uint32(buf[i*4 : i*4+4])
	}

	d.ra = ra
	return &d, nil
}

// Size returns the virtual disk size in bytes.
func (d *Disk) Size() int64 { return d.diskSize }

// Close releases no resources; the caller owns ra.
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

	tracks := int64(d.tracks)
	total := 0
	for len(p) > 0 {
		sector := off / sectorSize
		inSector := off % sectorSize
		cluster := sector / tracks
		inCluster := sector % tracks

		n := int64(sectorSize - inSector)
		if int64(len(p)) < n {
			n = int64(len(p))
		}

		var bat uint32
		if cluster < int64(d.batSize) {
			bat = d.bat[cluster]
		}
		if bat == 0 {
			clear(p[:n])
		} else {
			// bat entries are absolute sector numbers divided by off_multiplier
			// (they already include the data-area offset).
			hostSector := int64(bat)*int64(d.offMult) + inCluster
			if _, err := d.ra.ReadAt(p[:n], hostSector*sectorSize+inSector); err != nil {
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
