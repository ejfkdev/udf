// write.go
package ntfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"time"
	"unicode/utf16"

	diskfs "github.com/carbon-os/diskimg/fs"
)

// WriteFile writes data to name, creating or truncating the file as needed.
func (v *ntfsVolume) WriteFile(name string, data []byte, perm fs.FileMode) error {
	mftNum, rec, err := v.lookupPath(name)
	if errors.Is(err, fs.ErrNotExist) {
		mftNum, rec, err = v.createFile(name, perm)
	}
	if err != nil {
		return &fs.PathError{Op: "writefile", Path: name, Err: err}
	}
	if flags := binary.LittleEndian.Uint16(rec[0x16:]); flags&mftFlagDir != 0 {
		return &fs.PathError{Op: "writefile", Path: name, Err: fmt.Errorf("is a directory")}
	}
	return v.writeFileData(mftNum, rec, data)
}

// writeFileData writes data as the unnamed $DATA attribute of the record at mftNum.
func (v *ntfsVolume) writeFileData(mftNum uint64, rec []byte, data []byte) error {
	rec = cloneRecord(rec)
	rec = removeAttr(rec, attrDATA, "")

	if int64(len(data)) <= maxResidentDataSize(rec) {
		rec = appendResidentAttr(rec, attrDATA, data)
	} else {
		runs, err := v.allocAndWrite(data)
		if err != nil {
			return err
		}
		rec = appendNonResidentAttr(rec, attrDATA, runs, int64(len(data)), v.clusterSize)
	}
	binary.LittleEndian.PutUint32(rec[0x18:], uint32(len(rec)))
	setStdInfoMtime(rec, time.Now())
	return v.putRecord(mftNum, rec)
}

// flushFileWrite commits buffered write data from an ntfsFile to disk.
func (v *ntfsVolume) flushFileWrite(f *ntfsFile) error {
	rec, err := v.getRecord(f.mftNum)
	if err != nil {
		return err
	}
	return v.writeFileData(f.mftNum, rec, f.writeBuf)
}

// truncateFile removes the $DATA attribute from a file (truncates to zero).
func (v *ntfsVolume) truncateFile(mftNum uint64, rec []byte) error {
	rec = cloneRecord(rec)
	if dataAttr := findAttr(rec, attrDATA, ""); dataAttr != nil && dataAttr[8] != 0 {
		rlOff := int(binary.LittleEndian.Uint16(dataAttr[0x20:]))
		if rlOff < len(dataAttr) {
			if runs, err := decodeRunlist(dataAttr[rlOff:]); err == nil {
				v.freeRuns(runs)
			}
		}
	}
	rec = removeAttr(rec, attrDATA, "")
	return v.putRecord(mftNum, rec)
}

// Mkdir creates a directory at name with the given permissions.
func (v *ntfsVolume) Mkdir(name string, perm fs.FileMode) error {
	if _, _, err := v.lookupPath(name); err == nil {
		return &fs.PathError{Op: "mkdir", Path: name, Err: fs.ErrExist}
	}
	_, _, err := v.createDir(name, perm)
	return err
}

// MkdirAll creates name and any missing parent directories.
func (v *ntfsVolume) MkdirAll(name string, perm fs.FileMode) error {
	if _, _, err := v.lookupPath(name); err == nil {
		return nil
	}
	parent := path.Dir(name)
	if parent != "/" && parent != name {
		if err := v.MkdirAll(parent, perm); err != nil {
			return err
		}
	}
	if err := v.Mkdir(name, perm); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return nil
}

// Remove removes the file or empty directory at name.
func (v *ntfsVolume) Remove(name string) error {
	mftNum, rec, err := v.lookupPath(name)
	if err != nil {
		return &fs.PathError{Op: "remove", Path: name, Err: err}
	}
	flags := binary.LittleEndian.Uint16(rec[0x16:])
	if flags&mftFlagDir != 0 {
		entries, _ := v.readDirEntries(rec, mftNum)
		if len(entries) > 0 {
			return &fs.PathError{Op: "remove", Path: name, Err: fmt.Errorf("directory not empty")}
		}
	}
	return v.unlinkEntry(mftNum, rec, name)
}

