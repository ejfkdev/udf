# udf

[![license](https://img.shields.io/github/license/ejfkdev/udf)](./LICENSE)
[![release](https://github.com/ejfkdev/udf/actions/workflows/release.yml/badge.svg)](https://github.com/ejfkdev/udf/actions/workflows/release.yml)

`udf` is a Go CLI tool that extracts Harbor / Docker image archives into a merged root filesystem (`rootfs`).

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
- Outer archive formats: `.tar`, `.tar.gz`, `.tgz`, `.zip`
- Common image layouts: flat `manifest.json + config.json + layers/...` and classic `docker save` (`<layer-id>/layer.tar`)
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

Convenience:

- Export `config.json` as readable `config.yaml`
- One error taxonomy: CLI exit code, HTTP status and MCP error code stay aligned
- Importable as a Go library

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

Route overview: `GET /info?archive=…`, `GET /ls?archive=…&path=…`, `POST /cp`, `POST /extract`, plus `/healthz` and `/openapi.json`.

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

- a single archive file — required by `info`, `ls` and `cp`
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

Supported:

- offline image archive extraction, batch via directory or glob
- listing merged image contents without extracting (`ls`), selective extraction (`cp`), metadata (`info`)
- one binary, three interfaces: CLI, HTTP (REST + OpenAPI), MCP tools
- config YAML export
- importable as a Go library

Not intended as:

- a Docker replacement
- a container runtime
- an OCI registry client

## License

This project is licensed under the MIT License. See [LICENSE](./LICENSE).