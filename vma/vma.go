package vma

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strings"
)

const clusterSize = 65536

// Disk is one device extracted from a VMA.
type Disk struct {
	Name string
	size int64
	file *os.File
}

// ReadAt reads from the extracted raw image.
func (d *Disk) ReadAt(p []byte, off int64) (int, error) { return d.file.ReadAt(p, off) }

// Size returns the device size in bytes.
func (d *Disk) Size() int64 { return d.size }

// Close releases the temporary file backing this disk.
func (d *Disk) Close() error {
	if d.file == nil {
		return nil
	}
	var err error
	if cerr := d.file.Close(); cerr != nil {
		err = cerr
	}
	if rerr := os.Remove(d.file.Name()); rerr != nil && !os.IsNotExist(rerr) && err == nil {
		err = rerr
	}
	d.file = nil
	return err
}

// Image is an opened VMA archive.
type Image struct {
	Disks []*Disk
}

// Close releases every extracted disk.
func (img *Image) Close() error {
	var err error
	for _, d := range img.Disks {
		if cerr := d.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}
	return err
}

// OpenFile opens path and extracts each device to a temporary raw file.
func OpenFile(path string) (*Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	hdr, err := readMainHeader(f)
	if err != nil {
		return nil, err
	}

	blobs, err := readBlobBuffer(f, hdr.blobOffset, hdr.blobSize)
	if err != nil {
		return nil, err
	}

	// Create one temporary file per populated device. blobs map an offset in
	// the blob buffer to its (uncompressed) data.
	disks := make([]*Disk, len(hdr.devInfo))
	for i, di := range hdr.devInfo {
		if di.size == 0 {
			continue
		}
		tmp, err := os.CreateTemp("", "udf-vma-*.raw")
		if err != nil {
			closeDisks(disks)
			return nil, err
		}
		name := string(blobs[di.nameOff])
		name = strings.SplitN(name, "\x00", 2)[0]
		if name == "" {
			name = fmt.Sprintf("disk-%d", i)
		}
		disks[i] = &Disk{Name: name, size: di.size, file: tmp}
	}

	if _, err := f.Seek(int64(hdr.headerSize), io.SeekStart); err != nil {
		closeDisks(disks)
		return nil, err
	}

	// Reuse a single 64 KiB scratch buffer across every cluster instead of
	// allocating per cluster, which is the hot path for large images.
	scratch := make([]byte, clusterSize)

	// Read extents to the end of the file.
	for {
		ext, err := readExtentHeader(f)
		if err == io.EOF {
			break
		}
		if err != nil {
			closeDisks(disks)
			return nil, err
		}
		if err := applyExtent(f, disks, ext, scratch); err != nil {
			closeDisks(disks)
			return nil, err
		}
	}

	img := &Image{}
	for _, d := range disks {
		if d != nil {
			img.Disks = append(img.Disks, d)
		}
	}
	return img, nil
}

type mainHeader struct {
	blobOffset int
	blobSize   int
	headerSize int
	devInfo    []deviceInfo
}

type deviceInfo struct {
	nameOff int
	size    int64
}

func readMainHeader(f io.Reader) (*mainHeader, error) {
	// The fixed header is 12289 bytes: magic/version/uuid/ctime/md5 (60),
	// then 1984 bytes padding, 2×256 config offsets, 256 device entries,
	// and a trailing padding byte — after which the blob buffer starts.
	buf := make([]byte, 12289)
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, fmt.Errorf("read vma header: %w", err)
	}
	if string(buf[0:4]) != "VMA\x00" {
		return nil, fmt.Errorf("not a VMA archive (bad magic)")
	}
	if be32(buf[4:8]) != 1 {
		return nil, fmt.Errorf("unsupported VMA version %d", be32(buf[4:8]))
	}

	h := &mainHeader{
		blobOffset: int(be32(buf[48:52])),
		blobSize:   int(be32(buf[52:56])),
		headerSize: int(be32(buf[56:60])),
		devInfo:    make([]deviceInfo, 256),
	}
	// Device entries begin at offset 4096, 32 bytes each.
	for i := 0; i < 256; i++ {
		e := buf[4096+i*32 : 4096+i*32+32]
		h.devInfo[i] = deviceInfo{
			nameOff: int(be32(e[0:4])),
			size:    int64(be64(e[8:16])),
		}
	}
	return h, nil
}

