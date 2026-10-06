# Changelog

All notable changes to this project will be documented in this file. The GitHub
release workflow reads the topmost `## [vX.Y.Z]` section into the release notes;
keep the newest version at the top.

## [v0.7.8] - 2026-10-06

### Added

- The random-access index covers zstd-compressed tars as well as gzip ones, so
  a large `.tar.zst` stops costing a full decompression per command: one pass
  records the frame table and the member directory, and every later `info`/`ls`
  answers from it (a 3 GB `tar.zst`: 1.2 s per listing before, 13 ms after).
  Reading a member starts at the frame that holds it when the stream has
  several — what pzstd, the zstd seekable format and containerd's zstd-chunked
  layers produce — and at the beginning when it has one, which the `zstd` CLI
  and `tar --zstd` write; there the member directory still turns two decodes
  into one. The index holds no decoder windows, so it stays small (a few KB for
  a friend's frame table, a few MB for the directory of a huge archive).

### Fixed

- qcow2 images whose clusters are zstd-compressed — `qemu-img convert -c -o
  compression_type=zstd` writes them — failed outright with "unsupported
  compression type (zstd)": the qcow2 library handles deflate itself and leaves
  zstd to the caller, and udf now registers the decoder. Verified on a
  qemu-produced image of an ext4 filesystem: its superblock, group descriptors
  and inodes are read through the compressed clusters and their checksums
  verify, which is an independent check that the decompressed bytes are right.

## [v0.7.7] - 2026-10-06

### Added

- `udf verify` checks an archive's digests: the config against the digest its
  name records, each layer against the uncompressed digest the config lists
  (`rootfs.diff_ids` — the one thing both docker-save and OCI archives record),
  and, for OCI archives, the manifest against the digest the index names. A
  truncated or tampered layer fails with the digest it actually has; a
  non-distributable (foreign) layer is reported as skipped rather than failed,
  because an archive is not supposed to carry it; `--fast` only checks that
  every layer is present. Failures produce a non-zero exit and are named in the
  summary.
- `--platform` (with `--tag`, `--index`) selects an image by
  `os/arch[/variant]` — the spelling docker and OCI use — for every command
  that takes a selection. A value without a variant matches any variant, an
  ambiguous match lists the candidates instead of guessing, and a platform
  given together with an index is refused. On an archive holding several
  platforms of one tag, `--tag app:1.2 --platform linux/arm64` now picks the
  right one.
- An OCI layout whose `index.json` names several manifests now lists every one
  of them (with the tag from each descriptor's annotation), and a manifest list
  referenced from the index is followed, so multi-platform OCI layouts behave
  like docker-save archives: `info` lists the images, and the selection flags
  work across them. Previously only `manifests[0]` was read and the rest were
  dropped silently.
- `info` reports what the manifest records: `manifest_digest`, the distinct
  `layer_media_types` (so a zstd or foreign layer is visible), the count of
  `non_distributable_layers`, and `platform`/`variant` alongside os and
  architecture. A Docker schema 1 manifest (`fsLayers`, no diff ids) is now
  refused with a message that says so, instead of listing an image with no
  layers.

### Fixed

- zstd-compressed OCI layers (`application/vnd.oci.image.layer.v1.tar+zstd`,
  what containerd and nerdctl write with `--compression zstd`) failed to open
  with "unsupported zstd-compressed layer", so such an image could not be
  listed or extracted at all. Layers now decode by magic — gzip, zstd, xz or
  plain tar — regardless of what the archive calls them.
- A layer whose content is not in the archive because it is non-distributable
  reported "entry not found", which reads like a corrupt archive; it now says
  the layer is foreign and why.

### Changed

- The layer merge is checked against fixtures from another project
  (go-containerregistry's whiteout, whiteout-directory and overwritten-file
  archives, Apache-2.0, with attribution): the same archives its tests assert
  on now run through `udf ls`/`cp`, so the semantics are pinned to bytes udf
  did not produce.

## [v0.7.6] - 2026-10-06

### Fixed

- The deflate scanner decoded a fixed-Huffman block with the previous dynamic
  block's code tables. It built the fixed tables once and never rebuilt them,
  so the first dynamic→fixed transition in a stream produced garbage and ended
  in "corrupt deflate stream". The 26 GB XDR upgrade bundle that reported this
  makes that transition a megabyte in, which meant no random-access index could
  be built for it and every command paid a full decompression (69 s for `info`,
  and again for the next one). A hand-built stream — a dynamic block, then empty
  fixed blocks — now pins the transition.

### Changed

- Plain gzip archives — a tar.gz that is not a docker-save image, like that
  upgrade bundle — now use the random-access index as well, and every command
  reads through it: `info`, `ls`, `cp`, `cat` and `extract` no longer
  decompress the stream at all once the index exists. The index records each
  member's header fields (type, mode, modification time, owner ids and link
  target) during the same single pass that records its offset, so a listing
  needs nothing but the index. Measured on the 26 GB bundle: the first command
  builds the index in 61 s (the same one pass as before, cached at 6.8 MB),
  after which `info` takes 0.016 s, a full listing 0.02 s, `cat` of a member
  0.03 s (against 14.8 s through the stream) and `cp` of a 1.95 GB layer 2.5 s
  (against 18 s) — with output byte-identical to `tar -xzOf` (checked by md5).

## [v0.7.5] - 2026-10-06

### Fixed

- ext4 group descriptor checksums with `metadata_csum`: a Kylin/PlatOS
  appliance image (and any filesystem made by a recent `mkfs` with 64-byte
  descriptors) was refused with "checksum mismatch", so no partition on the disk
  could be read. The CRC-32C for this variant covers the **whole** descriptor
  with its checksum field zeroed — not just the bytes before it, which is the
  range the older `gdt_csum` variant uses and what v0.7.4 assumed. Both variants
  are now pinned by real filesystems in the tests: an Android `gdt_csum`
  descriptor (→ 0xa2a1) and two descriptors of a Kylin `metadata_csum` one
  (groups 0 and 7 → 0x8752 and 0x07d9).

### Changed

- A checksum that does not verify no longer makes a filesystem unreadable.
  Group descriptors, inodes and directory blocks are read as they are on the
  disk, which is what the kernel does when mounting a filesystem it cannot
  verify; a formula bug in udf must not cost someone access to their image.
  The checksum computations stay pinned by the frozen-vector tests.

## [v0.7.4] - 2026-09-28

### Added

- Android `boot.img` (header v0–v4) opens as an archive: the kernel, ramdisk,
  second stage, recovery DTBO and dtb are listed as components, and the ramdisk
  is unpacked in turn — gzip/bzip2/xz/zstd/lz4/lzma are recognized — so the
  files inside it (`ramdisk/init`, `ramdisk/default.prop`, …) list and extract
  like any other archive member. The `ANDROID!` magic is weak, so the header is
  validated before the format is claimed and a file that merely starts with
  those bytes is still reported as unknown.
- Android sparse images (magic `0xed26ff3a`) are read as a disk container:
  the raw, fill and don't-care chunks are expanded on demand, so a sparse
  `system.img` is listed, searched and extracted exactly like a raw image, with
  only the chunks a read actually touches.
- Unity AssetBundles (`UnityFS`) open as archives: the big-endian container
  header and block/node tables are parsed and each node is decompressed from
  just the blocks it overlaps, with stored, LZ4, LZ4HC and LZMA supported.
  LZHAM-compressed and encrypted bundles are refused with a message that says
  so rather than a truncated listing.
- py2exe executables open as archives. udf walks the PE resource directory
  (types, names, languages; RVAs mapped through the section table), lists every
  resource as `resource/<type>/<name>`, and decodes the bootstrap script in the
  `PYTHONSCRIPT` resource into a `.pyc` — the header carries the real CPython
  magic when the exe says which `pythonXY.dll` it uses and is left zeroed
  otherwise, which decompilers still accept. The module archive py2exe appends
  to a one-file exe is listed under `bundle/`, and detection prefers this format
  over the generic zip-SFX reading so nothing is lost.
- LZ4's legacy frame format (magic `0x184C2102`) is decompressed, so the
  Android emulator's `ramdisk.img`/`initrd` — and ramdisks inside many devices'
  `boot.img`, which are compressed the same way — list and extract their cpio
  contents. The emulator appends a bootconfig blob after the stream, which the
  decoder stops at cleanly instead of failing.
- `resources.arsc` inside an APK (or any zip) is decoded: the compiled resource
  table lists as `0x7f010000 string/app_name [default] "Magisk"`-style lines
  with the package map, resource names, configurations (language/region,
  density, SDK, night mode, …) and values — strings, colors, dimensions,
  booleans, integers — read back into text, references such as
  `@drawable/ic_logo` resolved against the table's own id map, and bag entries
  (styles) shown with their parent and each item's value. The string-pool
  reader is now shared with the AXML decoder instead of duplicated.

- Android `super` partitions — the dynamic partitions of every modern device
  and of the emulator's `system.img` — open as volumes of their own. The LP
  metadata is parsed (geometry, both metadata slots, the extent and partition
  tables) and each logical partition is read through an extent mapping, so
  `system`, `product`, `vendor`, `system_ext` and `system_dlkm` list and extract
  even when their extents are fragmented or partly unallocated. A super inside a
  disk is found by the disk reader (`p2/system`), a standalone `super.img` by
  the detector (`system`).
- Raw GPT/MBR disks are recognized as disk images. A GPT disk puts no
  filesystem magic at offset 0 — its protective MBR does not either — so an
  Android `system.img`, `vendor.img` or `encryptionkey.img` used to be reported
  as an unsupported archive. The GPT header at LBA 1, or an MBR with a usable
  partition entry, now identifies them.

- On a disk with a single filesystem, an explicit `/` lists that filesystem's
  root (a bare path still lists the volumes, so nothing else changes).
- A qcow2 that is a delta over a backing file now says so when its volumes hold
  no filesystem ("this qcow2 is a delta whose backing file is
  \"userdata-qemu.img\""), instead of a bare "no supported filesystem" that
  reads like a reader bug. The Android emulator's userdata overlay is this
  shape, and the backing file it names is the one to read.

### Fixed

- ext4 group descriptor checksums are computed the way e2fsprogs does: CRC-16
  (the ARC variant — the code had the CCITT one), seeded from the filesystem
  UUID, over the descriptor's bytes up to the checksum field. The previous
  computation also hashed the whole descriptor with the checksum zeroed, so any
  ext4 written by a modern `mkfs` (Android images included, whose `gdt_csum` is
  set) failed to open with "checksum mismatch". The metadata-csum variant, used
  by newer filesystems, is corrected the same way.
- AXML colors: the four value types are RGB8, ARGB8, RGB4 and ARGB4 — the 4-bit
  forms now expand each nibble to a full byte (and the wider forms keep their
  channel order), so decoded color values are right instead of plausible.

## [v0.7.3] - 2026-09-28

### Added

- The image selection flags are named for what they take: `--tag` for a name —
  a full repo tag, the tail of its path (`chaitin/safeline-mgt:latest`),
  `name:tag`, a repository name, or a bare tag — and `--index` for a position
  in the manifest. A name that matches several images is refused with the list
  of candidates, so a selection is never a guess. `-t`/`--repo-tag` and
  `-i`/`--image-index` are equivalent spellings; `--image`, which took both a
  number and a name and so duplicated `--tag`, is gone. Long flags are
  hyphenated everywhere — CLI, HTTP query parameters and MCP arguments alike.
- `cat` accepts `-b/--buffer-size`, which `cp` and `extract` already had (so
  the README's promise is now true).

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

### Fixed

- The help and both READMEs are corrected where they described flags or output
  that do not exist: the `info` examples match what is printed now, the example
  block is aligned at render time, the supported-input list mentions cabs
  inside `.msi` and `.ppkg`, the flag descriptions say what the selection
  actually accepts, and the cache location is stated in `--help`.

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