// Package ntfs implements a read-only NTFS 3.1 volume driver that operates
// directly on partition data without OS involvement. It is vendored from
// carbon-os/diskimg/ntfs; the backend is io.ReaderAt (not *os.File), and the
// write methods remain but are stubbed to fail, so images are never modified.
//
// On-disk structures supported:
//   - Boot sector BPB (bytes-per-sector, sectors-per-cluster, MFT location)
//   - Master File Table records with Update Sequence Array (fixup) handling
//   - Resident and non-resident attributes with runlist decoding
//   - $I30 directory B-tree indices (INDEX_ROOT + INDEX_ALLOCATION)
//   - Symlinks and junction points via $REPARSE_POINT
//   - Cluster bitmap ($Bitmap) and MFT record bitmap for allocation
//
// Write operations update MFT records and the cluster bitmap in-place.
// The NTFS journal ($LogFile) is not replayed on open; Unmount marks the
// volume clean.  Run ntfsfix(8) or chkdsk /f if strict journal replay is
// required after an unclean shutdown of the image.
package ntfs

import (
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"sync"
	"time"

	"github.com/carbon-os/diskimg/fs/fstype"
)

// ── attribute type codes ──────────────────────────────────────────────────────

const (
	attrSTANDARD_INFORMATION uint32 = 0x10
	attrATTRIBUTE_LIST       uint32 = 0x20
	attrFILE_NAME            uint32 = 0x30
	attrOBJECT_ID            uint32 = 0x40
	attrSECURITY_DESCRIPTOR  uint32 = 0x50
	attrVOLUME_NAME          uint32 = 0x60
	attrVOLUME_INFORMATION   uint32 = 0x70
	attrDATA                 uint32 = 0x80
	attrINDEX_ROOT           uint32 = 0x90
	attrINDEX_ALLOCATION     uint32 = 0xA0
	attrBITMAP               uint32 = 0xB0
	attrREPARSE_POINT        uint32 = 0xC0
	attrEND                  uint32 = 0xFFFFFFFF
)

// ── well-known MFT record numbers ─────────────────────────────────────────────

const (
	recMFT     = 0
	recMFTMirr = 1
	recLogFile = 2
	recVolume  = 3
	recAttrDef = 4
	recRoot    = 5
	recBitmap  = 6
	recBoot    = 7
)

// ── misc constants ────────────────────────────────────────────────────────────

const (
	ntfsOEM = "NTFS    "

	mftFlagInUse uint16 = 0x01
	mftFlagDir   uint16 = 0x02

	idxFlagSubNode   uint32 = 0x01
	idxFlagLastEntry uint32 = 0x02

	// File attribute flags from $STANDARD_INFORMATION / $FILE_NAME.
	faReadOnly  uint32 = 0x0001
	faHidden    uint32 = 0x0002
	faSystem    uint32 = 0x0004
	faDirectory uint32 = 0x0010
	faArchive   uint32 = 0x0020

	// Reparse point tags.
	reparseSymlink  uint32 = 0xA000000C
	reparseJunction uint32 = 0xA0000003

	// 100-ns intervals between the Windows (1601-01-01) and Unix (1970-01-01) epochs.
	filetimeDelta = int64(116444736000000000)
)

// ── volume ────────────────────────────────────────────────────────────────────

// ntfsVolume is the runtime state of a mounted NTFS partition.
type ntfsVolume struct {
	f    io.ReaderAt
	base int64 // byte offset of partition start within f
	size int64 // partition byte length

	clusterSize   int64
	recSize       int64 // MFT record size in bytes (typically 1024)
	idxBlockSize  int64 // index block size in bytes (typically 4096)
	mftOff        int64 // partition-relative byte offset of $MFT start
	totalClusters int64

	mu        sync.Mutex
	mftCache  map[uint64][]byte // record# → fixup-applied record bytes
	bitmap    []byte            // volume cluster allocation bitmap
	mftBitmap []byte            // $MFT record allocation bitmap
	dirty     bool
}

// ── Open ──────────────────────────────────────────────────────────────────────

