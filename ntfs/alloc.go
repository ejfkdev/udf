// alloc.go
package ntfs

import (
	"encoding/binary"
	"fmt"
	"path"
	"strings"
	"unicode/utf16"
)

// ── cluster bitmap ────────────────────────────────────────────────────────────

// allocCluster finds the first free cluster at LCN ≥ 1, marks it used, and
// returns its LCN.  LCN 0 is the NTFS boot sector and must never be allocated
// for file data; if mkfs left that bit clear we correct it silently here.
func (v *ntfsVolume) allocCluster() (int64, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for i, b := range v.bitmap {
		if b == 0xFF {
			continue
		}
		for bit := 0; bit < 8; bit++ {
			if b&(1<<uint(bit)) == 0 {
				lcn := int64(i*8 + bit)
				if lcn == 0 {
					// Boot sector cluster — mark used and keep scanning.
					v.bitmap[i] |= 1 << uint(bit)
					v.dirty = true
					continue
				}
				v.bitmap[i] |= 1 << uint(bit)
				v.dirty = true
				return lcn, nil
			}
		}
	}
	return 0, fmt.Errorf("no free clusters (volume full)")
}

// allocClusters allocates n clusters and returns a (possibly fragmented) runlist.
func (v *ntfsVolume) allocClusters(n int64) ([]run, error) {
	var runs []run
	remaining := n
	for remaining > 0 {
		lcn, err := v.allocCluster()
		if err != nil {
			return nil, err
		}
		if len(runs) > 0 {
			last := &runs[len(runs)-1]
			if last.lcn+last.length == lcn {
				last.length++
				remaining--
				continue
			}
		}
		runs = append(runs, run{lcn: lcn, length: 1})
		remaining--
	}
	return runs, nil
}

// tryAllocContiguous scans v.bitmap for a run of n contiguous free clusters
// starting at LCN ≥ 1.  On success it marks them used and returns a single-
// element runlist.  Returns nil, nil when no run of that size exists.
//
// Caller must hold v.mu.
func (v *ntfsVolume) tryAllocContiguous(n int64) ([]run, error) {
	needed := int(n)
	startLCN := -1
	runLen := 0

	for i := 0; i < len(v.bitmap); i++ {
		for bit := 0; bit < 8; bit++ {
			lcn := i*8 + bit
			if lcn == 0 {
				startLCN = -1
				runLen = 0
				continue
			}
			if v.bitmap[i]&(1<<uint(bit)) == 0 {
				if runLen == 0 {
					startLCN = lcn
				}
				runLen++
				if runLen >= needed {
					for j := 0; j < needed; j++ {
						l := int64(startLCN + j)
						v.bitmap[l/8] |= 1 << uint(l%8)
					}
					v.dirty = true
					return []run{{lcn: int64(startLCN), length: n}}, nil
				}
			} else {
				startLCN = -1
				runLen = 0
			}
		}
	}
	return nil, nil
}

// allocContiguousClusters allocates n clusters, strongly preferring a single
// contiguous run so that NTFS runlists (and therefore MFT records) stay small.
// Falls back to the fragmented allocClusters when no contiguous range exists.
func (v *ntfsVolume) allocContiguousClusters(n int64) ([]run, error) {
	if n <= 0 {
		return nil, nil
	}
	v.mu.Lock()
	runs, err := v.tryAllocContiguous(n)
	v.mu.Unlock()
	if err != nil || runs != nil {
		return runs, err
	}
	return v.allocClusters(n)
}