// readBlobBuffer parses the length-prefixed blob area, returning data keyed by
// its offset within the buffer (the key device and config entries reference).
// The blob buffer begins with a single padding byte, so the first blob's length
// prefix sits at offset 1 and is keyed 1 (see vma_spec.txt).
func readBlobBuffer(f io.ReaderAt, offset, size int) (map[int][]byte, error) {
	buf := make([]byte, size)
	if _, err := f.ReadAt(buf, int64(offset)); err != nil {
		return nil, fmt.Errorf("read vma blob buffer: %w", err)
	}
	out := make(map[int][]byte)
	pos := 1
	for pos+2 <= len(buf) {
		key := pos
		n := int(buf[pos]) | int(buf[pos+1])<<8 // little-endian length
		pos += 2
		if n == 0 || pos+n > len(buf) {
			break
		}
		out[key] = buf[pos : pos+n]
		pos += n
	}
	return out, nil
}

type extent struct {
	blockCount int
	infos      []blockInfo
}

type blockInfo struct {
	mask       uint16
	devID      int
	clusterNum int64
}

func readExtentHeader(f io.Reader) (*extent, error) {
	buf := make([]byte, 512)
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, err
	}
	if string(buf[0:4]) != "VMAE" {
		return nil, fmt.Errorf("bad VMA extent magic %q", buf[0:4])
	}
	blockCount := int(be16(buf[6:8]))
	if blockCount < 1 || blockCount > 59 {
		return nil, fmt.Errorf("invalid VMA extent block count %d", blockCount)
	}
	// The extent header is a fixed 512 bytes. Each block-info record is 8 bytes
	// (see vma_spec.txt): mask(u16 BE) at +0, reserved(u8) at +2, dev_id(u8) at
	// +3, cluster_num(u32 BE) at +4. A full extent holds 59 records from +40.
	infos := make([]blockInfo, blockCount)
	for i := 0; i < blockCount; i++ {
		b := buf[40+i*8 : 40+i*8+8]
		infos[i] = blockInfo{
			mask:       be16(b[0:2]),
			devID:      int(b[3]),
			clusterNum: int64(be32(b[4:8])),
		}
	}
	return &extent{blockCount: blockCount, infos: infos}, nil
}

func applyExtent(f io.Reader, disks []*Disk, ext *extent, scratch []byte) error {
	for _, bi := range ext.infos {
		if bi.devID <= 0 || bi.devID >= len(disks) || disks[bi.devID] == nil {
			// A device this extent does not describe still consumes only the
			// data its mask says it holds, which is zero here.
			if bi.mask != 0 {
				return fmt.Errorf("vma cluster references invalid device %d", bi.devID)
			}
			continue
		}
		d := disks[bi.devID]
		pos := bi.clusterNum * clusterSize

		switch bi.mask {
		case 0:
			// Sparse zero cluster; leave the temp file hole untouched.
		case 0xFFFF:
			if _, err := io.ReadFull(f, scratch); err != nil {
				return fmt.Errorf("read vma cluster: %w", err)
			}
			if _, err := d.file.WriteAt(scratch, pos); err != nil {
				return err
			}
		default:
			chunk := scratch[:4096]
			for i := 0; i < 16; i++ {
				if bi.mask&(1<<i) == 0 {
					continue
				}
				if _, err := io.ReadFull(f, chunk); err != nil {
					return fmt.Errorf("read vma cluster chunk: %w", err)
				}
				if _, err := d.file.WriteAt(chunk, pos+int64(i)*4096); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func closeDisks(disks []*Disk) {
	for _, d := range disks {
		if d != nil {
			_ = d.Close()
		}
	}
}

func be16(b []byte) uint16 { return binary.BigEndian.Uint16(b) }
func be32(b []byte) uint32 { return binary.BigEndian.Uint32(b) }
func be64(b []byte) uint64 { return binary.BigEndian.Uint64(b) }
