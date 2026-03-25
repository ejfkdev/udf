# udf

`udf` is a Go CLI tool that extracts Harbor / Docker image archives into a merged root filesystem (`rootfs`).

It is designed for offline image analysis and large archive handling, with support for multiple archive formats, layered filesystem merging, whiteout processing, and bilingual CLI output.

中文说明见 [README.zh-CN.md](./README.zh-CN.md).

## Features

- Extract image archives into a merged `rootfs`
- Support input as a single file, directory, or glob pattern
- Support outer archive formats:
  - `.tar`
  - `.tar.gz`
  - `.tgz`
  - `.zip`
- Support common image archive layouts:
  - flat `manifest.json + config.json + layers/...`
  - classic `docker save` layout with `<layer-id>/layer.tar`
- Correctly apply image layers in `manifest.json` order
- Handle whiteout files and opaque directories
- Support symlinks and hardlinks
- Fallback to file copy when the target filesystem does not support hardlinks
- Export `config.json` as readable `config.yaml`
- Bilingual CLI and help output:
  - Chinese
  - English

## Why

This tool is useful when you need to:

- inspect container files offline without running Docker
- extract image contents from Harbor-exported archives
- analyze application files and runtime layout
- process large image archives with low memory usage
- batch-extract multiple image archives from a directory

## Installation

### Install with `go install`

```bash
go install github.com/ejfdkev/udf@latest
```

### Build from source

```bash
git clone https://github.com/ejfdkev/udf.git
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

## Multi-image Archives

If an archive contains only one image:

- you do not need to specify `-t` or `-i`

If an archive contains multiple images:

- `udf` requires a selection
- you should usually use `-t`
- if you do not specify one, the tool will show the available values and ask you to select one

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
- Project-generated user-facing messages support both Chinese and English
- Low-level system errors are preserved as-is for diagnostics

## Technical Notes

- Layer application follows `manifest.json`
- Whiteout files are handled during extraction
- Some Harbor exports store inner layers as compressed streams; `udf` detects and handles common cases
- Directory metadata is restored after extraction to avoid intermediate permission issues
- Memory usage is kept low by streaming layer extraction instead of unpacking all layers to disk first

## Current Scope

Supported:

- offline image archive extraction
- batch processing
- bilingual CLI
- config YAML export

Not intended as:

- a Docker replacement
- a container runtime
- an OCI registry client

## License

This project is licensed under the MIT License. See [LICENSE](./LICENSE).
