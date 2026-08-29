// Package qcow implements a read-only reader for the QCOW version 1 format,
// the predecessor of qcow2. It uses a two-level page table and supports both
// plain and zlib-compressed data clusters. All on-disk fields are big-endian.
//
// Encrypted images and images with a backing file are rejected, as are
// qcow2/3 images (those are handled by the go-qcow2reader-based path).
package qcow
