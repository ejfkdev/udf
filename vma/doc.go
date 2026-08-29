// Package vma reads Proxmox vzdump backup archives (.vma). A VMA holds a small
// header (device list, config blobs) followed by a sequence of extents that
// describe each device's data as sparse 64 KiB clusters, so the archive is
// deflated into one or more raw disk images for the rest of udf to walk.
package vma
