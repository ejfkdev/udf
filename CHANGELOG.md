# Changelog

All notable changes to this project will be documented in this file. The GitHub
release workflow reads the topmost `## [vX.Y.Z]` section into the release notes;
keep the newest version at the top.

## [v0.5.1] - 2026-08-29

### Fixed

- Single-file compressed inputs (`.gz`/`.bz2`/`.xz`/`.zst`/`.lz4` of a non-tar
  file) are no longer misclassified as compressed tar archives, so they no
  longer surface a confusing `read tar entry: unexpected EOF`
- ODC-format cpio (`070707` octal fields) is no longer reported as a readable
  cpio; only newc/crc (`070701`/`070702`) are (the reader does not parse ODC)

## [v0.5.0] - 2026-08-29

### Added

- Plain archives (tar, tar.gz/tgz, zip, 7z, rar, cpio) now list and extract
  end-to-end, plus compressed variants: `.tar.xz`/`.tar.bz2`/`.tar.zst`/`.tar.lz4`
  and `.cpio.gz`/`.cpio.xz`/`.cpio.bz2`/`.cpio.zst`/`.cpio.lz4` (incl. Linux initramfs)
- Electron ASAR archives (`.asar`)
- RPM packages (`.rpm`)
- Debian / OpenWrt packages (`.deb` / `.ipk`)
- Windows Cabinet (`.cab`, incl. MSI-embedded MSZIP cabs)
- Nix Archive (`.nar`)
- macOS installer XAR (`.xar` / `.pkg`)
- OCI image archive (single-file `.oci.tar`, e.g. `podman save --format oci`) and flatpak bundles
- AppImage (ELF-embedded SquashFS)
- btrfs filesystem
- NTFS filesystem, on any backend (qcow2/vhd/vhdx/vmdk/raw and partitions)

### Changed

- Archive container parsing and compression moved into a dedicated `image/archive`
  subpackage for a cleaner, more extensible layout

### Fixed

- OCI index-with-manifests resolution (multi-image indexes previously failed)
- Bare-filesystem partition detection when a boot sector (NTFS/FAT/exFAT) shares
  the MBR `0x55AA` signature

## [v0.4.0] - 2026-08-29

### Added

- Disk image support: qcow2/qcow1, vmdk, vhd/vhdx, vdi, qed, parallels, vma,
  ova/ovf, sif, ffu, wim/esd/swm, raw/img/ami
- Filesystems: ext2/3/4, xfs, squashfs, iso9660, udf, exfat, erofs, fat12/16/32
  (incl. LVM2 logical volumes)