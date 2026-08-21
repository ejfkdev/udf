# udf

[![license](https://img.shields.io/github/license/ejfkdev/udf)](./LICENSE)
[![release](https://github.com/ejfkdev/udf/actions/workflows/release.yml/badge.svg)](https://github.com/ejfkdev/udf/actions/workflows/release.yml)

`udf` is a Go CLI tool that extracts Harbor / Docker image archives into a merged root filesystem (`rootfs`).

It is designed for offline image analysis and large archive handling: multiple archive formats, layered filesystem merging, whiteout processing, listing without extraction, selective extraction, and a bilingual CLI.

中文说明见 [README.zh-CN.md](./README.zh-CN.md).

## Table of Contents

- [Features](#features)
- [Why](#why)
- [Installation](#installation)
- [Usage](#usage)
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

Input and formats:

- Extract image archives into a merged `rootfs`
- Input as a single file, a directory, or a glob pattern
- Outer archive formats: `.tar`, `.tar.gz`, `.tgz`, `.zip`
- Common image layouts: flat `manifest.json + config.json + layers/...` and classic `docker save` (`<layer-id>/layer.tar`)

Extraction correctness:

- Layers merged in `manifest.json` order
- Whiteout files and opaque directories handled
- Symlinks and hardlinks supported
- Fallback to file copy when the target filesystem does not support hardlinks
- Directory metadata restored after extraction

Inspection:

- List merged contents without extracting (`udf ls`, output like `ls -al`)
- Extract a single file or directory only (`udf cp`)

Convenience:

- Export `config.json` as readable `config.yaml`
- Bilingual (Chinese / English) CLI and help output
- Importable as a Go library

## Why

This tool is useful when you need to:

- inspect container files offline without running Docker
- extract image contents from Harbor-exported archives
- analyze application files and runtime layout
- process large image archives with low memory usage
- batch-extract multiple image archives from a directory

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

## Usage

```bash
./udf [options] <archive|directory|glob>...
```

Examples:

```bash
./udf ./image.tar
./udf ./image.tar.gz
./udf ./image.zip
./udf ./repo
./udf "./repo/*.tar"
./udf -o ./output ./image.tar
./udf -t repo/app:latest ./image.tar
./udf -i 1 ./image.tar
./udf -f ./image.tar
./udf --lang en ./image.tar
```

## Input Modes

You can pass:

- a single archive file
- a directory
- a glob pattern
- multiple inputs in one command

Examples:

```bash
./udf ./image.tar
./udf ./images
./udf "./images/*.tar"
./udf ./a.tar ./b.tar.gz "./repo/*.zip"
```

Directory input only scans the top level and is not recursive.

## Subcommands

### `ls` — list image contents without extracting

`udf ls` builds the merged filesystem view in memory and prints it the way
`ls -al` would, without writing a single file to disk.

```bash
./udf ls ./image.tar                # list the image root
./udf ls ./image.tar /etc           # list a directory inside the image
./udf ls ./image.tar /etc/passwd    # show one file
./udf ls -t repo/app:latest ./image.tar /usr/local/bin
```

Example output:

```text
$ ./udf ls ./image.tar /etc
Image contents: ./image.tar
total 2
-rw-r--r--   1 root      root           30 Sep 13  2020 passwd
lrwxrwxrwx   1 root      root           19 Sep 13  2020 resolv.conf -> /run/systemd/resolve
```

Details:

- Layers are merged with the same whiteout and opaque-directory semantics as the full extract
- Long format shows permissions, link count, owner, group, size, mtime and name; symlinks display their target
- Leading `/` in the path is optional; `/` or `.` lists the image root
- Use `-t` / `-i` to select an image in a multi-image archive

### `cp` — extract a single file or directory

`udf cp` streams only the entries that belong to the selection, still
respecting the merged view, so you never have to unpack the whole image.

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

## Output Rules

If `-o/--output` is not specified:

- output goes beside the input archive

If `-o/--output` is specified:

- output goes under the given parent directory

Output layout:

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

- `-o, --output`
  - Output parent directory
  - A same-named subdirectory will be created
  - Defaults to the input file's directory
- `-f, --force`
  - Force writing into an existing non-empty target directory
  - Does not clear the directory first
- `-t, --repo-tag`
  - Select the image by `RepoTags` from `manifest.json`
  - Recommended when one archive contains multiple images
- `-i, --image-index`
  - Select the image by index in the `manifest.json` array
- `-b, --buffer-size`
  - File copy buffer size in bytes
- `-l, --lang`
  - CLI language: `zh` or `en`
- `--no-progress`
  - Disable the dynamic progress bar

The `ls` and `cp` subcommands share `-t` and `-i` (plus `-b` for `cp`); their `--lang` flag has no `-l` shorthand.

## Multi-image Archives

If an archive contains only one image:

- you do not need to specify `-t` or `-i`

If an archive contains multiple images:

- a selection is required; `udf` is not interactive — without `-t` or `-i` it exits with an error whose message lists the available options
- you should usually use `-t`
- pick a value from the error message and re-run with `-t` or `-i`

`-t` and `-i` do not mean the same thing:

- `-t` selects by tag
- `-i` selects by position in `manifest.json`

## Generated Files

For each extracted image, `udf` writes:

- merged `rootfs`
- `config.yaml`

`config.yaml` is generated from the original `config.json`, with strings formatted for readability where possible.

## Error Handling

- In batch mode, one failed archive does not stop the others
- Non-image archives are skipped in batch mode
- Exit codes: `0` when at least one image was processed successfully (even if others failed in batch mode), `1` when nothing could be processed
- Project-generated user-facing messages support both Chinese and English
- Low-level system errors are preserved as-is for diagnostics

## Technical Notes

- Layer application follows `manifest.json`
- Whiteout files are handled during extraction
- Inner layer streams are auto-detected: gzip, bzip2 and uncompressed tar are supported
- Directory metadata is restored after extraction to avoid intermediate permission issues
- Memory usage is kept low by streaming layer extraction instead of unpacking all layers to disk first

## Known Limitations

- zstd-compressed inner layers are not supported and fail with an explicit error
- Directory input only scans the top level and is not recursive
- Multi-image archives require an explicit `-t`/`-i` selection; `udf` never prompts interactively

## Use as a Library

Everything is importable as a Go library — no code lives under `internal/`:

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

Supported:

- offline image archive extraction
- batch processing
- listing merged image contents without extracting (`ls`)
- extracting a single file or directory (`cp`)
- bilingual CLI
- config YAML export

Not intended as:

- a Docker replacement
- a container runtime
- an OCI registry client

## License

This project is licensed under the MIT License. See [LICENSE](./LICENSE).