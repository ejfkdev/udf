# udf

`udf` 是一个 Go 编写的命令行工具，用于将 Harbor / Docker 导出的镜像归档解包为合并后的根文件系统（`rootfs`）。

它适合离线镜像分析、大体积镜像包处理，以及批量解包场景，支持多种归档格式、分层文件系统合并、whiteout 处理和中英双语命令行输出。

English version: [README.md](./README.md)

## 功能特性

- 将镜像归档解包为合并后的 `rootfs`
- 支持单文件、目录、通配符三种输入方式
- 支持外层归档格式：
  - `.tar`
  - `.tar.gz`
  - `.tgz`
  - `.zip`
- 支持常见镜像归档结构：
  - 平铺结构：`manifest.json + config.json + layers/...`
  - 经典 `docker save` 结构：`<layer-id>/layer.tar`
- 按 `manifest.json` 中的层顺序正确合并
- 正确处理 whiteout 文件和 opaque 目录
- 支持软链接和硬链接
- 当目标文件系统不支持硬链接时自动降级为文件复制
- 将原始 `config.json` 导出为可读性更高的 `config.yaml`
- 支持中英双语帮助信息和运行时提示

## 适用场景

适合以下用途：

- 不运行 Docker，直接离线查看镜像内容
- 分析 Harbor 导出的镜像包
- 查看业务文件、依赖文件和运行时布局
- 低内存处理大体积镜像包
- 批量解包某个目录下的多个镜像归档

## 安装

### 使用 `go install`

```bash
go install github.com/ejfdkev/udf@latest
```

### 从源码编译

```bash
git clone https://github.com/ejfdkev/udf.git
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

## 多镜像归档说明

如果一个归档里只有一个镜像：

- 不需要指定 `-t` 或 `-i`

如果一个归档里有多个镜像：

- 需要指定具体提取哪一个
- 通常建议使用 `-t`
- 如果未指定，程序会列出可选值并提示你重新选择

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
- 项目自身产生的用户可见提示支持中英文
- 系统底层错误会原样保留，便于排查问题

## 技术说明

- 层合并顺序以 `manifest.json` 为准
- 解包过程中实时处理 whiteout
- 部分 Harbor 导出的 layer 可能是压缩流，`udf` 会自动识别常见情况
- 为避免中途目录权限导致写入失败，目录权限和时间戳会在解包完成后统一恢复
- 使用流式方式处理 layer，避免把所有层先完整落盘，尽量降低内存占用

## 当前范围

当前支持：

- 离线镜像归档解包
- 批量处理
- 中英双语 CLI
- `config.yaml` 导出

当前不定位为：

- Docker 替代品
- 容器运行时
- OCI Registry 客户端

## License

本项目采用 MIT License 开源，详见 [LICENSE](./LICENSE)。
