package crc

import "testing"

func TestCRC16ArcCheckValue(t *testing.T) {
	// CRC-16/ARC's standard check value: "123456789" with a zero seed.
	if got := CRC16Arc(0, []byte("123456789")); got != 0xBB3D {
		t.Fatalf("CRC16Arc(0) = %#x, want 0xbb3d", got)
	}
	if got := CRC16Arc(0xFFFF, []byte("123456789")); got != 0x4B37 {
		t.Fatalf("CRC16Arc(0xffff) = %#x, want 0x4b37", got)
	}
}
