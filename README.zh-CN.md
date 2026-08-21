# udf

[![license](https://img.shields.io/github/license/ejfkdev/udf)](./LICENSE)
[![release](https://github.com/ejfkdev/udf/actions/workflows/release.yml/badge.svg)](https://github.com/ejfkdev/udf/actions/workflows/release.yml)

`udf` 是一个 Go 编写的命令行工具，用于将 Harbor / Docker 导出的镜像归档解包为合并后的根文件系统（`rootfs`）。

它适合离线镜像分析、大体积镜像包处理以及批量解包场景，支持多种归档格式、分层文件系统合并、whiteout 处理、不解压目录列表、选择性提取和中英双语命令行输出，并可作为 Go 库引用。

English version: [README.md](./README.md)

## 目录

- [功能特性](#功能特性)
- [适用场景](#适用场景)
- [安装](#安装)
- [基本用法](#基本用法)
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

输入与格式：

- 将镜像归档解包为合并后的 `rootfs`
- 支持单文件、目录、通配符三种输入方式
- 外层归档格式：`.tar`、`.tar.gz`、`.tgz`、`.zip`
- 支持常见镜像归档结构：平铺结构 `manifest.json + config.json + layers/...` 与经典 `docker save` 结构 `<layer-id>/layer.tar`

合并正确性：

- 按 `manifest.json` 中的层顺序正确合并
- 正确处理 whiteout 文件和 opaque 目录
- 支持软链接和硬链接
- 当目标文件系统不支持硬链接时自动降级为文件复制
- 解包完成后统一恢复目录元数据

查看与选择：

- 不解压即可列出镜像合并后的目录内容（`udf ls`，输出类似 `ls -al`）
- 不解包整个镜像即可单独提取某个文件或目录（`udf cp`）

其他：

- 将原始 `config.json` 导出为可读性更高的 `config.yaml`
- 支持中英双语帮助信息和运行时提示
- 可作为 Go 库引用

## 适用场景

适合以下用途：

- 不运行 Docker，直接离线查看镜像内容
- 分析 Harbor 导出的镜像包
- 查看业务文件、依赖文件和运行时布局
- 低内存处理大体积镜像包
- 批量解包某个目录下的多个镜像归档

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

## 基本用法

```bash
./udf [选项] <归档文件|目录|通配符>...
```

示例：

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
./udf --lang zh ./image.tar
```

## 输入方式

支持以下输入：

- 单个归档文件
- 一个目录
- 一个通配符模式
- 一次传入多个输入项

示例：

```bash
./udf ./image.tar
./udf ./images
./udf "./images/*.tar"
./udf ./a.tar ./b.tar.gz "./repo/*.zip"
```

目录输入只扫描当前一层，不递归子目录。

## 子命令

### `ls` — 不解压列出镜像目录内容

`udf ls` 在内存中构建合并后的文件系统视图，并按 `ls -al` 的方式输出，整个过程不会向磁盘写入任何文件。

```bash
./udf ls ./image.tar                # 列出镜像根目录
./udf ls ./image.tar /etc           # 列出镜像内的某个目录
./udf ls ./image.tar /etc/passwd    # 查看单个文件
./udf ls -t repo/app:latest ./image.tar /usr/local/bin
```

示例输出：

```text
$ ./udf ls ./image.tar /etc
镜像内容: ./image.tar
total 2
-rw-r--r--   1 root      root           30 Sep 13  2020 passwd
lrwxrwxrwx   1 root      root           19 Sep 13  2020 resolv.conf -> /run/systemd/resolve
```

说明：

- 层的合并与完整解包完全一致，包含 whiteout 和 opaque 目录语义
- 长格式包含权限、链接数、属主、属组、大小、修改时间和名称；软链接会显示指向目标
- 路径前导 `/` 可省略；`/` 或 `.` 表示镜像根目录
- 多镜像归档可使用 `-t` / `-i` 选择镜像

### `cp` — 单独提取某个文件或目录

`udf cp` 只流式提取选中范围内的条目，同样遵循合并后的最终视图，无需解包整个镜像。

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

## 输出规则

如果不指定 `-o/--output`：

- 默认输出到输入文件所在目录

如果指定 `-o/--output`：

- 输出到指定父目录下

输出目录结构：

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

- `-o, --output`
  - 输出父目录
  - 实际会在其下创建一个同名子目录
  - 默认是输入文件所在目录
- `-f, --force`
  - 强制写入已存在的非空目标目录
  - 不会预先清空目录
- `-t, --repo-tag`
  - 按 `manifest.json` 中的 `RepoTags` 选择镜像
  - 一个归档里有多个镜像时，推荐优先使用
- `-i, --image-index`
  - 按 `manifest.json` 数组中的索引选择镜像
- `-b, --buffer-size`
  - 文件复制缓冲区大小，单位字节
- `-l, --lang`
  - 界面语言：`zh` 或 `en`
- `--no-progress`
  - 禁用动态进度条

`ls` 和 `cp` 子命令共用 `-t`、`-i`（`cp` 另有 `-b`）；子命令的 `--lang` 不带 `-l` 简写。

## 多镜像归档说明

如果一个归档里只有一个镜像：

- 不需要指定 `-t` 或 `-i`

如果一个归档里有多个镜像：

- 必须指定提取哪一个；程序不做交互——未指定时会直接报错退出，错误信息中会列出可选值
- 通常建议使用 `-t`
- 从错误信息中选定一个值后，带上 `-t` 或 `-i` 重新运行

`-t` 和 `-i` 的区别：

- `-t` 是按 tag 选
- `-i` 是按 `manifest.json` 中的位置选

## 生成文件

每个解包结果目录中，`udf` 会写出：

- 合并后的 `rootfs`
- `config.yaml`

其中 `config.yaml` 来自原始 `config.json`，并尽量按更易读的方式格式化字符串内容。

## 错误处理

- 批量模式下，单个归档失败不会中断其他归档
- 批量模式下，非镜像归档会自动跳过
- 退出码：至少一个镜像处理成功时返回 0（即使批量模式中其他镜像失败），一个都没成功时返回 1
- 项目自身产生的用户可见提示支持中英文
- 系统底层错误会原样保留，便于排查问题

## 技术说明

- 层合并顺序以 `manifest.json` 为准
- 解包过程中实时处理 whiteout
- 层内压缩流自动识别：支持 gzip、bzip2 和未压缩 tar
- 为避免中途目录权限导致写入失败，目录权限和时间戳会在解包完成后统一恢复
- 使用流式方式处理 layer，避免把所有层先完整落盘，尽量降低内存占用

## 已知限制

- 不支持 zstd 压缩的层（会明确报错）
- 目录输入只扫描当前一层，不递归子目录
- 多镜像归档必须显式指定 `-t` / `-i`，程序不会交互式询问

## 作为库使用

所有代码都可以作为 Go 库引用，不存在 `internal/` 目录：

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

当前支持：

- 离线镜像归档解包
- 批量处理
- 不解压列出镜像目录内容（`ls`）
- 单独提取文件或目录（`cp`）
- 中英双语 CLI
- `config.yaml` 导出

当前不定位为：

- Docker 替代品
- 容器运行时
- OCI Registry 客户端

## License

本项目采用 MIT License 开源，详见 [LICENSE](./LICENSE)。