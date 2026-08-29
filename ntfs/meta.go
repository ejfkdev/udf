package ntfs

import (
	"encoding/binary"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"time"
	"unicode/utf16"

	diskfs "github.com/carbon-os/diskimg/fs"
)

// recordInfo builds an ntfsFileInfo by reading $STANDARD_INFORMATION and
// the best available $FILE_NAME attribute from an MFT record.
func (v *ntfsVolume) recordInfo(mftNum uint64, rec []byte, name string) (*ntfsFileInfo, error) {
	recFlags := binary.LittleEndian.Uint16(rec[0x16:])
	isDir := recFlags&mftFlagDir != 0

	var size int64
	var mode fs.FileMode
	var modTime time.Time
	var fileAttrs uint32

	// $STANDARD_INFORMATION (always resident, always present).
	si := findAttr(rec, attrSTANDARD_INFORMATION, "")
	if si != nil {
		val, err := v.attrValue(rec, si)
		if err == nil && len(val) >= 0x24 {
			modTime = filetimeToTime(int64(binary.LittleEndian.Uint64(val[0x08:])))
			fileAttrs = binary.LittleEndian.Uint32(val[0x20:])
		}
	}

	// File size from the unnamed $DATA attribute.
	if !isDir {
		dataAttr := findAttr(rec, attrDATA, "")
		if dataAttr != nil {
			if dataAttr[8] == 0 {
				size = int64(binary.LittleEndian.Uint32(dataAttr[0x10:]))
			} else {
				size = int64(binary.LittleEndian.Uint64(dataAttr[0x30:]))
			}
		}
	}

	mode = fileAttrsToMode(fileAttrs, isDir)

	// Prefer the Win32 name from $FILE_NAME if the caller passed an empty name.
	if name == "" {
		name = v.bestFileName(rec)
	}

	return &ntfsFileInfo{
		name:    name,
		size:    size,
		mode:    mode,
		modTime: modTime,
		isDir:   isDir,
	}, nil
}

// bestFileName returns the Win32 (or POSIX) name from the $FILE_NAME attributes.
func (v *ntfsVolume) bestFileName(rec []byte) string {
	// Walk all $FILE_NAME attributes; prefer Win32 (ns=1 or 3) over POSIX (ns=0).
	best := ""
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
		if t == attrFILE_NAME {
			val, err := v.attrValue(rec, rec[off:off+attrLen])
			if err == nil && len(val) >= 66 {
				ns := val[65]
				if ns == 2 {
					off += attrLen
					continue
				}
				nameLen := int(val[64])
				if 66+nameLen*2 <= len(val) {
					u16 := make([]uint16, nameLen)
					for i := range u16 {
						u16[i] = binary.LittleEndian.Uint16(val[66+i*2:])
					}
					candidate := string(utf16.Decode(u16))
					if ns == 1 || ns == 3 || best == "" {
						best = candidate
					}
				}
			}
		}
		off += attrLen
	}
	return best
}

func fileAttrsToMode(fa uint32, isDir bool) fs.FileMode {
	mode := fs.FileMode(0644)
	if isDir {
		mode = fs.FileMode(0755) | fs.ModeDir
	}
	if fa&faReadOnly != 0 {
		mode &^= 0222
	}
	return mode
}

// Stat returns a FileInfo for the named path, following symlinks.
func (v *ntfsVolume) Stat(name string) (fs.FileInfo, error) {
	name, err := v.resolveSymlinks(name, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: err}
	}
	mftNum, rec, err := v.lookupPath(name)
	if err != nil {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: err}
	}
	return v.recordInfo(mftNum, rec, path.Base(name))
}

// Lstat returns a FileInfo for the named path without following symlinks.
func (v *ntfsVolume) Lstat(name string) (fs.FileInfo, error) {
	mftNum, rec, err := v.lookupPath(name)
	if err != nil {
		return nil, &fs.PathError{Op: "lstat", Path: name, Err: err}
	}
	return v.recordInfo(mftNum, rec, path.Base(name))
}

