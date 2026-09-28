# Changelog

All notable changes to this project will be documented in this file. The GitHub
release workflow reads the topmost `## [vX.Y.Z]` section into the release notes;
keep the newest version at the top.

## [Unreleased]

### Added

- The image selection flags are named for what they take: `--tag` (a name:
  a full repo tag, `name:tag`, a repository name or a bare tag) and `--index`
  (a position in the manifest), with `-t/--repo-tag` and `-i/--image-index`
  kept as equivalent spellings. `--image`, which accepted both a number and a
  name and so duplicated `--tag`, is gone; the long forms are hyphenated
  (`--repo-tag`, `--image-index`, `--buffer-size`) everywhere, CLI, HTTP and
  MCP alike.
- `cat` accepts `-b/--buffer-size` like `cp` and `extract` do; the flag
  descriptions for the image selection now say what actually works (a full
  RepoTag, `name:tag`, a repository name or a bare tag) instead of naming only
  `RepoTags`.
- The help and both READMEs were corrected: the `info` output examples match
  what is printed now, the example block is aligned at render time, the
  supported-input list mentions cabs inside `.msi` and `.ppkg`, and the cache
  location is stated in `--help` as well.

- Selecting an image no longer requires the full repo tag. `--tag` (and
  `-t`/`--repo-tag`, which resolve the same way) accepts any name an image is
  listed under — a full repo tag, the tail of its path (`chatin/safeline-mgt:latest`),
  `name:tag`, a repository name, or a bare tag — as long as it is unambiguous; a
  name matching several images is refused with the list of candidates, so a
  selection is never a guess. `--image` takes an index like `-i` and a name or
  tag as well.

### Changed

- Everything udf derives or unpacks now lives in one per-user directory inside
  the system temporary directory — `<temp>/ejfkdev/udf` (`/var/folders/…/T/…`
  on macOS, `/tmp` on Linux, `%TEMP%` on Windows), with `UDF_CACHE_DIR`
  overriding the location. Derived values (`*.json`, `idx/*.idx`) and per-run
  scratch (`tmp/*`, the disks unpacked from an OVA/VMA/VM export) sit under the
  same root, so each system's own reclamation applies (macOS purges entries
  unused for three days, Linux clears /tmp at boot or after ten days) and udf
  prunes what the OS leaves behind — derived values after seven days without
  being rewritten, interrupted scratch after a day — so nothing grows without
  bound on Windows either. Deleting the directory by hand is always safe: every
  entry is rebuilt from its input, and scratch is removed by the run that made
  it.

## [v0.7.2] - 2026-09-28

### Added

- `info` on an archive that holds several images now prints what the file is
  instead of asking for a selection: one line per image, with the repo tags to
  pass to `-t`, the config's `os`, `architecture` and `created`, the layer count
  and how much space the layers take in the archive. `--json` returns the same
  as an array, and `-t`/`-i` still answer with one image's detail. Selecting an
  image is still required where a target is genuinely needed (`ls`, `cp`,
  `cat`, `extract`), and that error keeps listing the available tags.
- The single-image detail gained `os`, `created`, `docker_version`,
  `layer_count` and `size`, so a machine can be identified from the config
  without unpacking anything.

### Changed

- The `info` result is two shapes now: one image's detail, or the disk/plain
  archive summary. Neither carries the other's empty fields, in the text form
  or in JSON.


## [v0.7.1] - 2026-09-28

### Fixed

- `info` (and every command that reads the image metadata) no longer decompresses
  the whole archive when the index already answered. Asking a multi-image
  archive for one of its images without `-t`/`-i` is an error, and that error —
  or any other error that reading would produce again — was treated as "the
  index cannot serve this" and retried through the sequential reader, so each
  call cost an index pass plus a full decompression and the error was never
  cached. On the 2.1 GB safeline archive: the first `info` 29.6s and its repeat
  18.2s, now 11.6s and 0.08s. A genuine index failure (a stale or damaged index)
  still falls back.
- Input classification no longer misreads an image as a plain archive when the
  index cannot read `manifest.json`: a failed read now falls through to the
  sequential probe instead of reporting "no manifest.json here".


## [v0.7.0] - 2026-09-28

### Added

- Random-access index for large gzip image archives. The first command that
  needs random layer access scans the archive once with a block-aware deflate
  scanner and caches a small index (~5 MiB for a 2 GiB archive) holding restart
  checkpoints at deflate block boundaries plus the tar entry directory; later
  commands restart the decompressor at the nearest checkpoint instead of
  decompressing everything before it, and layers are read in parallel.
  Measured on a 2.1 GB docker-save `tar.gz` (26 images, 5.5 GB decompressed,
  14 layers): `ls` 18.6s -> 0.8s, single-file `cp` 35s -> 1.3s, `cat` 23s ->
  1.0s, whole-image `extract` 2m31s -> 4.9s once the index exists (the build
  costs one extra pass, ~11s for this archive).
  Checkpoints are recorded only on byte-aligned deflate block boundaries — a
  stored block consumes the rest of its byte, so a bit-shifted restart would
  shift that grid and diverge a few MiB later — and every checkpoint is
  verified against a 64 KiB context before it is stored. Reads fall back to
  the sequential paths whenever the index is missing, unusable or fails.
