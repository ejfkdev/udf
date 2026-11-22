// Package dmg opens Apple UDIF disk images (.dmg) as a read-only virtual disk.
//
// A UDIF file ends with a 512-byte "koly" trailer that points at an XML
// property list describing the image layout. Each "blkx" entry in that list
// carries one "mish" block table: a list of chunks, each mapping a sector
// range of the virtual disk to a byte range in the data fork, compressed with
// zlib, bzip2, LZFSE, LZMA or Apple's ADC, or stored raw or zero-filled.
//
// Disk implements io.ReaderAt over that virtual disk; the filesystem probes
// and partition-table parsing then see it exactly like a raw image.
package dmg
