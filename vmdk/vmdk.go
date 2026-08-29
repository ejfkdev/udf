// Package vmdk implements read-only access to VMware Virtual Disk (VMDK)
// sparse extents — both monolithicSparse (flat or deflate-compressed) and
// streamOptimized. It builds the grain directory / grain table index once and
// then exposes the virtual disk as a random-access byte stream, mirroring the
// layout described in the VMware VMDK 5.0 specification.
package vmdk

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const sectorSize = 512

const (
	sparseMagic  = 0x564d444b // "VMDK"
	flagCompress = 1 << 16
	flagEmbedLBA = 1 << 17
)

// Disk a read-only VMDK sparse extent. It implements io.ReaderAt.
type Disk struct {
	ra            io.ReaderAt
	size          int64 // virtual size in bytes
	grainBytes    int64 // bytes per grain
	grainSectors  int64 // sectors per grain
	lastGrain     int64 // index of the final (possibly short) grain
	lastGrainSize int64 // bytes in the final grain
	compressed    bool
	embedLBA      bool
	gt            []uint32 // grain index -> absolute sector, 0=unallocated, 1=zero
}

// Open parses the sparse header and grain-table index at the start of ra.
func Open(ra io.ReaderAt) (*Disk, error) {
	hdr := make([]byte, sectorSize)
	if _, err := ra.ReadAt(hdr, 0); err != nil {
		return nil, fmt.Errorf("read vmdk header: %w", err)
	}
	le := binary.LittleEndian
	if le.Uint32(hdr[0:4]) != sparseMagic {
		return nil, errors.New("not a VMDK sparse extent")
	}
	flags := le.Uint32(hdr[8:12])
	capacity := int64(le.Uint64(hdr[12:20]))
	grainSectors := int64(le.Uint64(hdr[20:28]))
	numGTEsPerGT := int64(le.Uint32(hdr[44:48]))
	gdOffset := int64(le.Uint64(hdr[56:64]))

	if grainSectors < 1 || grainSectors > 128 || grainSectors&(grainSectors-1) != 0 {
		return nil, fmt.Errorf("invalid vmdk grain size %d sectors", grainSectors)
	}

	d := &Disk{
		ra:           ra,
		size:         capacity * sectorSize,
		grainSectors: grainSectors,
		grainBytes:   grainSectors * sectorSize,
		lastGrain:    capacity / grainSectors,
		compressed:   flags&flagCompress != 0,
		embedLBA:     flags&flagEmbedLBA != 0,
	}
	d.lastGrainSize = (capacity & (grainSectors - 1)) * sectorSize

	gtes := d.lastGrain
	if d.lastGrainSize != 0 {
		gtes++
	}
	if gtes == 0 || numGTEsPerGT == 0 {
		return nil, errors.New("invalid vmdk grain table geometry")
	}
	gts := (gtes + numGTEsPerGT - 1) / numGTEsPerGT
	gdSectors := (gts*4 + sectorSize - 1) / sectorSize
	gtSectors := (numGTEsPerGT*4 + sectorSize - 1) / sectorSize

	// Read the grain directory (GDE[i] is the sector of grain table i).
	gd := make([]byte, gdSectors*sectorSize)
	if _, err := ra.ReadAt(gd, gdOffset*sectorSize); err != nil {
		return nil, fmt.Errorf("read vmdk grain directory: %w", err)
	}

	d.gt = make([]uint32, gtes)
	gtBuf := make([]byte, gtSectors*sectorSize)
	for i := int64(0); i < gts; i++ {
		gtSector := int64(le.Uint32(gd[i*4 : i*4+4]))
		if gtSector == 0 || gtSector == 1 {
			continue
		}
		if _, err := ra.ReadAt(gtBuf, gtSector*sectorSize); err != nil {
			return nil, fmt.Errorf("read vmdk grain table %d: %w", i, err)
		}
		base := i * numGTEsPerGT
		for j := int64(0); j < numGTEsPerGT && base+j < gtes; j++ {
			d.gt[base+j] = le.Uint32(gtBuf[j*4 : j*4+4])
		}
	}
	return d, nil
}

// Size returns the virtual size in bytes.
func (d *Disk) Size() int64 { return d.size }

// Close releases no underlying resources; the caller owns ra.
func (d *Disk) Close() error { return nil }

// ReadAt reads len(p) bytes at off, matching io.ReaderAt semantics.
func (d *Disk) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("negative offset")
	}
	if off >= d.size {
		return 0, io.EOF
	}
	if int64(len(p)) > d.size-off {
		p = p[:d.size-off]
	}

	grainBuf := make([]byte, d.grainBytes)
	readBuf := make([]byte, d.grainBytes+sectorSize)

	total := 0
	for len(p) > 0 {
		grainNr := off / d.grainBytes
		skip := off % d.grainBytes

		grainSize := d.grainBytes
		if grainNr == d.lastGrain && d.lastGrainSize != 0 {
			grainSize = d.lastGrainSize
		} else if grainNr > d.lastGrain {
			break
		}
		if skip >= grainSize {
			break
		}

		readLen := grainSize - skip
		if int64(len(p)) < readLen {
			readLen = int64(len(p))
		}

		if grainNr >= int64(len(d.gt)) {
			break
		}
		sector := int64(d.gt[grainNr])
		switch sector {
		case 0, 1:
			copy(p[:readLen], make([]byte, readLen))
		default:
			if err := d.readGrain(sector, p[:readLen], skip, grainBuf, readBuf); err != nil {
				return total, err
			}
		}

		p = p[readLen:]
		off += readLen
		total += int(readLen)
	}
	if total == 0 {
		return 0, io.EOF
	}
	return total, nil
}

// readGrain reads one grain located at the given absolute sector and copies
// skip..skip+len(dst) bytes into dst.
func (d *Disk) readGrain(sector int64, dst []byte, skip int64, grainBuf, readBuf []byte) error {
	if !d.compressed {
		if _, err := d.ra.ReadAt(grainBuf[:d.grainBytes], sector*sectorSize); err != nil {
			return err
		}
		copy(dst, grainBuf[skip:skip+int64(len(dst))])
		return nil
	}

	hdrlen := int64(4)
	if d.embedLBA {
		hdrlen = 12
	}
	// Read the header sector, then the remainder if the compressed payload
	// spans more than one sector.
	if _, err := d.ra.ReadAt(readBuf[:sectorSize], sector*sectorSize); err != nil {
		return err
	}
	cmpSize := int64(binary.LittleEndian.Uint32(readBuf[hdrlen-4 : hdrlen]))
	totalBytes := hdrlen + cmpSize
	if totalBytes > sectorSize {
		extra := (totalBytes - sectorSize + sectorSize - 1) &^ (sectorSize - 1)
		if _, err := d.ra.ReadAt(readBuf[sectorSize:sectorSize+extra], (sector+1)*sectorSize); err != nil {
			return err
		}
	}

	zr, err := zlib.NewReader(bytes.NewReader(readBuf[hdrlen : hdrlen+cmpSize]))
	if err != nil {
		return fmt.Errorf("grain at sector %d: %w", sector, err)
	}
	defer zr.Close()
	if _, err := io.ReadFull(zr, grainBuf[:d.grainBytes]); err != nil && err != io.ErrUnexpectedEOF {
		return fmt.Errorf("inflate grain at sector %d: %w", sector, err)
	}
	copy(dst, grainBuf[skip:skip+int64(len(dst))])
	return nil
}
