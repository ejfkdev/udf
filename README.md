# udf

[![license](https://img.shields.io/github/license/ejfkdev/udf)](./LICENSE)
[![release](https://github.com/ejfkdev/udf/actions/workflows/release.yml/badge.svg)](https://github.com/ejfkdev/udf/actions/workflows/release.yml)
[![Built with ZCode](https://img.shields.io/badge/Built%20with%20ZCode-000000.svg?style=flat&logo=data:image/svg%2bxml;base64,PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciIHdpZHRoPSIxMTgiIGhlaWdodD0iMTAwIiB2aWV3Qm94PSIwIDAgMjU2IDIxOCI+PHBhdGggZmlsbD0iI2ZmZmZmZiIgZD0iTTEzNC40IDAuMTMwMTUyTDExNi40OCAyNS42MDIyQzExMy42NjUgMjkuNTY5OSAxMDkuMDU0IDMyLjAwMTkgMTA0LjA2NCAzMi4wMDE5SDYuMzk5OVYwQzYuMzk5OSAwLjEzMDE0OSAxMzQuNCAwLjEzMDE1MiAxMzQuNCAwLjEzMDE1MloiLz48cGF0aCBmaWxsPSIjZmZmZmZmIiBkPSJNMjU2IDAuMTMwMTI3TDEwMi40MDEgMjE3LjczMkgwTDE1My41OTkgMC4xMzAxMjdIMjU2WiIvPjxwYXRoIGZpbGw9IiNmZmZmZmYiIGQ9Ik0xMjEuNjAxIDIxNy43MzJMMTM5LjY1IDE5Mi4xMzRDMTQyLjQ2NSAxODguMTY2IDE0Ny4wNzYgMTg1LjczNCAxNTIuMDY3IDE4NS43MzRIMjQ5LjYwNFYyMTcuNzM2SDEyMS42MDFWMjE3LjczMloiLz48L3N2Zz4=)](https://zcode.z.ai/)

`udf` is a Go CLI tool that extracts content from archives, virtual disk images and filesystem images, and lists (or merges into a single `rootfs`) image filesystems.

One binary speaks two faces: a traditional CLI and a networked agent. Every command is defined once ([xyz-go](https://github.com/ejfkdev/xyz-go)) and automatically available as a **CLI subcommand**, an **HTTP REST route** (with an OpenAPI document) and an **MCP tool**. All inputs are **local file paths on the machine running udf** — nothing is uploaded; the operation always happens where the program runs.

中文说明见 [README.zh-CN.md](./README.zh-CN.md).

## Table of Contents

- [Features](#features)
- [Interfaces](#interfaces)
- [Installation](#installation)
- [CLI Usage](#cli-usage)
- [Input Modes](#input-modes)
- [Subcommands](#subcommands)
- [Output Rules](#output-rules)
- [Options](#options)
- [Multi-image Archives](#multi-image-archives)
- [Generated Files](#generated-files)
- [Error Handling](#error-handling)
- [Technical Notes](#technical-notes)
- [Known Limitations](#known-limitations)
- [Use as a Library](#use-as-a-library)
- [Current Scope](#current-scope)
- [License](#license)

## Features

Three interfaces, one definition:

- CLI subcommands, an HTTP REST service (`/openapi.json` included) and an MCP tool server, generated from the same command definitions
- Local-path semantics: `archive` / `dest` are paths on the host running udf; no file uploads
- Bearer-auth, TLS and CORS built in for the network modes

Archive handling:

- Extract image archives into a merged `rootfs`
- Outer archive formats: `.tar`, `.tar.gz`, `.tgz`, `.tar.xz`, `.tar.bz2`, `.tar.zst`, `.tar.lz4`, `.zip`, `.7z`, `.rar`, `.cpio` (and `.cpio.gz`/`.cpio.xz`/`.cpio.zst`, e.g. initramfs), `.asar` (Electron), `.rpm`, `.deb`/`.ipk`, `.cab`/`.msi`-embedded cabs, `.nar` (Nix), `.xar`/`.pkg` (macOS installer) (and `.ppkg` Windows provisioning packages, an OPC/ZIP)
- Executable bundle formats: PyInstaller onefile (CArchive + PYZ, `.pyc` reconstruction), Nuitka onefile (appended and embedded payloads), .NET single-file apps (bundle v1/v2/v6, incl. deflate), ZIP self-extracting executables; binary Android XML (AXML) inside APKs — `AndroidManifest.xml`, layouts — is decoded to readable text XML automatically
- Virtual disk images: qcow2, QCOW v1, VMDK, VHD/VHDX, VDI, QED, Parallels, WIM, ESD, SWM, FFU, raw/`.img`/`.ami`, OVA, OVF, VMA, SIF, AppImage (ext4/xfs/btrfs/NTFS/squashfs/ISO9660/UDF/exFAT/EROFS/FAT, including LVM2 logical volumes)
- Common image layouts: flat `manifest.json + config.json + layers/...`, classic `docker save` (`<layer-id>/layer.tar`), OCI image layout directories, and single-file OCI image archives (`.oci.tar`, incl. flatpak bundles)
- Input may be a single archive, a glob pattern, or a directory (top level scanned)

Extraction correctness:

- Layers merged in `manifest.json` order
- Whiteout files and opaque directories handled
- Symlinks and hardlinks supported
- Fallback to file copy when the target filesystem does not support hardlinks
- Directory metadata restored after extraction

Inspection:

- List merged contents without extracting (`udf ls`, `ls -al`-style information both as a CLI table and as structured JSON)
- Extract a single file or directory only (`udf cp`)
- Show image metadata: tags, layers, working dir, entrypoint (`udf info`)
- Write a single file's raw bytes to stdout, pipe-friendly (`udf cat`)
- Hex-dump the first bytes of a file to inspect its header (`udf xxd`)

Convenience:

- Export `config.json` as readable `config.yaml`
- One error taxonomy: CLI exit code, HTTP status and MCP error code stay aligned
- Importable as a Go library

### Containers and disk images

Beyond container layer archives, `udf` can open a virtual disk image and extract
a filesystem through the same commands. `info`, `ls`, `cp` and `extract` detect
disk inputs automatically:

```text
./udf info ./disk.qcow2                        # list disks, volumes + filesystems
./udf ls ./disk.qcow2                          # list the volumes (partitions + LVs)
./udf ls ./disk.qcow2 /vg1/root                # list a volume's root directory
./udf ls ./disk.qcow2 /vg1/root/etc            # list /etc inside a volume
./udf cp ./disk.qcow2 /vg1/root/etc/hostname ./hostname
./udf extract ./disk.qcow2                     # extract every filesystem volume
./udf ls ./appliance.ova                       # list the vmdk disks in an OVA
./udf ls ./appliance.ova /disk1.vmdk           # then its volumes
```

Supported disk container formats:

- **qcow2** (v2/v3), read-only and streamed
- **VMDK** sparse extents — both monolithicSparse (flat or deflate) and
  streamOptimized
- **OVA** archives (a tar of an OVF descriptor plus one or more `.vmdk` disks)
- **OVF** standalone descriptors (`.ovf` referencing sibling `.vmdk` disks)
- **VHD/VHDX** virtual disks (fixed, dynamic and differencing)
- **QED** QEMU Enhanced Disk images (two-level page table; little-endian)
- **QCOW v1** legacy QEMU disk images (two-level page table; compressed clusters)
- **WIM** Windows imaging files (the first image; XPRESS and LZX compression)
- **ESD** Windows "Electronic Software Download" files (WIM with LZMS compression)
- **SWM** split WIM images (the first part; sibling `*.swm2/3/…` parts are read automatically)
- **FFU** Full Flash Update images (locates the embedded disk by scanning past the security header — heuristic, not validated against a real Microsoft FFU)
- **VMA** Proxmox vzdump archives (one or more raw disks)
- **SIF** Singularity container images (the system partition is extracted)
- **VDI** VirtualBox disk images
- **Parallels** disk images (`.parallels`/`.hds`)
- **raw** disk and filesystem images (`.img`/`.raw`/`.dd`/`.ext4`/`.xfs`, …) — bytes are the device itself

The virtual disk is addressed as a path tree: `/` lists disks (for a multi-disk
OVA) or volumes, `/<volume>` enters a volume, and the rest is a path inside it.
Volumes are named `p1`…`pN` for partitions and `vg/lv` for LVM logical volumes.

- MBR/GPT partition tables are parsed, plus LVM2 physical volumes in-process:
  each logical volume is exposed as a selectable volume, in addition to plain
  partitions
- Filesystems supported for extraction are **ext4**, **xfs**, **btrfs**, **NTFS**, **squashfs**, **ISO9660**, **UDF**, **exFAT**, **EROFS** and **FAT** (fat12/16/32)
- When a disk holds several filesystems, `ls` and `cp` require a volume prefix
  (or start from `/` and descend); a single filesystem volume is used
  implicitly, so `./udf ls img.qcow2 /etc` still works for simple images
- `extract` writes every filesystem volume; with several, each lands in its own
  subdirectory named after the volume
- Regular files, directories and symlinks are recreated; hardlinks become
  independent copies, and device nodes, FIFOs and sockets are skipped

## Interfaces

```bash
# CLI
./udf ls ./image.tar /etc

# HTTP — every routed command answers on the same port
./udf serve --addr 127.0.0.1:8080
curl -s 'http://127.0.0.1:8080/ls?archive=/data/image.tar&path=/etc'
curl -s -X POST 'http://127.0.0.1:8080/cp' -H 'Content-Type: application/json' \
  -d '{"archive":"/data/image.tar","source":"/etc/passwd","dest":"/tmp/passwd"}'
curl -s http://127.0.0.1:8080/openapi.json

# MCP — commands become tools (stdio / SSE / streamable HTTP)
./udf mcp stdio
./udf mcp http --addr 127.0.0.1:9000 --bearer s3cret
```

Route overview: `GET /info?archive=…`, `GET /ls?archive=…&path=…`, `POST /cp`, `GET /cat?archive=…&source=…`, `GET /xxd?archive=…&source=…`, `POST /extract`, plus `/healthz` and `/openapi.json`.

In MCP clients, register udf as a stdio server:

```json
{"command": "udf", "args": ["mcp", "stdio"]}
```

> **Security:** every path argument refers to the machine that runs udf. When you expose `serve` or `mcp http/sse` beyond the loopback interface, that means callers can read and write server-local files. Protect those modes with `--bearer`, TLS (`--tls-cert`/`--tls-key`) and CORS allowlists — see the [xyz-go](https://github.com/ejfkdev/xyz-go) built-in configuration.

## Installation

### Install with Homebrew (macOS)

```bash
brew install ejfkdev/tap/udf
```

The formula lives in the [`homebrew-udf`](https://github.com/ejfkdev/homebrew-udf) tap and packages the binaries published by this repository's release workflow.

### Install with `go install`

```bash
go install github.com/ejfkdev/udf@latest
```

### Build from source

```bash
git clone https://github.com/ejfkdev/udf.git
cd udf
go build -o udf .
```

## CLI Usage

```bash
./udf <command> [arguments]
```

Examples:

```bash
./udf info ./image.tar
./udf ls ./image.tar /etc
./udf cp ./image.tar /etc/passwd ./passwd
./udf extract ./image.tar
./udf extract -o ./output -t repo/app:latest './repo/*.tar'
./udf serve --addr 127.0.0.1:8080
./udf mcp stdio
```

`extract` is the default command, so the classic invocation keeps working:

```bash
./udf ./image.tar                    # same as: ./udf extract ./image.tar
./udf './repo/*.tar' -o ./output -t repo/app:latest
```

When using the shorthand form, put flags after the archive path; to lead with flags, name the subcommand explicitly (`./udf extract -o ./out ./image.tar`).

Built-in conveniences: `-h` per-command help, `-v` version, `--json` for machine-readable CLI output, and `completion bash|zsh|fish`.

## Input Modes

Every command takes **one input expression** for the archive:

- a single archive file (`.tar`/`.tar.gz`/`.tgz`/`.zip`/`.7z`/`.rar`/`.cpio`/`.asar`/`.rpm`/PyInstaller/Nuitka/.NET single-file executable) or a disk image (`.qcow2`/`.vmdk`/`.vhd`/`.vhdx`/`.vdi`/`.img`/`.raw`/`.dd`/`.ova`/`.vma`) — required by `info`, `ls` and `cp`
- a glob pattern, or a directory (top level scanned, not recursive) — `extract` also accepts these and expands them into a batch

Examples:

```bash
./udf extract ./images
./udf extract './images/*.tar'
./udf ls ./image.tar /etc
```

## Subcommands

### `info` — image archive metadata

```bash
./udf info ./image.tar
```

```text
index         0
total_images  1
repo_tags     [demo/app:latest]
config_path   config.json
architecture  amd64
working_dir   /app
layers        [layer1.tar layer2.tar]
```

### `ls` — list directory contents without extracting

`udf ls` builds the merged filesystem view in memory and renders it like `ls -al`, without writing anything to disk. The CLI prints an aligned table; HTTP and MCP return the same data as structured JSON.

```bash
./udf ls ./image.tar          # the image root
./udf ls ./image.tar /etc     # one directory
```

```text
name    type  mode        size  mod_time                    target
------  ----  ----------  ----  --------------------------  ------
group   file  -rw-r--r--  10    2026-08-22T00:04:25+08:00
passwd  file  -rw-r--r--  30    2026-08-22T00:04:25+08:00
```

Details:

- Layers are merged with the same whiteout and opaque-directory semantics as a full extract
- Columns carry type (`dir|file|symlink|hardlink`), mode, size, mtime and the symlink target
- Leading `/` in the path is optional; `/` or `.` lists the image root
- A file path lists that single entry

### `cp` — extract a single file or directory

```bash
./udf cp ./image.tar /etc/passwd ./passwd
./udf cp ./image.tar /etc/nginx ./nginx
./udf cp ./image.tar / ./rootfs
```

Destination semantics mirror `cp`:

- Directory source into an existing directory lands at `<dest>/<basename>`
- Directory source into a non-existent path copies into that path directly
- File or symlink source goes to `<dest>` as a file, or into `<dest>/<basename>` when `<dest>` is an existing directory
- Extracting `/` (the image root) puts the contents directly into `<dest>`
- Whiteout-processed entries are skipped, symlinks are recreated as symlinks, and hardlinks are preserved when the source stays inside the selection (content is copied otherwise)

### `cat` — write a single file to stdout

Like the OS `cat`, `udf cat` streams one file's raw bytes to stdout (binary-safe, no trailing newline), so it can be piped into other programs:

```bash
./udf cat ./image.tar /etc/passwd | grep root
./udf cat ./disk.qcow2 /p1/etc/hostname
```

Symlinks and hardlinks inside the image are followed to their target file. Prefer `cat` for small text files; for large or binary files use `xxd` to preview a bounded prefix instead of dumping everything.

### `xxd` — hex-dump a file header

Like `xxd -l N` / `hexdump -C`, `udf xxd` prints the first bytes of a file as hex + ASCII so you can identify it by its header:

```bash
./udf xxd ./image.tar /etc/passwd                        # first 256 bytes
./udf xxd ./image.tar /etc/passwd -n 64 -s 4096          # 64 bytes, skipping 4096
```

`-n/--bytes` sets the byte count (default 256) and `-s/--offset` skips bytes from the start before dumping.

### `extract` — extract the merged rootfs

```bash
./udf extract ./image.tar
./udf ./image.tar                     # extract is the default command
./udf extract -o ./output -f -t repo/app:latest './repo/*.tar'
```

`extract` expands its input expression into one or more archives and processes them in order. The result is one row per archive (table on the CLI, JSON array over HTTP/MCP), and a row carries its own message when one archive fails without stopping the rest:

```text
archive      output_dir                     layers  error
-----------  -----------------------------  ------  -----
./image.tar  /tmp/out/image                 2
```

## Output Rules

For `extract`, if `-o/--output` is not specified the output goes beside each input archive; with `-o` it goes under the given parent directory:

- single-image archive:
  - `{file_name}/`
- multi-image archive:
  - `{file_name}/{repo_tag}/`
  - if no tag exists, fallback to `{file_name}/index-{n}/`

Examples:

```text
input:  /data/demo/tempest.tar
output: /data/demo/tempest
```

```text
input:  /data/demo/bundle.tar
tags:   repo/app:1.0, repo/app:latest
output: /data/demo/bundle/repo_app_1.0
        /data/demo/bundle/repo_app_latest
```

## Options

Command flags:

- `-t, --repo-tag` — select the image by `RepoTags` from `manifest.json` (`info`, `ls`, `cp`, `extract`)
- `-i, --image-index` — select the image by its index in the `manifest.json` array (all commands)
- `-o, --output` — output parent directory (`extract`)
- `-f, --force` — write into an existing non-empty target directory (`extract`)
- `-b, --buffer-size` — file copy buffer size in bytes (`cp`, `extract`)
- `-n, --bytes` — number of bytes to dump (`xxd`, default 256)
- `-s, --offset` — skip this many bytes from the start before dumping (`xxd`)

Built-in flags come from xyz-go: `-h/--help`, `-v/--version`, `--json`, `--xyz.lang en|zh-CN` (interface language, defaults to `LANG`/`LC_ALL` detection), and `completion bash|zsh|fish`. The `serve` and `mcp` modes add `--addr`, `--bearer`, `--cors`, `--tls-cert`/`--tls-key`, `--timeout`, `--log-level` (plus `--versions` and `--session-timeout` for `mcp`) — details in the [xyz-go README](https://github.com/ejfkdev/xyz-go).

## Multi-image Archives

If an archive contains only one image:

- you do not need to specify `-t` or `-i`

If an archive contains multiple images:

- a selection is required; `udf` is not interactive — without `-t` or `-i` it exits with an error whose message lists the available options
- pick a value from the error message and re-run with `-t` or `-i`

## Generated Files

For each extracted image, `udf` writes:

- merged `rootfs`
- `config.yaml`

`config.yaml` is generated from the original `config.json`, with strings formatted for readability where possible.

## Error Handling

One error taxonomy drives all three interfaces:

| Kind | CLI exit | HTTP status | MCP code |
|---|---|---|---|
| invalid input | 2 | 400 | -32602 |
| not found | 1 | 404 | -32001 |
| conflict | 1 | 409 | -32009 |
| unauthorized / forbidden | 1 | 401 / 403 | -32010 / -32011 |
| unavailable | 1 | 503 | -32603 |
| internal | 1 | 500 | -32603 |

Batch `extract` keeps going after one archive fails: failures land in the row's `error` column, and the command only reports an error itself when nothing succeeded. Low-level system errors are preserved as-is for diagnostics.

## Technical Notes

- Layer application follows `manifest.json`
- Whiteout files are handled during extraction
- Inner layer streams are auto-detected: gzip, bzip2 and uncompressed tar are supported
- Directory metadata is restored after extraction to avoid intermediate permission issues
- Memory usage is kept low by streaming layer extraction instead of unpacking all layers to disk first

## Known Limitations

- zstd-compressed inner layers are not supported and fail with an explicit error
- A disk image must contain a supported filesystem (ext4/xfs/btrfs/NTFS/squashfs/ISO9660/UDF/exFAT/EROFS/FAT) (whole disk, partitions, or LVM2 logical volumes); other filesystems (e.g. JFS/ReiserFS) are reported by `info` but not extracted
- Disk extraction recreates regular files, directories and symlinks; file ownership is not preserved (files are written as the current user)
- Directory input only scans the top level and is not recursive
- Multi-image archives require an explicit `-t`/`-i` selection; `udf` never prompts interactively
- One input expression per command; use a directory or a glob for batches
- The interface language follows `LANG`/`LC_ALL`, override with `--xyz.lang en|zh-CN`; the Go library errors still carry stable i18n keys

## Use as a Library

Everything is importable as a Go library:

```go
package main

import (
	"fmt"
	"log"

	"github.com/ejfkdev/udf/image"
)

func main() {
	meta, err := image.ScanImageMetadata("./app.tar", image.Selection{RepoTag: "demo/app:latest"})
	if err != nil {
		log.Fatal(err)
	}

	// Print /etc the way `ls -al` would, without extracting anything.
	tree, err := image.BuildFileSystem("./app.tar", meta)
	if err != nil {
		log.Fatal(err)
	}
	listing, err := image.FormatListing(tree, "/etc")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(listing)

	// The same view as structured data.
	entries, err := image.ListEntries(tree, "/etc")
	if err != nil {
		log.Fatal(err)
	}
	_ = entries

	// Extract a single file out of the image.
	if _, err := image.ExtractPath("./app.tar", meta, "/etc/passwd", "./passwd", 1<<20); err != nil {
		log.Fatal(err)
	}
}
```

Public packages:

- `github.com/ejfkdev/udf/image` — high level: metadata scanning, merged file system view, listing, selective and full extraction
- `github.com/ejfkdev/udf/fsview` — whiteout-aware in-memory merged filesystem tree
- `github.com/ejfkdev/udf/layer` — single layer application and compressed stream detection
- `github.com/ejfkdev/udf/fsutil` — escape-safe path resolution and file writing helpers
- `github.com/ejfkdev/udf/types` — shared data structures
- `github.com/ejfkdev/udf/i18n` — bilingual message bundles

Selection and not-found errors implement `*i18n.LocalizedError` with a stable
`.Key` you can match on; `Error()` renders readable English by default.

## Current Scope

`udf` is a general "extract the contents of an encapsulation format" tool: it
detects the input by its content (magic bytes), not its extension, and opens it
through one of three pipelines — an archive, a virtual disk container, or a raw
filesystem image.

Supported:

- archive extraction (tar, tar.gz/tgz, tar.xz, tar.bz2, tar.zst, tar.lz4, zip, 7z, rar, cpio, cpio.gz/xz/zst, asar, rpm, deb/ipk, cab, nar, xar/pkg, pyinstaller, nuitka, .NET single-file, zip SFX, OCI layout, oci-archive/flatpak, `docker save`), batch via directory or glob
- disk / VM images: qcow2, QCOW v1, VMDK, VHD/VHDX, VDI, QED, Parallels, WIM/ESD/SWM, FFU, VMA, SIF, OVA/OVF, raw/`.img`/`.ami`
- filesystems (whole disk, partitions, or LVM2 logical volumes): ext2/3/4, xfs, btrfs, NTFS, squashfs, ISO9660, UDF, exFAT, EROFS (uncompressed), FAT12/16/32
- listing without extracting (`ls`), selective extraction (`cp`), metadata (`info`), single-file read to stdout (`cat`), hex header dump (`xxd`), full extraction (`extract`)
- one binary, three interfaces: CLI, HTTP (REST + OpenAPI), MCP tools
- config YAML export
- importable as a Go library

Not intended as:

- a Docker replacement
- a container runtime
- an OCI registry client

## License

This project is licensed under the MIT License. See [LICENSE](./LICENSE).