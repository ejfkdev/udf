package ext4

import (
	"encoding/hex"
	"testing"

	"github.com/ejfkdev/udf/diskkit/filesystem/ext4/crc"
)

// TestGroupDescriptorChecksumRealFilesystem freezes one real-world case: the
// UUID and the first group descriptor of a system partition taken from an
// Android emulator image (android-37.1, arm64), together with the checksum the
// filesystem itself stores there.
//
// ext4's gdt_csum variant is CRC-16/ARC seeded from the UUID, then the group
// number, then the descriptor only up to the checksum field — an independent
// derivation of it reproduces 0xa2a1 for these bytes, and this test keeps the
// implementation on that value. It is a regression anchor for a bug that made
// every modern Android ext4 unreadable: the checksum was computed with the
// wrong CRC (CCITT instead of ARC), over the whole descriptor instead of its
// first 0x1e bytes, with the wrong seed.
func TestGroupDescriptorChecksumRealFilesystem(t *testing.T) {
	uuid, err := hex.DecodeString("171f9600d60257a3bb78d03a3bd39210")
	if err != nil {
		t.Fatal(err)
	}
	desc, err := hex.DecodeString("020000000300000004000000000000005d00000000000000000000000000a1a2")
	if err != nil {
		t.Fatal(err)
	}
	seed := crc.CRC16Arc(0xffff, uuid)
	if got := groupDescriptorChecksum(desc, 0, seed, 0, gdtChecksumGdt); got != 0xa2a1 {
		t.Fatalf("group descriptor checksum = %#06x, want 0xa2a1", got)
	}
}