- The index build also parses the layers while it scans. A docker-save or OCI
  archive is a tar of tars, and the layer directories are wanted right after
  the build; the scan now captures the header blocks of the members that are
  themselves tars and replays them into `archive/tar` (file contents are
  skipped, never copied nor decompressed twice), so the merged tree is built
  from bytes that were already in hand. On the 2.1 GB archive the tree phase
  goes from 616ms (re-reading the layers through the index, in parallel) to
  under a millisecond, and the first `ls` from 13.0s to 12.4s. Captures are
  deliberately best-effort and in-memory only: members whose stream cannot be
  replayed faithfully (a PAX size override, GNU sparse files, a broken header)
  or that exceed the capture budget are read through the index as before, and
  a differential test compares the captured tree with the index tree entry for
  entry.
- Input classification, image metadata (`manifest.json`, the selected config)
  and `ls` now go through the same index: on a large gzip archive these
  otherwise decompress the whole stream once each (the manifest sits last), so
  the first command on an archive is one index pass instead of several full
  decompressions. On the same archive: first `cat`/`cp` 109s -> 15s, `info`
  45s -> 12.6s, `ls` 17.5s -> 13.2s; every later command is unchanged and
  still sub-second.

### Changed

- The block-aware deflate scanner is about twice as fast (2.1 GB / 5.5 GB
  stream: 34s -> 11s, 260 -> 490 MB/s of output; ~1.65x the general-purpose
  flate reader on the same stream). The symbol loop now keeps the bit buffer,
  input position and output length in locals, refills 48 bits at a time with
  word-sized loads instead of per-code calls, decodes codes longer than the
  fast table straight off the bit register, writes literals and matches into
  the output buffer directly behind one capacity check per symbol group,
  copies short matches with a single 8-byte load/store instead of a `memmove`
  call, and reuses its code tables across the tens of thousands of deflate
  blocks a layer contains. On Unix the compressed stream is decoded straight
  out of a read-only file mapping, which avoids copying it through a read
  buffer first.
- A batch of archives given to `extract` is extracted with one worker per core
  (at most eight, `UDF_EXTRACT_WORKERS` overrides), since archives are
  independent inputs: three 2.1 GB archives go from 49s to 17s. Results stay in
  input order, and a batch whose members share a file name — they would resolve
  to the same output directory — is kept sequential so two workers never write
  the same tree.
- Checkpoint verification during an index build runs in parallel (it is the one
  part of a build that is not tied to the serial scan).

### Fixed

- The read-only mapping of the compressed stream never actually engaged: `mmap`
  requires a page-aligned offset and a gzip stream starts a few bytes into the
  file, so every mapping attempt failed and the scanner silently fell back to
  buffered reads. The mapping now starts at the enclosing page boundary and
  slices off the leading bytes, and a test covers unaligned offsets.
- Cache keys are derived from the source file's identity only — path, size,
  modification time, and change/creation time where the platform reports them.
  Nothing reads file contents to build a key, so a lookup costs one `stat`, and
  any edit that moves a timestamp lands on a fresh key.
- Derived data (listings, image metadata, archive indexes) now lives in
  `<system temporary directory>/ejfkdev/udf` instead of the user cache
  directory, so it is per-user, self-cleaning and survives no reboot;
  `UDF_CACHE_DIR` overrides the location. Nothing in it is needed for
  correctness: every entry is rebuilt from its source file.

## [v0.6.2] - 2026-09-28

### Changed

- Much faster `info`/`ls`/`cp`/`extract` on large image archives:
  - the input class (`image` vs `archive`) is cached; before, every command
    probed for `manifest.json`, which can sit at the very end of a compressed
    archive and cost a full decompression each time
  - plain-archive `info` summaries are cached as well
  - image layers are merged from a single sequential pass over the archive
    (parse every layer, then merge in manifest order) instead of one
    decompression per layer, because manifest order rarely matches archive
    order and every random access costs a full decompression
  - `cp` and `extract` write each path once, in the archive's physical order,
    with deferred hardlink resolution — no per-layer random reads
  - measured on a 2.1 GB docker-save `tar.gz` (26 images, 14 layers, 5.5 GB
    decompressed stream): `info` 17-38s -> 0.02s warm; `ls` 2m41s -> 18.6s per
    new path, 0.02s cached; single-file `cp` 2m39s -> 35s; `cp /etc` (451
    entries) 3m33s -> 36s; whole-image `extract` 2m31s -> 36s. Extracted trees
    verified byte-identical to the previous implementation

## [v0.6.1] - 2026-09-15

### Changed

- docs: add the "Built with ZCode" badge to both READMEs

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