// Package image is the orchestration layer: it detects an input by content,
// scans its metadata, exposes its merged filesystem view, and lists or
// extracts files. It covers virtual disk containers (qcow2/vmdk/vhd/vhdx/vdi/
// vma/ova/sif/wim/ffu/appimage/...), raw filesystem images (ext4/xfs/squashfs/
// iso9660/fat/...), and, via the archive subpackage, archive formats
// (tar/zip/7z/rar/cpio/asar/rpm/deb/oci). Detection is content-based, never
// extension-based.
package image
