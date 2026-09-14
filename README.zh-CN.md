# udf

[![license](https://img.shields.io/github/license/ejfkdev/udf)](./LICENSE)
[![release](https://github.com/ejfkdev/udf/actions/workflows/release.yml/badge.svg)](https://github.com/ejfkdev/udf/actions/workflows/release.yml)

`udf` 是一个 Go 编写的命令行工具，用于从归档、虚拟磁盘镜像与文件系统镜像中提取内容，并列出（或合并为单一 `rootfs`）镜像文件系统。

一个二进制提供三种接口：每个命令只定义一次（基于 [xyz-go](https://github.com/ejfkdev/xyz-go)），自动同时具备 **CLI 子命令**、**HTTP REST 路由**（自带 OpenAPI 文档）和 **MCP 工具**三种形态。所有输入都是**运行 udf 的机器上的本地文件路径**——不涉及文件上传，操作始终发生在程序所在的机器上。

English version: [README.md](./README.md)

## 目录

- [功能特性](#功能特性)
- [三种接口](#三种接口)
- [安装](#安装)
- [CLI 用法](#cli-用法)
- [输入方式](#输入方式)
- [子命令](#子命令)
- [输出规则](#输出规则)
- [参数说明](#参数说明)
- [多镜像归档说明](#多镜像归档说明)
- [生成文件](#生成文件)
- [错误处理](#错误处理)
- [技术说明](#技术说明)
- [已知限制](#已知限制)
- [作为库使用](#作为库使用)
- [当前范围](#当前范围)
- [License](#license)

## 功能特性

三种接口，一次定义：

- CLI 子命令、HTTP REST 服务（含 `/openapi.json`）与 MCP 工具服务器，由同一组命令定义自动生成
- 本地路径语义：`archive` / `dest` 是运行 udf 的主机上的路径，不做文件上传
- 网络模式内置 Bearer 鉴权、TLS 与 CORS

归档处理：

- 将镜像归档解包为合并后的 `rootfs`
- 外层归档格式：`.tar`、`.tar.gz`、`.tgz`、`.tar.xz`、`.tar.bz2`、`.tar.zst`、`.tar.lz4`、`.zip`、`.7z`、`.rar`、`.cpio`（以及 `.cpio.gz`/`.cpio.xz`/`.cpio.zst`，如 initramfs）、`.asar`（Electron）、`.rpm`、`.deb`/`.ipk`、`.cab`（含 `.msi` 内嵌 cab）、`.nar`（Nix）、`.xar`/`.pkg`（macOS 安装器）（以及 `.ppkg` Windows 预配包，OPC/ZIP）
- 可执行文件封装格式：PyInstaller onefile（CArchive + PYZ，重建 `.pyc`）、Nuitka onefile（尾部附加与内嵌载荷）、.NET single-file 应用（bundle v1/v2/v6，含 deflate）、ZIP 自解压 exe；APK 内的二进制 Android XML（AXML）——`AndroidManifest.xml`、布局文件——自动解码为可读文本 XML
- 虚拟磁盘镜像：qcow2、QCOW v1、VMDK、VHD/VHDX、VDI、QED、Parallels、WIM、ESD、SWM、FFU、raw/`.img`/`.ami`、OVA、OVF、VMA、SIF、AppImage（ext4/xfs/btrfs/NTFS/squashfs/ISO9660/UDF/exFAT/EROFS/FAT，含 LVM2 逻辑卷）
- 支持常见镜像归档结构：平铺结构 `manifest.json + config.json + layers/...`、经典 `docker save` 结构 `<layer-id>/layer.tar`、OCI 镜像布局目录，以及单文件 OCI 镜像归档（`.oci.tar`，含 flatpak bundle）
- 输入可以是单个归档、通配符模式或目录（只扫描一层）

合并正确性：

- 按 `manifest.json` 中的层顺序正确合并
- 正确处理 whiteout 文件和 opaque 目录
- 支持软链接和硬链接
- 当目标文件系统不支持硬链接时自动降级为文件复制
- 解包完成后统一恢复目录元数据

查看与选择：

- 不解压即可列出镜像目录内容（`udf ls`，CLI 表格与结构化 JSON 同源）
- 不解包整个镜像即可单独提取某个文件或目录（`udf cp`）
- 查看镜像元数据：标签、层、工作目录、入口命令（`udf info`）
- 将单个文件的原生字节输出到 stdout，可直接接管道（`udf cat`）
- 以十六进制预览文件头几个字节、判断文件类型（`udf xxd`）

其他：

- 将原始 `config.json` 导出为可读性更高的 `config.yaml`
- 一套错误分类同时映射 CLI 退出码、HTTP 状态码与 MCP 错误码
- 可作为 Go 库引用

### 容器与磁盘镜像

除容器分层归档外，`udf` 也可以打开虚拟磁盘镜像，并通过同一组命令解出其中的文件系统。`info`、`ls`、`cp`、`extract` 都会自动识别磁盘输入：

```text
./udf info ./disk.qcow2                          # 列出磁盘、卷与文件系统
./udf ls ./disk.qcow2                            # 列出卷（分区 + 逻辑卷）
./udf ls ./disk.qcow2 /vg1/root                  # 列出某卷的根目录
./udf ls ./disk.qcow2 /vg1/root/etc              # 列出某卷内的 /etc
./udf cp ./disk.qcow2 /vg1/root/etc/hostname ./hostname
./udf extract ./disk.qcow2                       # 解出所有文件系统卷
./udf ls ./appliance.ova                         # 列出 OVA 内的 vmdk 磁盘
./udf ls ./appliance.ova /disk1.vmdk             # 再列出该磁盘的卷
```

支持的磁盘容器格式：

- **qcow2**（v2/v3），只读流式
- **VMDK** sparse 盘——monolithicSparse（flat 或 deflate）与 streamOptimized 均支持
- **OVA** 归档（包含 OVF 描述 + 一个或多个 `.vmdk` 磁盘的 tar）
- **OVF** 独立描述符（`.ovf` 引用同目录 `.vmdk` 磁盘）
- **VHD/VHDX** 虚拟磁盘（fixed、dynamic、differencing）
- **QED** QEMU Enhanced Disk 镜像（二级页表；小端序）
- **QCOW v1** 传统 QEMU 磁盘镜像（二级页表；压缩簇）
- **WIM** Windows 镜像文件（取第一个 image；XPRESS 与 LZX 压缩）
- **ESD** Windows「电子软件下载」文件（WIM 的 LZMS 压缩形态）
- **SWM** 分卷 WIM 镜像（传第一个分卷，自动读取同目录 `*.swm2/3/…` 兄弟分卷）
- **FFU** Full Flash Update 镜像（扫描跳过安全头定位内嵌磁盘——启发式，未对真实 Microsoft FFU 验证）
- **VMA** Proxmox vzdump 归档（一个或多个 raw 磁盘）
- **SIF** Singularity 容器镜像（解出其系统分区）
- **VDI** VirtualBox 磁盘镜像
- **Parallels** 磁盘镜像（`.parallels`/`.hds`）
- **raw** 磁盘与文件系统镜像（`.img`/`.raw`/`.dd`/`.ext4`/`.xfs` 等）——文件本身即磁盘

虚拟磁盘按「路径树」寻址：`/` 列出磁盘（多磁盘 OVA）或卷，`/<卷>` 进入某个卷，之后就是卷内路径。分区命名为 `p1`…`pN`，LVM 逻辑卷命名为 `vg/lv`。

- 解析 MBR/GPT 分区表，并内置解析 LVM2 物理卷：每个逻辑卷都作为可选卷暴露，与普通分区并列
- 可解出的文件系统为 **ext4**、**xfs**、**btrfs**、**NTFS**、**squashfs**、**ISO9660**、**UDF**、**exFAT**、**EROFS** 与 **FAT**（fat12/16/32）
- 磁盘含多个文件系统时，`ls`/`cp` 需要带卷前缀（或从 `/` 逐层下钻）；只有一个文件系统卷时隐式使用该卷，所以 `./udf ls img.qcow2 /etc` 对简单镜像依然好用
- `extract` 解出所有文件系统卷；卷多时每个卷落到以卷名命名的子目录
- 普通文件、目录、软链接会原样重建；硬链接变为独立副本，设备节点、FIFO、套接字会被跳过

## 三种接口

```bash
# CLI
./udf ls ./image.tar /etc

# HTTP —— 所有注册命令在同一个端口上提供路由
./udf serve --addr 127.0.0.1:8080
curl -s 'http://127.0.0.1:8080/ls?archive=/data/image.tar&path=/etc'
curl -s -X POST 'http://127.0.0.1:8080/cp' -H 'Content-Type: application/json' \
  -d '{"archive":"/data/image.tar","source":"/etc/passwd","dest":"/tmp/passwd"}'
curl -s http://127.0.0.1:8080/openapi.json

# MCP —— 命令即工具（stdio / SSE / streamable HTTP）
./udf mcp stdio
./udf mcp http --addr 127.0.0.1:9000 --bearer s3cret
```

路由一览：`GET /info?archive=…`、`GET /ls?archive=…&path=…`、`POST /cp`、`GET /cat?archive=…&source=…`、`GET /xxd?archive=…&source=…`、`POST /extract`，另有 `/healthz` 与 `/openapi.json`。

在 MCP 客户端中，将 udf 注册为 stdio 服务：

```json
{"command": "udf", "args": ["mcp", "stdio"]}
```

> **安全提示：**所有路径参数都指向运行 udf 的机器。把 `serve` 或 `mcp http/sse` 暴露到回环地址之外，意味着调用方可以读写服务端的本地文件。请用 `--bearer`、TLS（`--tls-cert`/`--tls-key`）和 CORS 白名单保护这些模式——详见 [xyz-go](https://github.com/ejfkdev/xyz-go) 的内置配置。

## 安装

### 使用 Homebrew 安装（macOS）

```bash
brew install ejfkdev/tap/udf
```

formula 由 [`homebrew-udf`](https://github.com/ejfkdev/homebrew-udf) tap 提供，打包本仓库 Release 工作流发布的二进制产物。

### 使用 `go install`

```bash
go install github.com/ejfkdev/udf@latest
```

### 从源码编译

```bash
git clone https://github.com/ejfkdev/udf.git
cd udf
go build -o udf .
```

## CLI 用法

```bash
./udf <命令> [参数]
```

示例：

```bash
./udf info ./image.tar
./udf ls ./image.tar /etc
./udf cp ./image.tar /etc/passwd ./passwd
./udf extract ./image.tar
./udf extract -o ./output -t repo/app:latest './repo/*.tar'
./udf serve --addr 127.0.0.1:8080
./udf mcp stdio
```

`extract` 是默认命令，老用法依然有效：

```bash
./udf ./image.tar                    # 等同于: ./udf extract ./image.tar
./udf './repo/*.tar' -o ./output -t repo/app:latest
```

使用省略写法的 flag 要放在路径之后；需要 flag 前置时请显式写子命令（`./udf extract -o ./out ./image.tar`）。

内建便利项：每个命令可用 `-h` 查看帮助、`-v` 查看版本、`--json` 输出机器可读结果，以及 `completion bash|zsh|fish` 补全脚本。

## 输入方式

每个命令接收**一个输入表达式**作为归档：

- 单个归档文件（`.tar`/`.tar.gz`/`.tgz`/`.zip`/`.7z`/`.rar`/`.cpio`/`.asar`/`.rpm`/PyInstaller/Nuitka/.NET single-file 可执行文件）或磁盘镜像（`.qcow2`/`.vmdk`/`.vhd`/`.vhdx`/`.vdi`/`.img`/`.raw`/`.dd`/`.ova`/`.vma`）——`info`、`ls`、`cp` 要求此形式
- 通配符模式或目录（只扫描一层，不递归）——`extract` 额外支持，并展开为批量处理

示例：

```bash
./udf extract ./images
./udf extract './images/*.tar'
./udf ls ./image.tar /etc
```

## 子命令

### `info` — 查看镜像元数据

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

### `ls` — 不解压列出目录内容

`udf ls` 在内存中构建合并后的文件系统视图，以 `ls -al` 信息呈现，不向磁盘写入任何文件。CLI 输出对齐表格，HTTP/MCP 返回同源的结构化 JSON。

```bash
./udf ls ./image.tar          # 镜像根目录
./udf ls ./image.tar /etc     # 某个目录
```

```text
name    type  mode        size  mod_time                    target
------  ----  ----------  ----  --------------------------  ------
group   file  -rw-r--r--  10    2026-08-22T00:04:25+08:00
passwd  file  -rw-r--r--  30    2026-08-22T00:04:25+08:00
```

说明：

- 层的合并与完整解包完全一致，包含 whiteout 和 opaque 目录语义
- 列包含类型（`dir|file|symlink|hardlink`）、权限、大小、修改时间与软链接目标
- 路径前导 `/` 可省略；`/` 或 `.` 表示镜像根目录
- 传入文件路径时只列出该条目本身

### `cp` — 单独提取某个文件或目录

```bash
./udf cp ./image.tar /etc/passwd ./passwd
./udf cp ./image.tar /etc/nginx ./nginx
./udf cp ./image.tar / ./rootfs
```

目标路径语义与 `cp` 一致：

- 源是目录且目标已存在目录时，落到 `<目标>/<目录名>`
- 源是目录且目标不存在时，直接以目标路径复制
- 源是文件或软链接时，写入 `<目标>`；若目标已存在目录，则落到 `<目标>/<文件名>`
- 提取 `/`（镜像根）时，内容直接放入目标路径
- 被 whiteout 删除的条目会被跳过；软链接原样重建；源在选择范围内的硬链接会保留为硬链接，否则复制内容

### `cat` — 将单个文件输出到 stdout

与系统 `cat` 一致：`udf cat` 把单个文件的原始字节以二进制形式流式写到 stdout（末尾不补换行），可直接接管道：

```bash
./udf cat ./image.tar /etc/passwd | grep root
./udf cat ./disk.qcow2 /p1/etc/hostname
```

镜像内的软链接与硬链接会跟随到其目标文件。适合预览小型文本文件；大文件或二进制文件建议改用 `xxd` 预览前部有限字节，而不是整个打印到终端。

### `xxd` — 以十六进制预览文件头

与 `xxd -l N` / `hexdump -C` 一致：`udf xxd` 把文件头几个字节以「十六进制 + ASCII」形式打印，便于按文件头判断类型：

```bash
./udf xxd ./image.tar /etc/passwd                        # 默认前 256 字节
./udf xxd ./image.tar /etc/passwd -n 64 -s 4096          # 跳过 4096 字节后取 64 字节
```

`-n/--bytes` 指定字节数（默认 256），`-s/--offset` 从文件起始跳过若干字节再开始。

### `extract` — 解包合并后的 rootfs

```bash
./udf extract ./image.tar
./udf ./image.tar                     # extract 是默认命令，可省略
./udf extract -o ./output -f -t repo/app:latest './repo/*.tar'
```

`extract` 把输入表达式展开为一个或多个归档并按序处理，每个归档对应一行结果（CLI 为表格，HTTP/MCP 为 JSON 数组）；单个归档失败只记录在对应行的 `error` 列，不会中断其余归档：

```text
archive      output_dir                     layers  error
-----------  -----------------------------  ------  -----
./image.tar  /tmp/out/image                 2
```

## 输出规则

`extract` 不指定 `-o/--output` 时输出到各输入归档所在目录，指定时输出到给定父目录下：

- 单镜像归档：
  - `{file_name}/`
- 多镜像归档：
  - `{file_name}/{repo_tag}/`
  - 如果没有 tag，则回退为 `{file_name}/index-{n}/`

示例：

```text
输入:  /data/demo/tempest.tar
输出: /data/demo/tempest
```

```text
输入:  /data/demo/bundle.tar
标签:  repo/app:1.0, repo/app:latest
输出: /data/demo/bundle/repo_app_1.0
      /data/demo/bundle/repo_app_latest
```

## 参数说明

命令级参数：

- `-t, --repo-tag` — 按 `manifest.json` 中的 `RepoTags` 选择镜像（`info`、`ls`、`cp`、`extract` 均可用）
- `-i, --image-index` — 按 `manifest.json` 数组中的索引选择镜像（同上）
- `-o, --output` — 输出父目录（`extract`）
- `-f, --force` — 强制写入已存在的非空目标目录（`extract`）
- `-b, --buffer-size` — 文件复制缓冲区大小，单位字节（`cp`、`extract`）
- `-n, --bytes` — 要转储的字节数（`xxd`，默认 256）
- `-s, --offset` — 从文件起始跳过多少字节再开始（`xxd`）

内建参数来自 xyz-go：`-h/--help`、`-v/--version`、`--json`、`--xyz.lang en|zh-CN`（界面语言，默认跟随 `LANG`/`LC_ALL` 自动检测）与 `completion bash|zsh|fish`。`serve` 与 `mcp` 模式额外支持 `--addr`、`--bearer`、`--cors`、`--tls-cert`/`--tls-key`、`--timeout`、`--log-level`（`mcp` 另有 `--versions` 与 `--session-timeout`）——详见 [xyz-go README](https://github.com/ejfkdev/xyz-go)。

## 多镜像归档说明

如果一个归档里只有一个镜像：

- 不需要指定 `-t` 或 `-i`

如果一个归档里有多个镜像：

- 必须指定提取哪一个；程序不做交互——未指定时会直接报错退出，错误信息中会列出可选值
- 从错误信息中选定一个值后，带上 `-t` 或 `-i` 重新运行

## 生成文件

每个解包结果目录中，`udf` 会写出：

- 合并后的 `rootfs`
- `config.yaml`

其中 `config.yaml` 来自原始 `config.json`，并尽量按更易读的方式格式化字符串内容。

## 错误处理

同一套错误分类贯通三种接口：

| 分类 | CLI 退出码 | HTTP 状态码 | MCP 错误码 |
|---|---|---|---|
| 参数无效 | 2 | 400 | -32602 |
| 不存在 | 1 | 404 | -32001 |
| 冲突 | 1 | 409 | -32009 |
| 未授权 / 禁止 | 1 | 401 / 403 | -32010 / -32011 |
| 依赖不可用 | 1 | 503 | -32603 |
| 内部错误 | 1 | 500 | -32603 |

批量 `extract` 中单个归档失败不会中断：失败记录在对应行的 `error` 列，只有全部失败时命令本身才报错。系统底层错误会原样保留，便于排查问题。

## 技术说明

- 层合并顺序以 `manifest.json` 为准
- 解包过程中实时处理 whiteout
- 层内压缩流自动识别：支持 gzip、bzip2 和未压缩 tar
- 为避免中途目录权限导致写入失败，目录权限和时间戳会在解包完成后统一恢复
- 使用流式方式处理 layer，避免把所有层先完整落盘，尽量降低内存占用

## 已知限制

- 不支持 zstd 压缩的层（会明确报错）
- 磁盘镜像必须包含受支持的文件系统（ext4/xfs/btrfs/NTFS/squashfs/ISO9660/UDF/exFAT/FAT）（整块磁盘、分区或 LVM2 逻辑卷）；JFS/ReiserFS 等其余文件系统 `info` 会列出但不会解出
- 磁盘解包会重建普通文件、目录与软链接；不保留文件属主（文件以当前用户写入）
- 目录输入只扫描当前一层，不递归子目录
- 多镜像归档必须显式指定 `-t` / `-i`，程序不会交互式询问
- 每个命令接收一个输入表达式；批量请使用目录或通配符
- 界面语言跟随 `LANG`/`LC_ALL` 自动检测，可用 `--xyz.lang en|zh-CN` 覆盖；Go 库的错误仍携带稳定的 i18n key

## 作为库使用

所有代码都可以作为 Go 库引用：

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

	// 以 ls -al 的方式打印 /etc，不向磁盘写入任何文件。
	tree, err := image.BuildFileSystem("./app.tar", meta)
	if err != nil {
		log.Fatal(err)
	}
	listing, err := image.FormatListing(tree, "/etc")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(listing)

	// 同一视图的结构化数据。
	entries, err := image.ListEntries(tree, "/etc")
	if err != nil {
		log.Fatal(err)
	}
	_ = entries

	// 从镜像中单独提取一个文件。
	if _, err := image.ExtractPath("./app.tar", meta, "/etc/passwd", "./passwd", 1<<20); err != nil {
		log.Fatal(err)
	}
}
```

公开的包：

- `github.com/ejfkdev/udf/image` — 高层能力：元数据扫描、合并视图、目录列表、选择性提取与完整解包
- `github.com/ejfkdev/udf/fsview` — 感知 whiteout 的内存合并文件系统树
- `github.com/ejfkdev/udf/layer` — 单层应用与压缩流识别
- `github.com/ejfkdev/udf/fsutil` — 防路径逃逸的路径解析与文件写入辅助
- `github.com/ejfkdev/udf/types` — 共享数据结构
- `github.com/ejfkdev/udf/i18n` — 中英文双语消息包

镜像选择、路径不存在等错误实现为 `*i18n.LocalizedError`，带有可匹配的稳定
`.Key`；`Error()` 默认输出可读的英文消息。

## 当前范围

`udf` 定位为通用的「从各种封装格式提取内容」工具：按文件内容（magic 字节）而非扩展名识别输入，并经由归档、虚拟磁盘容器或裸文件系统镜像三条管线之一打开。

当前支持：

- 归档解包（tar、tar.gz/tgz、tar.xz、tar.bz2、tar.zst、tar.lz4、zip、7z、rar、cpio、cpio.gz/xz/zst、asar、rpm、deb/ipk、cab、nar、xar/pkg、pyinstaller、nuitka、.NET single-file、zip 自解压、OCI 布局、OCI 归档/flatpak、`docker save`），目录 / 通配符批量
- 磁盘 / 虚拟机镜像：qcow2、QCOW v1、VMDK、VHD/VHDX、VDI、QED、Parallels、WIM/ESD/SWM、FFU、VMA、SIF、OVA/OVF、raw/`.img`/`.ami`
- 文件系统（整块磁盘、分区或 LVM2 逻辑卷）：ext2/3/4、xfs、btrfs、NTFS、squashfs、ISO9660、UDF、exFAT、EROFS（未压缩）、FAT12/16/32
- 不解压列出内容（`ls`）、单独提取（`cp`）、元数据（`info`）、单文件读 stdout（`cat`）、十六进制文件头（`xxd`）、整包解压（`extract`）
- 一个二进制、三种接口：CLI、HTTP（REST + OpenAPI）、MCP 工具
- `config.yaml` 导出
- 可作为 Go 库引用

当前不定位为：

- Docker 替代品
- 容器运行时
- OCI Registry 客户端

## License

本项目采用 MIT License 开源，详见 [LICENSE](./LICENSE)。