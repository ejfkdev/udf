// Package vdi implements read-only access to VirtualBox Disk Images (.vdi).
// It parses the header and block-allocation map and exposes the virtual disk as
// io.ReaderAt, mirroring the layout QEMU's VDI driver reads. Compressed blocks
// (block_extra > 0) are not supported and are rejected.
package vdi
