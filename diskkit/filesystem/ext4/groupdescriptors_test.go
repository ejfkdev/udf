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
// TestGroupDescriptorChecksumMetadataCsumRealFilesystem freezes the other
// variant: a Kylin/PlatOS appliance image (aarch64, 64-byte descriptors,
// metadata_csum set, s_checksum_seed zero). Two descriptors are kept — group 0
// and group 7 — so the group number's part in the checksum is pinned as well.
//
// The difference that made the whole image unreadable: with metadata_csum the
// CRC-32C covers the *entire* descriptor (checksum field zeroed), not just the
// bytes before it.
func TestGroupDescriptorChecksumMetadataCsumRealFilesystem(t *testing.T) {
	uuid, err := hex.DecodeString("7b836773f0314721a466be2554ac023e")
	if err != nil {
		t.Fatal(err)
	}
	seed := crc.CRC32c(0xffffffff, uuid) // s_checksum_seed is 0 in this filesystem
	cases := []struct {
		group  uint16
		desc   string
		expect uint16
	}{
		{0, "8100000089000000910000005c6fb71f0b000400000000002c8d1a53b61f5287000000000000000000000000000000000000000000000000f372551500000000", 0x8752},
		{7, "8800000090000000910e00007f7f002000000500000000000b4c00000020d907000000000000000000000000000000000000000000000000c0fa000000000000", 0x07d9},
	}
	for _, tc := range cases {
		desc, err := hex.DecodeString(tc.desc)
		if err != nil {
			t.Fatal(err)
		}
		if got := groupDescriptorChecksum(desc, seed, 0, tc.group, gdtChecksumMetadata); got != tc.expect {
			t.Fatalf("group %d checksum = %#06x, want %#06x", tc.group, got, tc.expect)
		}
	}
}

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
