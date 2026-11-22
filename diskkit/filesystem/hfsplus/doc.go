// Package hfsplus reads HFS+ volumes ("Mac OS Extended", signatures "H+" and
// the case-sensitive "HX"): the filesystem every classic macOS disk image and
// DMG carries. Read-only: volume header, catalog B-tree, extents-overflow
// lookup and fork reads.
//
// The B-tree field offsets were verified against images written by hdiutil
// (UDZO/UDBZ/ULMO/ULFO/UDCO/UDRO, GPT/MBR/APM/no-partition layouts); see
// hfsplus_test.go for the differential checks against mounted volumes.
package hfsplus