// freeCluster marks a single cluster as free.
func (v *ntfsVolume) freeCluster(lcn int64) {
	if lcn == 0 {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	byteIdx := lcn / 8
	bitIdx := uint(lcn % 8)
	if int(byteIdx) < len(v.bitmap) {
		v.bitmap[byteIdx] &^= 1 << bitIdx
		v.dirty = true
	}
}

// freeRuns frees all clusters referenced by a runlist.
func (v *ntfsVolume) freeRuns(runs []run) {
	for _, r := range runs {
		if r.lcn < 0 {
			continue
		}
		for i := int64(0); i < r.length; i++ {
			v.freeCluster(r.lcn + i)
		}
	}
}

// allocAndWrite allocates clusters for data, writes the data, and returns the runlist.
func (v *ntfsVolume) allocAndWrite(data []byte) ([]run, error) {
	numClusters := (int64(len(data)) + v.clusterSize - 1) / v.clusterSize
	runs, err := v.allocContiguousClusters(numClusters)
	if err != nil {
		return nil, err
	}
	var written int64
	for _, r := range runs {
		for c := int64(0); c < r.length; c++ {
			off := (r.lcn + c) * v.clusterSize
			start := written
			end := written + v.clusterSize
			if end > int64(len(data)) {
				end = int64(len(data))
			}
			chunk := make([]byte, v.clusterSize)
			if start < int64(len(data)) {
				copy(chunk, data[start:end])
			}
			if _, err := v.partWrite(chunk, off); err != nil {
				return nil, fmt.Errorf("write cluster LCN %d: %w", r.lcn+c, err)
			}
			written += v.clusterSize
		}
	}
	return runs, nil
}

// ── MFT record bitmap ─────────────────────────────────────────────────────────

func (v *ntfsVolume) allocMFTRecord() (uint64, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for i, b := range v.mftBitmap {
		if b == 0xFF {
			continue
		}
		for bit := 0; bit < 8; bit++ {
			if b&(1<<uint(bit)) == 0 {
				n := uint64(i*8 + bit)
				if n < 12 {
					continue
				}
				v.mftBitmap[i] |= 1 << uint(bit)
				v.dirty = true
				return n, nil
			}
		}
	}
	newByte := byte(0x01)
	n := uint64(len(v.mftBitmap) * 8)
	v.mftBitmap = append(v.mftBitmap, newByte)
	v.dirty = true
	return n, nil
}

func freeMFTSlot(bitmap []byte, n uint64) {
	byteIdx := n / 8
	bitIdx := uint(n % 8)
	if int(byteIdx) < len(bitmap) {
		bitmap[byteIdx] &^= 1 << bitIdx
	}
}

// ── INDX block helpers ────────────────────────────────────────────────────────

// indxEntriesStart returns the absolute offset within an INDX block where
// index entries begin.  The USA (Update Sequence Array) occupies the bytes
// immediately following the 24-byte INDX+INDEX_HEADER preamble; entries start
// after the USA, aligned to 8 bytes.
//
// INDX layout:
//
//	0x00  4  "INDX"
//	0x04  2  USA offset = 0x28
//	0x06  2  USA count  = blockSize/512 + 1
//	0x08  8  LSN
//	0x10  8  VCN of this block
//	0x18 16  INDEX_HEADER (entries_offset, index_length, alloc_size, flags)
//	0x28  +  USA (usaCount × 2 bytes)
//	…     +  index entries start here (aligned to 8)
func indxEntriesStart(blockSize int64) int {
	usaCount := int(blockSize/512) + 1
	usaEnd := 0x28 + usaCount*2
	return (usaEnd + 7) &^ 7
}

// indxUsableSpace returns how many bytes are available for index entries in a
// single INDX block (excluding the 16-byte end-sentinel).
func indxUsableSpace(blockSize int64) int64 {
	return blockSize - int64(indxEntriesStart(blockSize)) - 16
}

// buildINDXBlock constructs a complete, USA-stamped INDX block.
// rawEntries is the concatenated bytes of all entries to place in the block
// (must fit within indxUsableSpace).  vcn is the 0-based block index.
func buildINDXBlock(vcn int64, rawEntries []byte, blockSize int64) []byte {
	buf := make([]byte, blockSize)

	usaCount := int(blockSize/512) + 1
	entriesStart := indxEntriesStart(blockSize)
	entriesOff := entriesStart - 0x18  // relative to INDEX_HEADER at 0x18
	allocSize := int(blockSize) - 0x18 // relative to INDEX_HEADER

	// INDX record header
	copy(buf[0:], "INDX")
	binary.LittleEndian.PutUint16(buf[0x04:], 0x28)
	binary.LittleEndian.PutUint16(buf[0x06:], uint16(usaCount))
	// LSN = 0 (already zeroed by make)
	binary.LittleEndian.PutUint64(buf[0x10:], uint64(vcn))

	// INDEX_HEADER at 0x18
	binary.LittleEndian.PutUint32(buf[0x18:], uint32(entriesOff))
	// index_length and alloc_size at 0x1C and 0x20
	binary.LittleEndian.PutUint32(buf[0x20:], uint32(allocSize))
	// flags = 0 (leaf node, no sub-tree pointers)

	// USA initial check value
	binary.LittleEndian.PutUint16(buf[0x28:], 1)

	// Copy entries
	if len(rawEntries) > 0 {
		copy(buf[entriesStart:], rawEntries)
	}

	// End sentinel: entLen=16, keyLen=0, flags=idxFlagLastEntry
	sentinelOff := entriesStart + len(rawEntries)
	binary.LittleEndian.PutUint16(buf[sentinelOff+8:], 16)
	binary.LittleEndian.PutUint32(buf[sentinelOff+12:], uint32(idxFlagLastEntry))

	// index_length = entriesOff + sizeof(all_entries) + sizeof(sentinel)
	indexLen := entriesOff + len(rawEntries) + 16
	binary.LittleEndian.PutUint32(buf[0x1C:], uint32(indexLen))

	// Stamp USA (sectors = blockSize/512; stampUSA takes sector count, not usaCount)
	stampUSA(buf, int(blockSize/512))
	return buf
}

// buildEmptyIndexRootVal builds a minimal $INDEX_ROOT value (16-byte root
// header preserved from src, plus an empty INDEX_HEADER with just the
// end-sentinel).  Used after spilling all entries to $INDEX_ALLOCATION.
func buildEmptyIndexRootVal(src []byte) []byte {
	// src is the original 16-byte INDEX_ROOT_HEADER (attr type, collation,
	// block size, clusters/block) from the existing $INDEX_ROOT value.
	ir := make([]byte, 48)
	if len(src) >= 16 {
		copy(ir[0:16], src[:16])
	}
	// INDEX_HEADER at offset 16:
	//   entries_offset = 16  (entries immediately follow INDEX_HEADER)
	//   index_length   = 32  (INDEX_HEADER[16] + sentinel[16])
	//   alloc_size     = 32
	//   flags          = 0   (leaf node)
	binary.LittleEndian.PutUint32(ir[16:], 16)
	binary.LittleEndian.PutUint32(ir[20:], 32)
	binary.LittleEndian.PutUint32(ir[24:], 32)
	// End sentinel at offset 32 (= 16 INDEX_HEADER + 16 entries_offset):
	binary.LittleEndian.PutUint16(ir[40:], 16)
	binary.LittleEndian.PutUint32(ir[44:], uint32(idxFlagLastEntry))
	return ir
}

// collectRawIndexEntries extracts all non-sentinel entries from an
// INDEX_HEADER body as a slice of raw byte slices.
func collectRawIndexEntries(hdr []byte) [][]byte {
	if len(hdr) < 8 {
		return nil
	}
	entriesOff := int(binary.LittleEndian.Uint32(hdr[0:]))
	indexLen := int(binary.LittleEndian.Uint32(hdr[4:]))
	if entriesOff > len(hdr) || indexLen > len(hdr) {
		return nil
	}
	data := hdr[entriesOff:indexLen]
	var out [][]byte
	off := 0
	for off+16 <= len(data) {
		flags := binary.LittleEndian.Uint32(data[off+12:])
		if flags&idxFlagLastEntry != 0 {
			break
		}
		entLen := int(binary.LittleEndian.Uint16(data[off+8:]))
		if entLen <= 0 {
			break
		}
		e := make([]byte, entLen)
		copy(e, data[off:off+entLen])
		out = append(out, e)
		off += entLen
	}
	return out
}

// flattenEntries concatenates a slice of raw entry byte slices.
func flattenEntries(entries [][]byte) []byte {
	var total int
	for _, e := range entries {
		total += len(e)
	}
	buf := make([]byte, total)
	off := 0
	for _, e := range entries {
		copy(buf[off:], e)
		off += len(e)
	}
	return buf
}

// lcnOfVCN returns the LCN for a given virtual cluster number within a runlist.
func lcnOfVCN(runs []run, vcn int64) int64 {
	var off int64
	for _, r := range runs {
		if vcn >= off && vcn < off+r.length {
			return r.lcn + (vcn - off)
		}
		off += r.length
	}
	return -1
}

// mergeRuns extends existing runs with newRuns, coalescing contiguous runs.
func mergeRuns(existing, newRuns []run) []run {
	result := make([]run, len(existing))
	copy(result, existing)
	for _, nr := range newRuns {
		if len(result) > 0 {
			last := &result[len(result)-1]
			if last.lcn+last.length == nr.lcn {
				last.length += nr.length
				continue
			}
		}
		result = append(result, nr)
	}
	return result
}

// growBitmap returns a bitmap with bit n set, expanding the slice if needed.
func growBitmap(existing []byte, n int) []byte {
	needed := n/8 + 1
	bm := make([]byte, needed)
	copy(bm, existing)
	bm[n/8] |= 1 << uint(n%8)
	return bm
}

// addEntryToINDXBlock inserts entry into an already-USA-applied INDX block
// immediately before the end-sentinel.  Returns false if there is not enough
// space.
func addEntryToINDXBlock(block []byte, entry []byte) bool {
	if len(block) < 0x28 || string(block[:4]) != "INDX" {
		return false
	}
	entriesOff := int(binary.LittleEndian.Uint32(block[0x18:]))
	indexLen := int(binary.LittleEndian.Uint32(block[0x1C:]))
	allocSize := int(binary.LittleEndian.Uint32(block[0x20:]))
	if allocSize-indexLen < len(entry) {
		return false
	}
	hdrStart := 0x18
	dataStart := hdrStart + entriesOff
	dataLen := indexLen - entriesOff
	if dataStart+dataLen > len(block) {
		return false
	}
	data := block[dataStart : dataStart+dataLen]
	// Locate the end-sentinel.
	sentinelRelOff := -1
	off := 0
	for off+16 <= len(data) {
		flags := binary.LittleEndian.Uint32(data[off+12:])
		if flags&idxFlagLastEntry != 0 {
			sentinelRelOff = off
			break
		}
		entLen := int(binary.LittleEndian.Uint16(data[off+8:]))
		if entLen <= 0 {
			break
		}
		off += entLen
	}
	if sentinelRelOff < 0 {
		return false
	}
	sentinelAbsOff := dataStart + sentinelRelOff
	afterSentinel := sentinelAbsOff + 16
	if afterSentinel+len(entry) > len(block) {
		return false
	}
	// Shift sentinel forward to make room.
	copy(block[sentinelAbsOff+len(entry):], block[sentinelAbsOff:afterSentinel])
	copy(block[sentinelAbsOff:], entry)
	binary.LittleEndian.PutUint32(block[0x1C:], uint32(indexLen+len(entry)))
	return true
}

// ── directory index management ────────────────────────────────────────────────

// addDirEntry inserts a $FILE_NAME index entry into the directory at dirMFTNum.
//
// Strategy:
//  1. If $INDEX_ALLOCATION already exists on the directory, append the new
//     entry directly to the index allocation (never touch $INDEX_ROOT again).
//  2. Otherwise, try to fit the new entry into the resident $INDEX_ROOT.
//  3. If inserting into $INDEX_ROOT would push the MFT record past recSize,
//     spill all existing entries plus the new one into $INDEX_ALLOCATION and
//     leave $INDEX_ROOT as an empty root node.
func (v *ntfsVolume) addDirEntry(dirMFTNum uint64, name string, childMFTNum uint64, isDir bool) error {
	dirRec, err := v.getRecord(dirMFTNum)
	if err != nil {
		return err
	}
	dirRec = cloneRecord(dirRec)

	// Build the new index entry.
	fa := uint32(faArchive)
	if isDir {
		fa = faDirectory
	}
	fnKey := buildFileNameAttr(name, dirMFTNum, fa, isDir)
	entry := buildIndexEntry(childMFTNum, fnKey)

	// ── fast path: $INDEX_ALLOCATION already exists ───────────────────────
	if findAttrOffset(dirRec, attrINDEX_ALLOCATION, "$I30") >= 0 ||
		findAttrOffset(dirRec, attrINDEX_ALLOCATION, "") >= 0 {
		return v.appendEntryToIndexAlloc(dirRec, dirMFTNum, entry)
	}

	// ── locate $INDEX_ROOT ────────────────────────────────────────────────
	idxRootOff := findAttrOffset(dirRec, attrINDEX_ROOT, "$I30")
	if idxRootOff < 0 {
		idxRootOff = findAttrOffset(dirRec, attrINDEX_ROOT, "")
	}
	if idxRootOff < 0 {
		return fmt.Errorf("directory %d has no $INDEX_ROOT", dirMFTNum)
	}

	valOff := idxRootOff + int(binary.LittleEndian.Uint16(dirRec[idxRootOff+0x14:]))
	valLen := int(binary.LittleEndian.Uint32(dirRec[idxRootOff+0x10:]))
	if valOff+valLen > len(dirRec) {
		return fmt.Errorf("$INDEX_ROOT value out of bounds")
	}

	idxHdr := dirRec[valOff+16 : valOff+valLen]
	newHdr, err := insertIndexEntry(idxHdr, entry)
	if err != nil {
		return err
	}

	newVal := make([]byte, 16+len(newHdr))
	copy(newVal, dirRec[valOff:valOff+16])
	copy(newVal[16:], newHdr)

	// Calculate the MFT record size after the update.  The $INDEX_ROOT will be
	// re-added as a named "$I30" attribute (nameBytes = 8, valOff = 40).
	oldAttrLen := int(binary.LittleEndian.Uint32(dirRec[idxRootOff+4:]))
	newAttrLen := (40 + len(newVal) + 7) &^ 7 // named resident hdr = 40 bytes
	usedNow := int(binary.LittleEndian.Uint32(dirRec[0x18:]))
	projectedUsed := usedNow - oldAttrLen + newAttrLen

	if int64(projectedUsed+8) <= v.recSize { // +8 for end-marker
		// ── fits: update $INDEX_ROOT inline ──────────────────────────────
		dirRec = removeAttr(dirRec, attrINDEX_ROOT, "$I30")
		if findAttrOffset(dirRec, attrINDEX_ROOT, "$I30") < 0 {
			dirRec = removeAttr(dirRec, attrINDEX_ROOT, "")
		}
		off := nextAttrOffset(dirRec, int(binary.LittleEndian.Uint16(dirRec[0x14:])))
		dirRec = appendNamedResidentAttrAt(dirRec, off, attrINDEX_ROOT, "$I30", newVal)
		return v.putRecord(dirMFTNum, dirRec)
	}

	// ── overflow: spill all entries + new one to $INDEX_ALLOCATION ────────
	existing := collectRawIndexEntries(idxHdr)
	allEntries := append(existing, entry)
	idxRootHdr := make([]byte, 16)
	copy(idxRootHdr, dirRec[valOff:valOff+16]) // preserve INDEX_ROOT_HEADER

	return v.spillToIndexAlloc(dirRec, dirMFTNum, allEntries, idxRootHdr)
}

// spillToIndexAlloc moves all entries out of $INDEX_ROOT into one or more
// INDX blocks in a new $INDEX_ALLOCATION attribute, then sets $INDEX_ROOT to
// an empty root node and adds $BITMAP.
func (v *ntfsVolume) spillToIndexAlloc(dirRec []byte, dirMFTNum uint64, entries [][]byte, idxRootHdr []byte) error {
	blockSize := v.idxBlockSize
	usable := indxUsableSpace(blockSize)

	// Pack entries into blocks, filling each block before starting the next.
	type blockData struct{ raw []byte }
	var blocks []blockData
	var cur [][]byte
	var curBytes int
	for _, e := range entries {
		if int64(curBytes+len(e)) > usable && len(cur) > 0 {
			blocks = append(blocks, blockData{raw: flattenEntries(cur)})
			cur = nil
			curBytes = 0
		}
		cur = append(cur, e)
		curBytes += len(e)
	}
	// Final (possibly partial) block — always create at least one block.
	blocks = append(blocks, blockData{raw: flattenEntries(cur)})

	// Prefer a single contiguous run so the runlist stays as small as possible.
	runs, err := v.allocContiguousClusters(int64(len(blocks)))
	if err != nil {
		return err
	}

	// Write INDX blocks.
	for vcn, b := range blocks {
		lcn := lcnOfVCN(runs, int64(vcn))
		if lcn < 0 {
			return fmt.Errorf("spillToIndexAlloc: no LCN for VCN %d", vcn)
		}
		block := buildINDXBlock(int64(vcn), b.raw, blockSize)
		if _, err := v.partWrite(block, lcn*v.clusterSize); err != nil {
			return fmt.Errorf("spillToIndexAlloc: write INDX VCN %d: %w", vcn, err)
		}
	}

	// Rebuild the directory MFT record.
	dirRec = cloneRecord(dirRec)

	// Replace $INDEX_ROOT with an empty root node (entries_offset=16, just sentinel).
	dirRec = removeAttr(dirRec, attrINDEX_ROOT, "$I30")
	if findAttrOffset(dirRec, attrINDEX_ROOT, "$I30") < 0 {
		dirRec = removeAttr(dirRec, attrINDEX_ROOT, "")
	}
	emptyRoot := buildEmptyIndexRootVal(idxRootHdr)
	off := nextAttrOffset(dirRec, int(binary.LittleEndian.Uint16(dirRec[0x14:])))
	dirRec = appendNamedResidentAttrAt(dirRec, off, attrINDEX_ROOT, "$I30", emptyRoot)

	// Add $INDEX_ALLOCATION (non-resident, named "$I30").
	totalBytes := int64(len(blocks)) * blockSize
	dirRec = appendNonResidentNamedAttr(dirRec, attrINDEX_ALLOCATION, "$I30",
		runs, totalBytes, v.clusterSize)

	// Add $BITMAP (resident, named "$I30"): one bit per INDX block, all set.
	bitmapVal := make([]byte, (len(blocks)+7)/8)
	for i := range blocks {
		bitmapVal[i/8] |= 1 << uint(i%8)
	}
	off = nextAttrOffset(dirRec, int(binary.LittleEndian.Uint16(dirRec[0x14:])))
	dirRec = appendNamedResidentAttrAt(dirRec, off, attrBITMAP, "$I30", bitmapVal)

	if len(dirRec) > int(v.recSize) {
		return fmt.Errorf("spillToIndexAlloc: dir %d MFT record still too large (%d bytes) after spill", dirMFTNum, len(dirRec))
	}
	return v.putRecord(dirMFTNum, dirRec)
}

// repackIndexAlloc consolidates a fragmented $INDEX_ALLOCATION into a single
// contiguous run. It reads all numBlocks existing INDX blocks into memory,
// frees the old clusters, allocates a fresh contiguous range for
// numBlocks + 1 clusters, rewrites all existing blocks at their new
// positions, and writes a new block containing newEntry.
//
// This is called from appendEntryToIndexAlloc when the directory MFT record
// would overflow recSize due to a long fragmented runlist.  By freeing old
// clusters before the new allocation, we maximise the chance of finding a
// contiguous range that covers everything in one run.
func (v *ntfsVolume) repackIndexAlloc(oldRuns []run, numBlocks int64, newEntry []byte, blockSize int64) ([]run, error) {
	// 1. Read all existing INDX blocks before touching the bitmap.
	saved := make([][]byte, numBlocks)
	for vcn := int64(0); vcn < numBlocks; vcn++ {
		lcn := lcnOfVCN(oldRuns, vcn)
		if lcn < 0 {
			return nil, fmt.Errorf("repackIndexAlloc: no LCN for existing VCN %d", vcn)
		}
		b := make([]byte, blockSize)
		if _, err := v.partRead(b, lcn*v.clusterSize); err != nil {
			return nil, fmt.Errorf("repackIndexAlloc: read VCN %d: %w", vcn, err)
		}
		saved[vcn] = b
	}

	// 2. Release old clusters so they become candidates for the new range.
	v.freeRuns(oldRuns)

	// 3. Allocate a fresh range: all existing blocks + 1 new block.
	totalBlocks := numBlocks + 1
	newRuns, err := v.allocContiguousClusters(totalBlocks)
	if err != nil {
		// Contiguous allocation failed (very fragmented volume); fall back to
		// fragmented allocation — runlist will still be shorter than before
		// because all blocks now originate from a single fresh allocation.
		newRuns, err = v.allocClusters(totalBlocks)
		if err != nil {
			return nil, fmt.Errorf("repackIndexAlloc: alloc %d clusters: %w", totalBlocks, err)
		}
	}

	// 4. Rewrite all existing blocks at their new LCNs.
	for vcn := int64(0); vcn < numBlocks; vcn++ {
		lcn := lcnOfVCN(newRuns, vcn)
		if lcn < 0 {
			return nil, fmt.Errorf("repackIndexAlloc: no new LCN for VCN %d", vcn)
		}
		if _, err := v.partWrite(saved[vcn], lcn*v.clusterSize); err != nil {
			return nil, fmt.Errorf("repackIndexAlloc: rewrite VCN %d: %w", vcn, err)
		}
	}

	// 5. Write the new block.
	newVCN := numBlocks
	newLCN := lcnOfVCN(newRuns, newVCN)
	if newLCN < 0 {
		return nil, fmt.Errorf("repackIndexAlloc: no LCN for new VCN %d", newVCN)
	}
	newBlock := buildINDXBlock(newVCN, flattenEntries([][]byte{newEntry}), blockSize)
	if _, err := v.partWrite(newBlock, newLCN*v.clusterSize); err != nil {
		return nil, fmt.Errorf("repackIndexAlloc: write new block: %w", err)
	}

	return newRuns, nil
}

// rebuildDirRecIndexAlloc replaces the $INDEX_ALLOCATION and $BITMAP attributes
// on a cloned directory record with fresh values derived from runs and newBitmap.
// Returns the (possibly grown) record slice; callers must check len vs recSize.
func rebuildDirRecIndexAlloc(dirRec []byte, runs []run, totalBytes int64, newBitmap []byte, clusterSize int64) []byte {
	dirRec = removeAttr(dirRec, attrINDEX_ALLOCATION, "$I30")
	if findAttrOffset(dirRec, attrINDEX_ALLOCATION, "$I30") < 0 {
		dirRec = removeAttr(dirRec, attrINDEX_ALLOCATION, "")
	}
	dirRec = removeAttr(dirRec, attrBITMAP, "$I30")
	if findAttrOffset(dirRec, attrBITMAP, "$I30") < 0 {
		dirRec = removeAttr(dirRec, attrBITMAP, "")
	}
	dirRec = appendNonResidentNamedAttr(dirRec, attrINDEX_ALLOCATION, "$I30",
		runs, totalBytes, clusterSize)
	off := nextAttrOffset(dirRec, int(binary.LittleEndian.Uint16(dirRec[0x14:])))
	dirRec = appendNamedResidentAttrAt(dirRec, off, attrBITMAP, "$I30", newBitmap)
	return dirRec
}

// appendEntryToIndexAlloc adds a single entry to the last INDX block in an
// existing $INDEX_ALLOCATION, growing the allocation by one cluster if the
// last block is full.
//
// When a new block is needed and the resulting directory MFT record would
// exceed recSize (because a long fragmented runlist pushes the
// $INDEX_ALLOCATION attribute past the limit), the entire allocation is
// repacked into a single contiguous run via repackIndexAlloc before the
// directory record is written.
func (v *ntfsVolume) appendEntryToIndexAlloc(dirRec []byte, dirMFTNum uint64, entry []byte) error {
	// Locate $INDEX_ALLOCATION attribute.
	iaOff := findAttrOffset(dirRec, attrINDEX_ALLOCATION, "$I30")
	if iaOff < 0 {
		iaOff = findAttrOffset(dirRec, attrINDEX_ALLOCATION, "")
	}
	if iaOff < 0 {
		return fmt.Errorf("appendEntryToIndexAlloc: directory %d has no $INDEX_ALLOCATION", dirMFTNum)
	}
	iaLen := int(binary.LittleEndian.Uint32(dirRec[iaOff+4:]))
	iaAttr := dirRec[iaOff : iaOff+iaLen]

	rlOff := int(binary.LittleEndian.Uint16(iaAttr[0x20:]))
	totalBytes := int64(binary.LittleEndian.Uint64(iaAttr[0x30:]))

	runs, err := decodeRunlist(iaAttr[rlOff:])
	if err != nil {
		return fmt.Errorf("appendEntryToIndexAlloc: decode runlist: %w", err)
	}

	blockSize := v.idxBlockSize
	numBlocks := totalBytes / blockSize

	// Read the last INDX block.
	lastVCN := numBlocks - 1
	lastLCN := lcnOfVCN(runs, lastVCN)
	if lastLCN < 0 {
		return fmt.Errorf("appendEntryToIndexAlloc: cannot find LCN for VCN %d", lastVCN)
	}
	block := make([]byte, blockSize)
	if _, err := v.partRead(block, lastLCN*v.clusterSize); err != nil {
		return fmt.Errorf("appendEntryToIndexAlloc: read last INDX block: %w", err)
	}
	applyUSA(block)

	// ── fast path: entry fits in the current last block ───────────────────
	if string(block[:4]) == "INDX" && addEntryToINDXBlock(block, entry) {
		stampUSA(block, int(blockSize/512))
		if _, err := v.partWrite(block, lastLCN*v.clusterSize); err != nil {
			return fmt.Errorf("appendEntryToIndexAlloc: write INDX block: %w", err)
		}
		return nil
	}

	// ── slow path: allocate a new INDX block ─────────────────────────────
	newVCN := numBlocks
	newTotalBytes := totalBytes + blockSize

	// Read the old $BITMAP value so we can extend it.
	var oldBitmap []byte
	bmOff := findAttrOffset(dirRec, attrBITMAP, "$I30")
	if bmOff < 0 {
		bmOff = findAttrOffset(dirRec, attrBITMAP, "")
	}
	if bmOff >= 0 {
		bvLen := int(binary.LittleEndian.Uint32(dirRec[bmOff+0x10:]))
		bvOff := bmOff + int(binary.LittleEndian.Uint16(dirRec[bmOff+0x14:]))
		oldBitmap = dirRec[bvOff : bvOff+bvLen]
	}
	newBitmap := growBitmap(oldBitmap, int(newVCN))

	// Prefer contiguous allocation to keep the runlist short.
	newRuns, err := v.allocContiguousClusters(1)
	if err != nil {
		return err
	}
	mergedRuns := mergeRuns(runs, newRuns)

	// Build a candidate directory record to check whether it fits in recSize.
	// This is cheap — we do it before any irreversible disk writes.
	candidate := rebuildDirRecIndexAlloc(
		cloneRecord(dirRec), mergedRuns, newTotalBytes, newBitmap, v.clusterSize,
	)

	if len(candidate) > int(v.recSize) {
		// The runlist is too fragmented to fit even after contiguous allocation.
		// Free the just-reserved cluster and consolidate the entire allocation.
		v.freeRuns(newRuns)

		mergedRuns, err = v.repackIndexAlloc(runs, numBlocks, entry, blockSize)
		if err != nil {
			return fmt.Errorf("appendEntryToIndexAlloc: repack: %w", err)
		}

		// Rebuild the candidate with the compact runlist from repack.
		// repackIndexAlloc already wrote all INDX blocks to disk.
		candidate = rebuildDirRecIndexAlloc(
			cloneRecord(dirRec), mergedRuns, newTotalBytes, newBitmap, v.clusterSize,
		)
		if len(candidate) > int(v.recSize) {
			return fmt.Errorf("appendEntryToIndexAlloc: dir %d record %d bytes > recSize after repack",
				dirMFTNum, len(candidate))
		}
		return v.putRecord(dirMFTNum, candidate)
	}

	// Candidate fits — write the new INDX block then commit the record.
	newLCN := newRuns[0].lcn
	newBlock := buildINDXBlock(newVCN, flattenEntries([][]byte{entry}), blockSize)
	if _, err := v.partWrite(newBlock, newLCN*v.clusterSize); err != nil {
		return fmt.Errorf("appendEntryToIndexAlloc: write new INDX block: %w", err)
	}
	return v.putRecord(dirMFTNum, candidate)
}

// removeDirEntry removes the index entry for name from its parent directory.
func (v *ntfsVolume) removeDirEntry(filePath string) error {
	parentPath := path.Dir(filePath)
	baseName := path.Base(filePath)
	parentNum, dirRec, err := v.lookupPath(parentPath)
	if err != nil {
		return err
	}
	dirRec = cloneRecord(dirRec)

	idxRootOff := findAttrOffset(dirRec, attrINDEX_ROOT, "$I30")
	if idxRootOff < 0 {
		idxRootOff = findAttrOffset(dirRec, attrINDEX_ROOT, "")
	}
	if idxRootOff < 0 {
		return nil
	}

	valOff := idxRootOff + int(binary.LittleEndian.Uint16(dirRec[idxRootOff+0x14:]))
	valLen := int(binary.LittleEndian.Uint32(dirRec[idxRootOff+0x10:]))
	if valOff+valLen > len(dirRec) {
		return nil
	}

	idxHdr := dirRec[valOff+16 : valOff+valLen]
	newHdr := deleteIndexEntry(idxHdr, baseName)

	newVal := make([]byte, 16+len(newHdr))
	copy(newVal, dirRec[valOff:valOff+16])
	copy(newVal[16:], newHdr)

	dirRec = removeAttr(dirRec, attrINDEX_ROOT, "$I30")
	if findAttrOffset(dirRec, attrINDEX_ROOT, "$I30") < 0 {
		dirRec = removeAttr(dirRec, attrINDEX_ROOT, "")
	}
	off := nextAttrOffset(dirRec, int(binary.LittleEndian.Uint16(dirRec[0x14:])))
	dirRec = appendNamedResidentAttrAt(dirRec, off, attrINDEX_ROOT, "$I30", newVal)
	return v.putRecord(parentNum, dirRec)
}

// buildIndexEntry constructs a binary $I30 index entry for a $FILE_NAME key.
func buildIndexEntry(mftNum uint64, fnKey []byte) []byte {
	keyLen := len(fnKey)
	entLen := (16 + keyLen + 7) &^ 7
	entry := make([]byte, entLen)
	binary.LittleEndian.PutUint64(entry[0:], mftNum|uint64(1)<<48)
	binary.LittleEndian.PutUint16(entry[8:], uint16(entLen))
	binary.LittleEndian.PutUint16(entry[10:], uint16(keyLen))
	copy(entry[16:], fnKey)
	return entry
}

// insertIndexEntry inserts a new index entry before the last-entry sentinel.
func insertIndexEntry(hdr []byte, entry []byte) ([]byte, error) {
	entriesOff := int(binary.LittleEndian.Uint32(hdr[0:]))
	indexLen := int(binary.LittleEndian.Uint32(hdr[4:]))
	if entriesOff > len(hdr) || indexLen > len(hdr) {
		return nil, fmt.Errorf("index header corrupt")
	}

	data := hdr[entriesOff:indexLen]
	sentinelOff := -1
	off := 0
	for off+16 <= len(data) {
		flags := binary.LittleEndian.Uint32(data[off+12:])
		if flags&idxFlagLastEntry != 0 {
			sentinelOff = entriesOff + off
			break
		}
		entLen := int(binary.LittleEndian.Uint16(data[off+8:]))
		if entLen <= 0 {
			break
		}
		off += entLen
	}
	if sentinelOff < 0 {
		return nil, fmt.Errorf("index has no last-entry sentinel")
	}

	newHdr := make([]byte, len(hdr)+len(entry))
	copy(newHdr, hdr[:sentinelOff])
	copy(newHdr[sentinelOff:], entry)
	copy(newHdr[sentinelOff+len(entry):], hdr[sentinelOff:])

	newIndexLen := indexLen + len(entry)
	binary.LittleEndian.PutUint32(newHdr[4:], uint32(newIndexLen))
	binary.LittleEndian.PutUint32(newHdr[8:], uint32(newIndexLen))
	return newHdr, nil
}

// deleteIndexEntry removes the entry with the given name from an INDEX_HEADER body.
func deleteIndexEntry(hdr []byte, name string) []byte {
	entriesOff := int(binary.LittleEndian.Uint32(hdr[0:]))
	indexLen := int(binary.LittleEndian.Uint32(hdr[4:]))
	if entriesOff > len(hdr) || indexLen > len(hdr) {
		return hdr
	}
	data := hdr[entriesOff:indexLen]
	off := 0
	for off+16 <= len(data) {
		flags := binary.LittleEndian.Uint32(data[off+12:])
		if flags&idxFlagLastEntry != 0 {
			break
		}
		entLen := int(binary.LittleEndian.Uint16(data[off+8:]))
		keyLen := int(binary.LittleEndian.Uint16(data[off+10:]))
		if entLen <= 0 {
			break
		}
		if keyLen >= 66 {
			key := data[off+16 : off+16+keyLen]
			nl := int(key[64])
			if 66+nl*2 <= len(key) {
				u16 := make([]uint16, nl)
				for i := range u16 {
					u16[i] = binary.LittleEndian.Uint16(key[66+i*2:])
				}
				if strings.EqualFold(string(utf16.Decode(u16)), name) {
					absOff := entriesOff + off
					newHdr := make([]byte, len(hdr)-entLen)
					copy(newHdr, hdr[:absOff])
					copy(newHdr[absOff:], hdr[absOff+entLen:])
					newIdxLen := indexLen - entLen
					binary.LittleEndian.PutUint32(newHdr[4:], uint32(newIdxLen))
					binary.LittleEndian.PutUint32(newHdr[8:], uint32(newIdxLen))
					return newHdr
				}
			}
		}
		off += entLen
	}
	return hdr
}