// Open mounts the NTFS partition starting at partOff within the read-only
// backend f.
func Open(f io.ReaderAt, partOff, partSize int64) (*ntfsVolume, error) {
	v := &ntfsVolume{
		f:        f,
		base:     partOff,
		size:     partSize,
		mftCache: make(map[uint64][]byte),
	}
	if err := v.parseBoot(); err != nil {
		return nil, fmt.Errorf("ntfs.Open: %w", err)
	}
	if err := v.loadMFT(); err != nil {
		return nil, fmt.Errorf("ntfs.Open: %w", err)
	}
	if err := v.loadBitmap(); err != nil {
		return nil, fmt.Errorf("ntfs.Open: %w", err)
	}
	return v, nil
}

// parseBoot reads the NTFS BPB and derives all geometry constants.
func (v *ntfsVolume) parseBoot() error {
	boot := make([]byte, 512)
	if _, err := v.f.ReadAt(boot, v.base); err != nil {
		return fmt.Errorf("boot sector: %w", err)
	}
	if string(boot[3:11]) != ntfsOEM {
		return fmt.Errorf("bad OEM ID %q", string(boot[3:11]))
	}

	bps := int64(binary.LittleEndian.Uint16(boot[0x0B:]))
	spc := int64(boot[0x0D])
	v.clusterSize = bps * spc

	totalSec := int64(binary.LittleEndian.Uint64(boot[0x28:]))
	if spc > 0 {
		v.totalClusters = totalSec / spc
	}

	mftLCN := int64(binary.LittleEndian.Uint64(boot[0x30:]))
	v.mftOff = mftLCN * v.clusterSize

	// clusters-per-MFT-record: negative byte means 2^(-byte) bytes.
	cpm := int8(boot[0x40])
	if cpm < 0 {
		v.recSize = 1 << uint(-cpm)
	} else {
		v.recSize = int64(cpm) * v.clusterSize
	}
	if v.recSize == 0 {
		v.recSize = 1024
	}

	cpi := int8(boot[0x44])
	if cpi < 0 {
		v.idxBlockSize = 1 << uint(-cpi)
	} else {
		v.idxBlockSize = int64(cpi) * v.clusterSize
	}
	if v.idxBlockSize == 0 {
		v.idxBlockSize = 4096
	}
	return nil
}

func (v *ntfsVolume) loadMFT() error {
	rec0, err := v.readRawRecord(0)
	if err != nil {
		return fmt.Errorf("MFT record 0: %w", err)
	}
	v.mftCache[0] = rec0

	// Pre-warm the system records (1–11) on a best-effort basis.
	for i := uint64(1); i <= 11; i++ {
		rec, err := v.readRawRecord(i)
		if err != nil {
			continue
		}
		v.mftCache[i] = rec
	}

	// $MFT's own $BITMAP tells us which MFT record slots are allocated.
	if bAttr := findAttr(rec0, attrBITMAP, ""); bAttr != nil {
		v.mftBitmap, _ = v.attrValue(rec0, bAttr)
	}
	return nil
}

func (v *ntfsVolume) loadBitmap() error {
	rec, err := v.getRecord(recBitmap)
	if err != nil {
		return fmt.Errorf("$Bitmap record: %w", err)
	}
	attr := findAttr(rec, attrDATA, "")
	if attr == nil {
		return fmt.Errorf("$Bitmap has no $DATA attribute")
	}
	data, err := v.attrValue(rec, attr)
	if err != nil {
		return fmt.Errorf("$Bitmap data: %w", err)
	}
	v.bitmap = data
	return nil
}

// ── raw I/O ───────────────────────────────────────────────────────────────────

func (v *ntfsVolume) partRead(p []byte, off int64) (int, error) {
	return v.f.ReadAt(p, v.base+off)
}

func (v *ntfsVolume) partWrite(p []byte, off int64) (int, error) {
	return 0, fmt.Errorf("ntfs: volume is read-only")
}

// readRawRecord reads MFT record n from its known location and applies the
// Update Sequence Array fixup.
func (v *ntfsVolume) readRawRecord(n uint64) ([]byte, error) {
	off := v.mftOff + int64(n)*v.recSize
	buf := make([]byte, v.recSize)
	if _, err := v.partRead(buf, off); err != nil {
		return nil, fmt.Errorf("read record %d: %w", n, err)
	}
	if len(buf) >= 4 && string(buf[:4]) == "BAAD" {
		return nil, fmt.Errorf("MFT record %d is corrupt (BAAD signature)", n)
	}
	if len(buf) >= 4 && string(buf[:4]) != "FILE" {
		return nil, fmt.Errorf("MFT record %d: unexpected signature %q", n, buf[:4])
	}
	applyUSA(buf)
	return buf, nil
}

