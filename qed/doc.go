// Package qed implements a read-only reader for the QEMU Enhanced Disk
// (QED) format, a two-level-page-table virtual disk image. On-disk fields are
// little-endian, and allocation is tracked through an L1 table of L2 tables
// that map logical clusters to physical file offsets.
//
// The format predates and was superseded by qcow2 in QEMU; QED images with a
// backing file are rejected because backing-file resolution is not supported.
package qed
