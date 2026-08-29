// Package lvm2 reads LVM2 physical volumes (and the volume groups on them)
// directly from a block device or disk image, without the LVM tooling. It
// parses the PV label and the text-format volume-group metadata to expose each
// logical volume as a byte range so a filesystem reader can extract it.
//
// Only the common single-PV case is supported: linear or single-stripe logical
// volumes whose physical extents all live on the one PV being read. Striped,
// mirrored and RAID logical volumes spanning multiple PVs are reported but not
// mapped to a contiguous range.
package lvm2