// getRecord returns the cached fixed-up record for n, reading it from disk if
// not already present in the cache.
func (v *ntfsVolume) getRecord(n uint64) ([]byte, error) {
	v.mu.Lock()
	rec, ok := v.mftCache[n]
	v.mu.Unlock()
	if ok {
		return rec, nil
	}
	rec, err := v.readRawRecord(n)
	if err != nil {
		return nil, err
	}
	v.mu.Lock()
	v.mftCache[n] = rec
	v.mu.Unlock()
	return rec, nil
}

// ── Update Sequence Array ─────────────────────────────────────────────────────

// applyUSA undoes the sector-end stamps so the record is readable.
func applyUSA(buf []byte) {
	if len(buf) < 8 {
		return
	}
	usaOff := int(binary.LittleEndian.Uint16(buf[4:]))
	usaCnt := int(binary.LittleEndian.Uint16(buf[6:]))
	if usaOff == 0 || usaOff+usaCnt*2 > len(buf) {
		return
	}
	for i := 1; i < usaCnt; i++ {
		pos := i*512 - 2
		if pos+2 > len(buf) {
			break
		}
		buf[pos] = buf[usaOff+i*2]
		buf[pos+1] = buf[usaOff+i*2+1]
	}
}

// stampUSA increments the sequence number and stamps the sector-end words,
// saving the originals into the USA.  sectors is len(buf)/512.
func stampUSA(buf []byte, sectors int) {
	if len(buf) < 8 {
		return
	}
	usaOff := int(binary.LittleEndian.Uint16(buf[4:]))
	usaCnt := int(binary.LittleEndian.Uint16(buf[6:]))
	if usaOff == 0 || usaOff+usaCnt*2 > len(buf) {
		return
	}
	seq := binary.LittleEndian.Uint16(buf[usaOff:]) + 1
	if seq == 0 {
		seq = 1
	}
	binary.LittleEndian.PutUint16(buf[usaOff:], seq)
	for i := 1; i < usaCnt && i <= sectors; i++ {
		pos := i*512 - 2
		if pos+2 > len(buf) {
			break
		}
		buf[usaOff+i*2] = buf[pos]
		buf[usaOff+i*2+1] = buf[pos+1]
		buf[pos] = byte(seq)
		buf[pos+1] = byte(seq >> 8)
	}
}

// ── time helpers ──────────────────────────────────────────────────────────────

func filetimeToTime(ft int64) time.Time {
	if ft == 0 {
		return time.Time{}
	}
	return time.Unix(0, (ft-filetimeDelta)*100).UTC()
}

func timeToFiletime(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()/100 + filetimeDelta
}

// Type implements fs.Volume.
func (v *ntfsVolume) Type() fstype.Type { return fstype.NTFS }

// ── fileInfo ──────────────────────────────────────────────────────────────────

// ntfsFileInfo implements fs.FileInfo for an MFT entry.
type ntfsFileInfo struct {
	name    string
	size    int64
	mode    fs.FileMode
	modTime time.Time
	isDir   bool
}

func (fi *ntfsFileInfo) Name() string       { return fi.name }
func (fi *ntfsFileInfo) Size() int64        { return fi.size }
func (fi *ntfsFileInfo) Mode() fs.FileMode  { return fi.mode }
func (fi *ntfsFileInfo) ModTime() time.Time { return fi.modTime }
func (fi *ntfsFileInfo) IsDir() bool        { return fi.isDir }
func (fi *ntfsFileInfo) Sys() any           { return nil }

// ntfsDirEntry implements fs.DirEntry.
type ntfsDirEntry struct {
	info   *ntfsFileInfo
	mftNum uint64
}

func (de *ntfsDirEntry) Name() string               { return de.info.Name() }
func (de *ntfsDirEntry) IsDir() bool                { return de.info.IsDir() }
func (de *ntfsDirEntry) Type() fs.FileMode          { return de.info.Mode().Type() }
func (de *ntfsDirEntry) Info() (fs.FileInfo, error) { return de.info, nil }
