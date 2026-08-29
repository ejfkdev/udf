// flush.go
package ntfs

import (
	"encoding/binary"
	"fmt"

	"github.com/carbon-os/diskimg/fs"
)

// Unmount flushes all dirty state and marks the volume clean.
func (v *ntfsVolume) Unmount() error {
	v.mu.Lock()
	dirty := v.dirty
	v.mu.Unlock()
	if !dirty {
		return nil
	}
	if err := v.flushBitmap(); err != nil {
		return fmt.Errorf("ntfs.Unmount: flush $Bitmap: %w", err)
	}
	if err := v.flushMFTBitmap(); err != nil {
		return fmt.Errorf("ntfs.Unmount: flush MFT bitmap: %w", err)
	}
	if err := v.markVolumeClean(); err != nil {
		return fmt.Errorf("ntfs.Unmount: mark clean: %w", err)
	}
	v.mu.Lock()
	v.dirty = false
	v.mu.Unlock()
	return nil
}

// flushBitmap writes the in-memory cluster allocation bitmap back to $Bitmap.
func (v *ntfsVolume) flushBitmap() error {
	v.mu.Lock()
	bm := make([]byte, len(v.bitmap))
	copy(bm, v.bitmap)
	v.mu.Unlock()

	rec, err := v.getRecord(recBitmap)
	if err != nil {
		return err
	}
	attr := findAttr(rec, attrDATA, "")
	if attr == nil {
		return fmt.Errorf("$Bitmap has no $DATA attribute")
	}

	if attr[8] == 0 {
		rec = cloneRecord(rec)
		valOff := findAttrOffset(rec, attrDATA, "")
		if valOff >= 0 {
			av := valOff + int(binary.LittleEndian.Uint16(rec[valOff+0x14:]))
			copy(rec[av:], bm)
			return v.putRecord(recBitmap, rec)
		}
	}
	rlOff := int(binary.LittleEndian.Uint16(attr[0x20:]))
	if rlOff >= len(attr) {
		return fmt.Errorf("$Bitmap runlist offset out of bounds")
	}
	runs, err := decodeRunlist(attr[rlOff:])
	if err != nil {
		return err
	}
	return v.writeRunsData(runs, bm)
}

// flushMFTBitmap writes the MFT record allocation bitmap back to $MFT's $BITMAP.
//
// The $BITMAP attribute in $MFT is resident and initially holds just 3 bytes.
// As files are created, allocMFTRecord extends v.mftBitmap.  If the new bitmap
// still fits in the original resident slot we overwrite it in place.  Otherwise
// we rebuild the attribute at the larger size, staying within recSize.  If the
// bitmap has grown so large that it no longer fits in the MFT record at all, we
// write as much as possible — allocation tracking becomes approximate for very
// large volumes, but no adjacent MFT record is ever corrupted.
func (v *ntfsVolume) flushMFTBitmap() error {
	if len(v.mftBitmap) == 0 {
		return nil
	}
	v.mu.Lock()
	bm := make([]byte, len(v.mftBitmap))
	copy(bm, v.mftBitmap)
	v.mu.Unlock()

	rec, err := v.getRecord(recMFT)
	if err != nil {
		return err
	}
	attr := findAttr(rec, attrBITMAP, "")
	if attr == nil {
		return nil
	}
	if attr[8] != 0 {
		// Non-resident $BITMAP — write via runlist.
		rlOff := int(binary.LittleEndian.Uint16(attr[0x20:]))
		if rlOff >= len(attr) {
			return nil
		}
		runs, err := decodeRunlist(attr[rlOff:])
		if err != nil {
			return err
		}
		return v.writeRunsData(runs, bm)
	}

	// Resident $BITMAP.
	off := findAttrOffset(rec, attrBITMAP, "")
	if off < 0 {
		return nil
	}
	origValLen := int(binary.LittleEndian.Uint32(rec[off+0x10:]))
	av := off + int(binary.LittleEndian.Uint16(rec[off+0x14:]))

	rec = cloneRecord(rec)

	if len(bm) <= origValLen {
		// New bitmap fits inside the existing value slot: overwrite in place.
		copy(rec[av:av+len(bm)], bm)
		for i := len(bm); i < origValLen; i++ {
			rec[av+i] = 0
		}
		return v.putRecord(recMFT, rec)
	}

	// The bitmap has grown beyond the current slot.  Rebuild the attribute.
	// Compute how much space is available after removing the old $BITMAP.
	oldAttrLen := int(binary.LittleEndian.Uint32(rec[off+4:]))
	usedAfterRemove := int(binary.LittleEndian.Uint32(rec[0x18:])) - oldAttrLen
	// Resident attr header = 24 bytes; 8 bytes for end-marker.
	maxBMLen := int(v.recSize) - usedAfterRemove - 24 - 8
	if maxBMLen < origValLen {
		maxBMLen = origValLen // never shrink below the original
	}
	newBM := bm
	if len(newBM) > maxBMLen {
		newBM = newBM[:maxBMLen]
	}

	rec = removeAttr(rec, attrBITMAP, "")
	rec = appendResidentAttr(rec, attrBITMAP, newBM)
	// appendResidentAttr may grow rec via append; putRecord enforces recSize.
	if len(rec) > int(v.recSize) {
		// Should not happen given the calculation above, but guard anyway.
		rec = rec[:v.recSize]
		binary.LittleEndian.PutUint32(rec[0x18:], uint32(v.recSize))
	}
	return v.putRecord(recMFT, rec)
}

// writeRunsData writes data sequentially across the cluster runs.
func (v *ntfsVolume) writeRunsData(runs []run, data []byte) error {
	var written int64
	for _, r := range runs {
		for c := int64(0); c < r.length; c++ {
			start := written
			end := written + v.clusterSize
			if start >= int64(len(data)) {
				return nil
			}
			if end > int64(len(data)) {
				end = int64(len(data))
			}
			chunk := make([]byte, v.clusterSize)
			copy(chunk, data[start:end])
			off := (r.lcn + c) * v.clusterSize
			if _, err := v.partWrite(chunk, off); err != nil {
				return err
			}
			written += v.clusterSize
		}
	}
	return nil
}

// markVolumeClean clears the "volume dirty" flag in $Volume's $VOLUME_INFORMATION.
func (v *ntfsVolume) markVolumeClean() error {
	rec, err := v.getRecord(recVolume)
	if err != nil {
		return nil
	}
	attr := findAttr(rec, attrVOLUME_INFORMATION, "")
	if attr == nil || attr[8] != 0 {
		return nil
	}
	valOff := findAttrOffset(rec, attrVOLUME_INFORMATION, "")
	if valOff < 0 {
		return nil
	}
	av := valOff + int(binary.LittleEndian.Uint16(rec[valOff+0x14:]))
	if av+10 > len(rec) {
		return nil
	}
	rec = cloneRecord(rec)
	rec[av+8] &^= 0x01
	return v.putRecord(recVolume, rec)
}

var _ fs.Volume = (*ntfsVolume)(nil)
