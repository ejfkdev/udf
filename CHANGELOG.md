# Changelog

All notable changes to this project will be documented in this file. The GitHub
release workflow reads the topmost `## [vX.Y.Z]` section into the release notes;
keep the newest version at the top.

## [v0.6.0] - 2026-09-14

### Added

- PyInstaller onefile executables (CArchive overlay, PyInstaller 2.0-6.x) are
  now detected by content and can be listed and extracted like any other
  archive: entry-point scripts and modules are rebuilt into valid `.pyc` files
  (pyc header reconstruction, version-aware magic), and PYZ archives are
  expanded under `<name>_extracted/` with the same layout pyinstxtractor
  produces (verified byte-identical against its output)
- Nuitka onefile executables: appended payloads (Windows/Linux, `KA`+`X`/`Y`
  trailer format, both the ≤1.4 and 2.x entry layouts, UTF-16 names and
  symlinks) and linker-embedded payloads (macOS, located by scanning and
  validated by a full entry-stream parse); extraction verified byte-identical
  against the original dist directory
- .NET single-file applications (`PublishSingleFile`): bundle format v1/v2/v6
  including deflate-compressed entries, located via the 32-byte apphost
  signature
- ZIP self-extracting executables: PE/ELF/Mach-O/shell hosts with a ZIP
  overlay are detected via a tail EOCD scan and opened through the regular
  zip path
- Binary Android XML (AXML) inside APKs — `AndroidManifest.xml`, layouts and
  other compiled resources — is decoded to readable text XML on listing,
  `cat` and extraction (string pool UTF-8/UTF-16, namespaces, typed values
  rendered apktool-style); enum/flag names that require the resource table
  are printed as integers

### Fixed

- Character devices, block devices and FIFOs in image layers or archives
  (e.g. `dev/console`, type `3` tar entries) no longer abort listing or
  extraction with `unsupported tar entry type`; `ls` now renders them with
  `c`/`b`/`p` mode prefixes and device numbers, and extraction skips them
  (they cannot be recreated without privileges), matching the existing
  disk-image path behaviour

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