// Readlink returns the target of the symbolic link at name.
func (v *ntfsVolume) Readlink(name string) (string, error) {
	_, rec, err := v.lookupPath(name)
	if err != nil {
		return "", &fs.PathError{Op: "readlink", Path: name, Err: err}
	}
	rp := findAttr(rec, attrREPARSE_POINT, "")
	if rp == nil {
		return "", &fs.PathError{Op: "readlink", Path: name, Err: fmt.Errorf("not a symlink")}
	}
	val, err := v.attrValue(rec, rp)
	if err != nil {
		return "", err
	}
	return parseReparseTarget(val)
}

// parseReparseTarget extracts the print-name target from a $REPARSE_POINT value.
func parseReparseTarget(val []byte) (string, error) {
	if len(val) < 12 {
		return "", fmt.Errorf("reparse point value too short")
	}
	tag := binary.LittleEndian.Uint32(val[0:])
	if tag != reparseSymlink && tag != reparseJunction {
		return "", fmt.Errorf("not a symlink or junction (tag 0x%08X)", tag)
	}
	// Reparse data starts at offset 8 (after tag, data length, reserved).
	data := val[8:]
	if len(data) < 12 {
		return "", fmt.Errorf("reparse data too short")
	}
	subsOff := int(binary.LittleEndian.Uint16(data[0:]))
	subsLen := int(binary.LittleEndian.Uint16(data[2:]))
	printOff := int(binary.LittleEndian.Uint16(data[4:]))
	printLen := int(binary.LittleEndian.Uint16(data[6:]))
	// flags := binary.LittleEndian.Uint32(data[8:]) // 0=absolute, 1=relative
	buf := data[12:]

	// Prefer the print name; fall back to substitution name.
	var raw []byte
	if printOff+printLen <= len(buf) {
		raw = buf[printOff : printOff+printLen]
	} else if subsOff+subsLen <= len(buf) {
		raw = buf[subsOff : subsOff+subsLen]
	} else {
		return "", fmt.Errorf("reparse path offsets out of bounds")
	}

	u16 := make([]uint16, len(raw)/2)
	for i := range u16 {
		u16[i] = binary.LittleEndian.Uint16(raw[i*2:])
	}
	target := string(utf16.Decode(u16))
	// Convert Windows path separators to Unix.
	target = strings.ReplaceAll(target, `\`, "/")
	// Strip absolute Win32 namespace prefix "\??\".
	target = strings.TrimPrefix(target, "/??/")
	return target, nil
}

// resolveSymlinks follows symlinks up to maxDepth times, returning the final path.
func (v *ntfsVolume) resolveSymlinks(p string, depth int) (string, error) {
	const maxDepth = 40
	if depth > maxDepth {
		return "", fmt.Errorf("too many levels of symbolic links")
	}
	_, rec, err := v.lookupPath(p)
	if err != nil {
		return p, err
	}
	rp := findAttr(rec, attrREPARSE_POINT, "")
	if rp == nil {
		return p, nil
	}
	val, err := v.attrValue(rec, rp)
	if err != nil {
		return p, nil
	}
	tag := binary.LittleEndian.Uint32(val[0:])
	if tag != reparseSymlink && tag != reparseJunction {
		return p, nil
	}
	target, err := parseReparseTarget(val)
	if err != nil {
		return p, nil
	}
	if !path.IsAbs(target) {
		target = path.Join(path.Dir(p), target)
	}
	return v.resolveSymlinks(target, depth+1)
}

// StatFS returns volume space statistics.
func (v *ntfsVolume) StatFS() (diskfs.VolumeInfo, error) {
	v.mu.Lock()
	bm := v.bitmap
	v.mu.Unlock()

	var usedClusters int64
	for _, b := range bm {
		for bit := 0; bit < 8; bit++ {
			if b&(1<<uint(bit)) != 0 {
				usedClusters++
			}
		}
	}
	total := v.totalClusters * v.clusterSize
	used := usedClusters * v.clusterSize
	return diskfs.VolumeInfo{
		TotalBytes: total,
		FreeBytes:  total - used,
		UsedBytes:  used,
		BlockSize:  v.clusterSize,
	}, nil
}
