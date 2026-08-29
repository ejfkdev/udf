package image

import (
	"errors"
	"io"
)

// ffuSignature is the 12-byte signature in a Full Flash Update security header
// ("SignedImage" followed by a space), found at offset 4 (after a u32 size).
const ffuSignature = "SignedImage "

// isFFU reports whether ra begins with a Full Flash Update security header.
func isFFU(ra io.ReaderAt) bool {
	var sig [16]byte
	if _, err := ra.ReadAt(sig[:], 0); err != nil {
		return false
	}
	return string(sig[4:16]) == ffuSignature
}

// ffuStorageOffset locates the raw flash storage embedded in an FFU image. The
// storage is a whole disk (always GPT- or MBR-partitioned), so it begins with
// either a GPT header ("EFI PART" at LBA 1, i.e. 512 bytes after the storage
// start with GPT at LBA 0 of the protective MBR) or an MBR boot sector. Scan
// the leading (small) security region for one of the two, at 512-byte
// alignment, and return the storage's byte offset.
func ffuStorageOffset(ra io.ReaderAt, size int64) (int64, error) {
	const maxScan = 4 << 20 // FFU security headers are small
	limit := size
	if limit > maxScan {
		limit = maxScan
	}
	var blk [512]byte
	for off := int64(0); off+512 <= limit; off += 512 {
		if _, err := ra.ReadAt(blk[:], off); err != nil {
			break
		}
		if off >= 512 && string(blk[0:8]) == "EFI PART" {
			return off - 512, nil
		}
		if blk[510] == 0x55 && blk[511] == 0xaa && mbrHasPartition(blk[:]) {
			return off, nil
		}
	}
	return 0, errors.New("ffu: could not locate the embedded storage")
}

// mbrHasPartition reports whether an MBR boot sector declares at least one
// partition (a non-zero partition-type byte in one of the four entries).
func mbrHasPartition(mbr []byte) bool {
	for i := 446 + 4; i < 446+64; i += 16 {
		if mbr[i] != 0 {
			return true
		}
	}
	return false
}