// RemoveAll removes name and all children recursively.
func (v *ntfsVolume) RemoveAll(name string) error {
	mftNum, rec, err := v.lookupPath(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	flags := binary.LittleEndian.Uint16(rec[0x16:])
	if flags&mftFlagDir != 0 {
		entries, _ := v.readDirEntries(rec, mftNum)
		for _, e := range entries {
			child := path.Join(name, e.Name())
			if err := v.RemoveAll(child); err != nil {
				return err
			}
		}
	}
	return v.unlinkEntry(mftNum, rec, name)
}

// unlinkEntry marks an MFT record as not in use and removes its directory entry.
func (v *ntfsVolume) unlinkEntry(mftNum uint64, rec []byte, name string) error {
	if dataAttr := findAttr(rec, attrDATA, ""); dataAttr != nil && dataAttr[8] != 0 {
		rlOff := int(binary.LittleEndian.Uint16(dataAttr[0x20:]))
		if rlOff < len(dataAttr) {
			if runs, err := decodeRunlist(dataAttr[rlOff:]); err == nil {
				v.freeRuns(runs)
			}
		}
	}
	rec = cloneRecord(rec)
	f := binary.LittleEndian.Uint16(rec[0x16:])
	binary.LittleEndian.PutUint16(rec[0x16:], f&^mftFlagInUse)
	if err := v.putRecord(mftNum, rec); err != nil {
		return err
	}
	v.mu.Lock()
	freeMFTSlot(v.mftBitmap, mftNum)
	v.dirty = true
	v.mu.Unlock()
	return v.removeDirEntry(name)
}

// Rename renames (moves) oldpath to newpath.
func (v *ntfsVolume) Rename(oldpath, newpath string) error {
	mftNum, rec, err := v.lookupPath(oldpath)
	if err != nil {
		return &fs.PathError{Op: "rename", Path: oldpath, Err: err}
	}
	if err := v.removeDirEntry(oldpath); err != nil {
		return err
	}
	newName := path.Base(newpath)
	newParentPath := path.Dir(newpath)
	newParentNum, _, err := v.lookupPath(newParentPath)
	if err != nil {
		return &fs.PathError{Op: "rename", Path: newpath, Err: err}
	}
	rec = cloneRecord(rec)
	updateFileName(rec, newName, newParentNum)
	if err := v.putRecord(mftNum, rec); err != nil {
		return err
	}
	return v.addDirEntry(newParentNum, newName, mftNum, binary.LittleEndian.Uint16(rec[0x16:])&mftFlagDir != 0)
}

// Symlink creates a symbolic link at newname pointing to oldname.
func (v *ntfsVolume) Symlink(oldname, newname string) error {
	mftNum, rec, err := v.createFile(newname, 0644)
	if err != nil {
		return &fs.PathError{Op: "symlink", Path: newname, Err: err}
	}
	rec = cloneRecord(rec)
	rp := buildReparsePoint(reparseSymlink, oldname)
	rec = appendResidentAttr(rec, attrREPARSE_POINT, rp)
	binary.LittleEndian.PutUint32(rec[0x18:], uint32(len(rec)))
	return v.putRecord(mftNum, rec)
}

// Link creates a hard link at newname pointing to the same file as oldname.
func (v *ntfsVolume) Link(oldname, newname string) error {
	mftNum, rec, err := v.lookupPath(oldname)
	if err != nil {
		return &fs.PathError{Op: "link", Path: oldname, Err: err}
	}
	rec = cloneRecord(rec)
	lc := binary.LittleEndian.Uint16(rec[0x12:])
	binary.LittleEndian.PutUint16(rec[0x12:], lc+1)
	newName := path.Base(newname)
	newParentPath := path.Dir(newname)
	newParentNum, _, err := v.lookupPath(newParentPath)
	if err != nil {
		return &fs.PathError{Op: "link", Path: newname, Err: err}
	}
	fnAttr := buildFileNameAttr(newName, newParentNum, 0, false)
	rec = appendResidentAttr(rec, attrFILE_NAME, fnAttr)
	if err := v.putRecord(mftNum, rec); err != nil {
		return err
	}
	return v.addDirEntry(newParentNum, newName, mftNum, false)
}

// Chmod updates the file permissions via $STANDARD_INFORMATION file attributes.
func (v *ntfsVolume) Chmod(name string, mode fs.FileMode) error {
	mftNum, rec, err := v.lookupPath(name)
	if err != nil {
		return &fs.PathError{Op: "chmod", Path: name, Err: err}
	}
	rec = cloneRecord(rec)
	siStart := findAttrOffset(rec, attrSTANDARD_INFORMATION, "")
	if siStart < 0 {
		return fmt.Errorf("chmod %s: no $STANDARD_INFORMATION", name)
	}
	attrValOff := siStart + int(binary.LittleEndian.Uint16(rec[siStart+0x14:]))
	if attrValOff+0x24 > len(rec) {
		return fmt.Errorf("chmod %s: $STANDARD_INFORMATION too short", name)
	}
	attrs := binary.LittleEndian.Uint32(rec[attrValOff+0x20:])
	if mode&0200 == 0 {
		attrs |= faReadOnly
	} else {
		attrs &^= faReadOnly
	}
	binary.LittleEndian.PutUint32(rec[attrValOff+0x20:], attrs)
	return v.putRecord(mftNum, rec)
}

// Chown is a no-op on NTFS (Windows ACLs are not modelled here).
func (v *ntfsVolume) Chown(name string, uid, gid int) error { return nil }

// Chtimes updates the access and modification times on name.
func (v *ntfsVolume) Chtimes(name string, atime, mtime time.Time) error {
	mftNum, rec, err := v.lookupPath(name)
	if err != nil {
		return &fs.PathError{Op: "chtimes", Path: name, Err: err}
	}
	rec = cloneRecord(rec)
	siStart := findAttrOffset(rec, attrSTANDARD_INFORMATION, "")
	if siStart < 0 {
		return fmt.Errorf("chtimes %s: no $STANDARD_INFORMATION", name)
	}
	valOff := siStart + int(binary.LittleEndian.Uint16(rec[siStart+0x14:]))
	if valOff+0x20 > len(rec) {
		return fmt.Errorf("chtimes %s: $STANDARD_INFORMATION value too short", name)
	}
	binary.LittleEndian.PutUint64(rec[valOff+0x08:], uint64(timeToFiletime(mtime)))
	binary.LittleEndian.PutUint64(rec[valOff+0x18:], uint64(timeToFiletime(atime)))
	return v.putRecord(mftNum, rec)
}

// ── record building helpers ───────────────────────────────────────────────────

func (v *ntfsVolume) createFile(filePath string, perm fs.FileMode) (uint64, []byte, error) {
	return v.createEntry(filePath, perm, false)
}

func (v *ntfsVolume) createDir(dirPath string, perm fs.FileMode) (uint64, []byte, error) {
	return v.createEntry(dirPath, perm, true)
}

func (v *ntfsVolume) createEntry(entryPath string, perm fs.FileMode, isDir bool) (uint64, []byte, error) {
	parentPath := path.Dir(entryPath)
	baseName := path.Base(entryPath)
	parentNum, _, err := v.lookupPath(parentPath)
	if err != nil {
		return 0, nil, fmt.Errorf("parent %s: %w", parentPath, err)
	}
	mftNum, err := v.allocMFTRecord()
	if err != nil {
		return 0, nil, err
	}
	rec := buildNewRecord(mftNum, baseName, parentNum, isDir, v.recSize)
	if err := v.putRecord(mftNum, rec); err != nil {
		return 0, nil, err
	}
	if err := v.addDirEntry(parentNum, baseName, mftNum, isDir); err != nil {
		return 0, nil, err
	}
	return mftNum, rec, nil
}

func buildNewRecord(mftNum uint64, name string, parentNum uint64, isDir bool, recSize int64) []byte {
	rec := make([]byte, recSize)
	copy(rec[0:], "FILE")
	usaOff := uint16(0x30)
	usaCnt := uint16(recSize/512 + 1)
	binary.LittleEndian.PutUint16(rec[0x04:], usaOff)
	binary.LittleEndian.PutUint16(rec[0x06:], usaCnt)
	binary.LittleEndian.PutUint16(rec[0x10:], 1)
	binary.LittleEndian.PutUint16(rec[0x12:], 1)

	firstAttrOff := uint16(0x30) + usaCnt*2
	firstAttrOff = (firstAttrOff + 7) &^ 7
	binary.LittleEndian.PutUint16(rec[0x14:], firstAttrOff)

	flags := mftFlagInUse
	if isDir {
		flags |= mftFlagDir
	}
	binary.LittleEndian.PutUint16(rec[0x16:], flags)
	binary.LittleEndian.PutUint32(rec[0x1C:], uint32(recSize))
	binary.LittleEndian.PutUint32(rec[0x2C:], uint32(mftNum))

	now := timeToFiletime(time.Now())
	siData := make([]byte, 72)
	for _, off := range []int{0, 8, 16, 24} {
		binary.LittleEndian.PutUint64(siData[off:], uint64(now))
	}
	var fa uint32 = faArchive
	if isDir {
		fa = faDirectory
	}
	binary.LittleEndian.PutUint32(siData[0x20:], fa)
	rec = appendResidentAttrAt(rec, int(firstAttrOff), attrSTANDARD_INFORMATION, siData)

	fnData := buildFileNameAttr(name, parentNum, fa, isDir)
	off := nextAttrOffset(rec, int(firstAttrOff))
	rec = appendResidentAttrAt(rec, off, attrFILE_NAME, fnData)

	if isDir {
		off = nextAttrOffset(rec, off)
		idxRoot := buildEmptyIndexRoot()
		rec = appendNamedResidentAttrAt(rec, off, attrINDEX_ROOT, "$I30", idxRoot)
		off = nextAttrOffset(rec, off)
	} else {
		off = nextAttrOffset(rec, off)
	}

	binary.LittleEndian.PutUint32(rec[off:], uint32(attrEND))
	binary.LittleEndian.PutUint32(rec[off+4:], 0)

	usedSize := off + 8
	binary.LittleEndian.PutUint32(rec[0x18:], uint32(usedSize))
	return rec
}

func buildFileNameAttr(name string, parentNum uint64, fileAttrs uint32, isDir bool) []byte {
	u16 := utf16.Encode([]rune(name))
	buf := make([]byte, 66+len(u16)*2)
	binary.LittleEndian.PutUint64(buf[0:], parentNum|uint64(1)<<48)
	now := uint64(timeToFiletime(time.Now()))
	for _, off := range []int{8, 16, 24, 32} {
		binary.LittleEndian.PutUint64(buf[off:], now)
	}
	binary.LittleEndian.PutUint32(buf[56:], fileAttrs)
	buf[64] = byte(len(u16))
	buf[65] = 3
	for i, c := range u16 {
		binary.LittleEndian.PutUint16(buf[66+i*2:], c)
	}
	return buf
}

func buildEmptyIndexRoot() []byte {
	buf := make([]byte, 48)
	binary.LittleEndian.PutUint32(buf[0:], uint32(attrFILE_NAME))
	binary.LittleEndian.PutUint32(buf[4:], 1)
	binary.LittleEndian.PutUint32(buf[8:], 4096)
	buf[12] = 1
	binary.LittleEndian.PutUint32(buf[16:], 16)
	binary.LittleEndian.PutUint32(buf[20:], 32)
	binary.LittleEndian.PutUint32(buf[24:], 32)
	binary.LittleEndian.PutUint16(buf[40:], 16)
	binary.LittleEndian.PutUint32(buf[44:], uint32(idxFlagLastEntry))
	return buf
}

func buildReparsePoint(tag uint32, target string) []byte {
	u16 := utf16.Encode([]rune(target))
	pathBuf := make([]byte, len(u16)*2)
	for i, c := range u16 {
		binary.LittleEndian.PutUint16(pathBuf[i*2:], c)
	}
	dataLen := 12 + len(pathBuf)*2
	rp := make([]byte, 8+dataLen)
	binary.LittleEndian.PutUint32(rp[0:], tag)
	binary.LittleEndian.PutUint16(rp[4:], uint16(dataLen))
	d := rp[8:]
	subsLen := len(pathBuf)
	binary.LittleEndian.PutUint16(d[0:], 0)
	binary.LittleEndian.PutUint16(d[2:], uint16(subsLen))
	binary.LittleEndian.PutUint16(d[4:], uint16(subsLen))
	binary.LittleEndian.PutUint16(d[6:], uint16(subsLen))
	binary.LittleEndian.PutUint32(d[8:], 1)
	copy(d[12:], pathBuf)
	copy(d[12+subsLen:], pathBuf)
	return rp
}

func setStdInfoMtime(rec []byte, t time.Time) {
	siOff := findAttrOffset(rec, attrSTANDARD_INFORMATION, "")
	if siOff < 0 {
		return
	}
	valOff := siOff + int(binary.LittleEndian.Uint16(rec[siOff+0x14:]))
	if valOff+16 > len(rec) {
		return
	}
	binary.LittleEndian.PutUint64(rec[valOff+8:], uint64(timeToFiletime(t)))
}

func updateFileName(rec []byte, name string, parentNum uint64) {
	off := findAttrOffset(rec, attrFILE_NAME, "")
	if off < 0 {
		return
	}
	valOff := off + int(binary.LittleEndian.Uint16(rec[off+0x14:]))
	if valOff+8 > len(rec) {
		return
	}
	binary.LittleEndian.PutUint64(rec[valOff:], parentNum|uint64(1)<<48)
	u16 := utf16.Encode([]rune(name))
	rec[valOff+64] = byte(len(u16))
	for i, c := range u16 {
		if valOff+66+i*2+2 > len(rec) {
			break
		}
		binary.LittleEndian.PutUint16(rec[valOff+66+i*2:], c)
	}
}

// ── attribute record manipulation ─────────────────────────────────────────────

func assignAttrID(rec []byte, attrOff int) {
	nextID := binary.LittleEndian.Uint16(rec[0x28:])
	binary.LittleEndian.PutUint16(rec[attrOff+0x0E:], nextID)
	binary.LittleEndian.PutUint16(rec[0x28:], nextID+1)
}

func cloneRecord(rec []byte) []byte {
	c := make([]byte, len(rec))
	copy(c, rec)
	return c
}

func maxResidentDataSize(rec []byte) int64 {
	usedSize := int(binary.LittleEndian.Uint32(rec[0x18:]))
	allocSize := int(binary.LittleEndian.Uint32(rec[0x1C:])) // FIXED: Read MFT allocation size instead of volatile slice length
	return int64(allocSize-usedSize) - 24
}

func findAttrOffset(rec []byte, attrType uint32, name string) int {
	if len(rec) < 0x30 {
		return -1
	}
	off := int(binary.LittleEndian.Uint16(rec[0x14:]))
	for off+8 <= len(rec) {
		t := binary.LittleEndian.Uint32(rec[off:])
		if t == attrEND {
			break
		}
		attrLen := int(binary.LittleEndian.Uint32(rec[off+4:]))
		if attrLen <= 0 || off+attrLen > len(rec) {
			break
		}
		if t == attrType {
			if name == "" {
				return off
			}
			nameLen := int(rec[off+9])
			nameOff := int(binary.LittleEndian.Uint16(rec[off+10:]))
			if nameLen > 0 && off+nameOff+nameLen*2 <= len(rec) {
				u16 := make([]uint16, nameLen)
				for i := range u16 {
					u16[i] = binary.LittleEndian.Uint16(rec[off+nameOff+i*2:])
				}
				if string(utf16.Decode(u16)) == name {
					return off
				}
			}
		}
		off += attrLen
	}
	return -1
}

func removeAttr(rec []byte, attrType uint32, name string) []byte {
	off := findAttrOffset(rec, attrType, name)
	if off < 0 {
		return rec
	}
	attrLen := int(binary.LittleEndian.Uint32(rec[off+4:]))
	out := make([]byte, len(rec)-attrLen)
	copy(out, rec[:off])
	copy(out[off:], rec[off+attrLen:])
	used := int(binary.LittleEndian.Uint32(out[0x18:]))
	binary.LittleEndian.PutUint32(out[0x18:], uint32(used-attrLen))
	return out
}

func nextAttrOffset(rec []byte, fromOff int) int {
	off := fromOff
	for off+8 <= len(rec) {
		t := binary.LittleEndian.Uint32(rec[off:])
		if t == attrEND {
			return off
		}
		attrLen := int(binary.LittleEndian.Uint32(rec[off+4:]))
		if attrLen <= 0 {
			break
		}
		off += attrLen
	}
	return off
}

func appendResidentAttr(rec []byte, attrType uint32, value []byte) []byte {
	off := nextAttrOffset(rec, int(binary.LittleEndian.Uint16(rec[0x14:])))
	return appendResidentAttrAt(rec, off, attrType, value)
}

func appendResidentAttrAt(rec []byte, off int, attrType uint32, value []byte) []byte {
	hdrLen := 24
	attrLen := (hdrLen + len(value) + 7) &^ 7
	needed := off + attrLen + 8
	for len(rec) < needed {
		rec = append(rec, 0)
	}
	binary.LittleEndian.PutUint32(rec[off:], uint32(attrType))
	binary.LittleEndian.PutUint32(rec[off+4:], uint32(attrLen))
	rec[off+8] = 0
	binary.LittleEndian.PutUint32(rec[off+16:], uint32(len(value)))
	binary.LittleEndian.PutUint16(rec[off+20:], 24)

	assignAttrID(rec, off) // FIXED: Generate Attribute ID
	copy(rec[off+24:], value)

	endOff := off + attrLen
	binary.LittleEndian.PutUint32(rec[endOff:], uint32(attrEND))
	binary.LittleEndian.PutUint32(rec[endOff+4:], 0)
	binary.LittleEndian.PutUint32(rec[0x18:], uint32(endOff+8))
	return rec
}

func appendNamedResidentAttrAt(rec []byte, off int, attrType uint32, name string, value []byte) []byte {
	u16 := utf16.Encode([]rune(name))
	nameBytes := len(u16) * 2
	nameOff := 24
	valOff := (nameOff + nameBytes + 7) &^ 7
	attrLen := (valOff + len(value) + 7) &^ 7
	needed := off + attrLen + 8
	for len(rec) < needed {
		rec = append(rec, 0)
	}
	binary.LittleEndian.PutUint32(rec[off:], uint32(attrType))
	binary.LittleEndian.PutUint32(rec[off+4:], uint32(attrLen))
	rec[off+8] = 0
	rec[off+9] = byte(len(u16))
	binary.LittleEndian.PutUint16(rec[off+10:], uint16(nameOff))
	binary.LittleEndian.PutUint32(rec[off+16:], uint32(len(value)))
	binary.LittleEndian.PutUint16(rec[off+20:], uint16(valOff))

	assignAttrID(rec, off) // FIXED: Generate Attribute ID
	for i, c := range u16 {
		binary.LittleEndian.PutUint16(rec[off+nameOff+i*2:], c)
	}
	copy(rec[off+valOff:], value)

	endOff := off + attrLen
	binary.LittleEndian.PutUint32(rec[endOff:], uint32(attrEND))
	binary.LittleEndian.PutUint32(rec[endOff+4:], 0)
	binary.LittleEndian.PutUint32(rec[0x18:], uint32(endOff+8))
	return rec
}

func appendNonResidentAttr(rec []byte, attrType uint32, runs []run, dataSize, clusterSize int64) []byte {
	rl := encodeRunlist(runs)
	rlOff := uint16(0x40)
	attrLen := (int(rlOff) + len(rl) + 7) &^ 7

	off := nextAttrOffset(rec, int(binary.LittleEndian.Uint16(rec[0x14:])))
	needed := off + attrLen + 8
	for len(rec) < needed {
		rec = append(rec, 0)
	}

	binary.LittleEndian.PutUint32(rec[off:], uint32(attrType))
	binary.LittleEndian.PutUint32(rec[off+4:], uint32(attrLen))
	rec[off+8] = 1

	assignAttrID(rec, off) // FIXED: Generate Attribute ID

	var totalClusters int64
	for _, r := range runs {
		totalClusters += r.length
	}
	binary.LittleEndian.PutUint64(rec[off+0x18:], uint64(totalClusters-1))
	binary.LittleEndian.PutUint16(rec[off+0x20:], rlOff)
	allocSize := totalClusters * clusterSize
	binary.LittleEndian.PutUint64(rec[off+0x28:], uint64(allocSize))
	binary.LittleEndian.PutUint64(rec[off+0x30:], uint64(dataSize))
	binary.LittleEndian.PutUint64(rec[off+0x38:], uint64(dataSize))
	copy(rec[off+int(rlOff):], rl)

	endOff := off + attrLen
	binary.LittleEndian.PutUint32(rec[endOff:], uint32(attrEND))
	binary.LittleEndian.PutUint32(rec[endOff+4:], 0)
	binary.LittleEndian.PutUint32(rec[0x18:], uint32(endOff+8))
	return rec
}

// appendNonResidentNamedAttr appends a named non-resident attribute.
// Used for $INDEX_ALLOCATION "$I30" on directories.
func appendNonResidentNamedAttr(rec []byte, attrType uint32, name string, runs []run, dataSize, clusterSize int64) []byte {
	u16 := utf16.Encode([]rune(name))
	nameBytes := len(u16) * 2
	nameOff := uint16(0x40)
	rlOff := uint16((int(nameOff) + nameBytes + 7) &^ 7)
	rl := encodeRunlist(runs)
	attrLen := (int(rlOff) + len(rl) + 7) &^ 7

	var totalClusters int64
	for _, r := range runs {
		totalClusters += r.length
	}
	allocSize := totalClusters * clusterSize

	off := nextAttrOffset(rec, int(binary.LittleEndian.Uint16(rec[0x14:])))
	needed := off + attrLen + 8
	for len(rec) < needed {
		rec = append(rec, 0)
	}

	binary.LittleEndian.PutUint32(rec[off:], uint32(attrType))
	binary.LittleEndian.PutUint32(rec[off+4:], uint32(attrLen))
	rec[off+8] = 1 // non-resident
	rec[off+9] = byte(len(u16))
	binary.LittleEndian.PutUint16(rec[off+10:], nameOff)

	assignAttrID(rec, off) // FIXED: Generate Attribute ID

	binary.LittleEndian.PutUint64(rec[off+0x18:], uint64(totalClusters-1))
	binary.LittleEndian.PutUint16(rec[off+0x20:], rlOff)
	binary.LittleEndian.PutUint64(rec[off+0x28:], uint64(allocSize))
	binary.LittleEndian.PutUint64(rec[off+0x30:], uint64(dataSize))
	binary.LittleEndian.PutUint64(rec[off+0x38:], uint64(dataSize))
	for i, c := range u16 {
		binary.LittleEndian.PutUint16(rec[off+int(nameOff)+i*2:], c)
	}
	copy(rec[off+int(rlOff):], rl)

	endOff := off + attrLen
	binary.LittleEndian.PutUint32(rec[endOff:], uint32(attrEND))
	binary.LittleEndian.PutUint32(rec[endOff+4:], 0)
	binary.LittleEndian.PutUint32(rec[0x18:], uint32(endOff+8))
	return rec
}

// putRecord writes rec to MFT slot n, re-encoding the USA.
// It is a hard error for rec to be larger than recSize — callers must
// ensure MFT records never grow beyond one slot.
func (v *ntfsVolume) putRecord(n uint64, rec []byte) error {
	if len(rec) > int(v.recSize) {
		return fmt.Errorf("putRecord: MFT record %d is %d bytes, exceeds recSize %d — caller grew the record beyond one slot",
			n, len(rec), v.recSize)
	}
	encoded := make([]byte, v.recSize)
	copy(encoded, rec)
	stampUSA(encoded, int(v.recSize/512))
	if _, err := v.partWrite(encoded, v.mftOff+int64(n)*v.recSize); err != nil {
		return fmt.Errorf("write MFT record %d: %w", n, err)
	}
	v.mu.Lock()
	v.mftCache[n] = rec
	v.dirty = true
	v.mu.Unlock()
	return nil
}

var _ = diskfs.NewFile
var _ = os.O_RDWR
