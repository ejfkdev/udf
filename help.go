package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ejfkdev/xyz-go/langx"
)

// exampleLine is one usage example: the command as typed, and what it does.
type exampleLine struct{ cmd, what string }

// helpTexts collects the custom help blocks for one interface language.
type helpTexts struct {
	summary       string
	versionLabel  string
	repoLabel     string
	examplesLabel string
	examples      []exampleLine
	formats       string
	options       string
	extractAfter  string
	catAfter      string
	xxdAfter      string
}

// helpTextsByLang holds the udf-owned help content per interface language;
// the framework strings themselves are localized by xyz-go.
var helpTextsByLang = map[langx.Language]helpTexts{
	langx.ZhCn: {
		summary:       "从归档、虚拟磁盘与文件系统镜像中提取内容，并可列出合并后的文件系统；一个二进制提供 CLI、HTTP REST、MCP 三种接口，所有输入都是本机文件路径，不做上传",
		versionLabel:  "版本",
		repoLabel:     "仓库",
		examplesLabel: "示例",
		examples: []exampleLine{
			{"udf info ./image.tar", "查看镜像元数据（多镜像归档每行一个镜像）"},
			{"udf info --tag safeline-mgt:latest ./img.tar", "多镜像：按 tag 选择（短名即可，唯一时）"},
			{"udf ls ./image.tar /etc", "列出镜像内 /etc（不落盘）"},
			{"udf cp ./image.tar /etc/passwd ./passwd", "单独提取一个文件"},
			{"udf cat ./image.tar /etc/passwd", "单个文件内容写到 stdout（可管道）"},
			{"udf xxd ./image.tar /etc/passwd", "以十六进制预览文件头"},
			{"udf ls ./boot.img", "安卓 boot 镜像：内核/ramdisk 及 ramdisk 内文件"},
			{"udf ls ./system.img p2/system", "安卓动态分区：GPT 磁盘内 super 里的 system"},
			{"udf ls ./app.exe", "py2exe：PE 资源（PYTHONSCRIPT 为 .pyc）+ 附加模块归档"},
			{"udf ls ./disk.qcow2", "磁盘镜像：列出分区/逻辑卷"},
			{"udf cp ./disk.qcow2 /p1/etc/hostname .", "从磁盘镜像提取单个文件"},
			{"udf ./image.tar -o ./out", "解包整镜像（extract 为默认命令）"},
			{"udf serve --addr 127.0.0.1:8080", "启动 HTTP REST 服务"},
			{"udf mcp stdio", "启动 MCP stdio 服务"},
		},
		formats: `支持的输入（按文件内容识别，不靠扩展名）:
  归档:     tar / tar.gz(tgz) / tar.xz / tar.bz2 / tar.zst / tar.lz4 / zip / 7z / rar /
            cpio(cpio.gz/xz/zst，含 initramfs) / asar / rpm / deb(ipk) / cab（含 .msi 内嵌）/ nar(nix) / xar(.pkg) / .ppkg，以及 OCI 布局 / OCI 归档(.oci.tar，含 flatpak) / docker save
  可执行封装: pyinstaller(onefile，含 PYZ，重建 .pyc) / py2exe(PYTHONSCRIPT 解码为 .pyc，附加模块 zip 列出) /
            nuitka onefile(附加与内嵌载荷) / .NET single-file(bundle v1/v2/v6，含 deflate) / zip 自解压 exe
  安卓:     boot.img(v0-v4：内核/ramdisk/dtb 及 ramdisk 内 cpio 文件，含 legacy LZ4 压缩的 ramdisk) /
            sparse 稀疏 img(磁盘容器) / super 动态分区(按 extent 映射读取，独立或 GPT 内均可) /
            resources.arsc(资源表解码，名称与引用可读)；APK 内二进制 AXML 自动解码为文本 XML
  游戏资源: Unity AssetBundle(UnityFS：按节点解压 none/LZ4/LZMA；LZHAM 与加密会明确报不支持)
  虚拟磁盘: qcow2 / qcow1 / vmdk / vhd(vhdx) / vdi / qed / parallels / vma / ova(ovf) / sif / ffu /
            wim(esd/swm) / raw(img，含 GPT/MBR 分区磁盘) / ami / appimage / 安卓 sparse img 与 super 动态分区
  文件系统: ext2/3/4 / xfs / btrfs / ntfs / squashfs / iso9660 / udf / exfat / erofs(未压缩) / fat12/16/32，含 LVM2 逻辑卷`,
		options: `内置选项:
  -h, --help         显示帮助（总览或当前子命令）
  -v, --version      显示版本号
  --json             命令结果以 JSON 输出
  completion         生成 shell 补全脚本: udf completion bash|zsh|fish
  --xyz.lang         界面语言: en | zh-CN（默认自动检测 LANG/LC_ALL）
  serve 与 mcp(http/sse) 支持 --addr、--bearer、--cors、--tls-cert/--tls-key、--timeout、--log-level
  mcp 另有 --versions、--session-timeout；全局等价写法 --xyz.<参数>（详见 xyz-go 文档）
  缓存与临时文件: <系统临时目录>/ejfkdev/udf（UDF_CACHE_DIR 可改；随时可删，都会按需重建）`,
		extractAfter: `extract 是默认命令，可省略子命令：
  udf ./image.tar         等同  udf extract ./image.tar
省略写法下 flag 请放在路径之后（udf ./image.tar -o out）；需要 flag 前置时使用显式写法（udf extract -o out ./image.tar）。`,
		catAfter: `cat 与系统 cat 命令行为一致：把文件字节原样写到 stdout，可直接接管道。
推荐用于预览小型文本文件；大文件或二进制文件不建议打印到终端，改用 xxd 查看前部有限字节。`,
		xxdAfter: `xxd 以前几个字节的十六进制加 ASCII 形式打印（同系统 xxd -l / hexdump -C），
便于按文件头判断类型。默认 256 字节，用 -n 增大量、-s 跳过偏移。`,
	},
	langx.En: {
		summary:       "Extract the contents of archives, virtual disk and filesystem images, and list the merged filesystem; one binary provides CLI, HTTP REST and MCP interfaces, and all inputs are local file paths - nothing is uploaded",
		versionLabel:  "Version",
		repoLabel:     "Repository",
		examplesLabel: "Examples",
		examples: []exampleLine{
			{"udf info ./image.tar", "show image metadata (a multi-image archive: one line per image)"},
			{"udf info --tag safeline-mgt:latest ./img.tar", "several images: pick one by tag (a short name is enough when unique)"},
			{"udf ls ./image.tar /etc", "list /etc inside the image (no extraction)"},
			{"udf cp ./image.tar /etc/passwd ./passwd", "extract a single file"},
			{"udf cat ./image.tar /etc/passwd", "write a single file to stdout (pipeable)"},
			{"udf xxd ./image.tar /etc/passwd", "hex-dump a file header"},
			{"udf ls ./boot.img", "Android boot image: kernel/ramdisk and the ramdisk's files"},
			{"udf ls ./system.img p2/system", "Android dynamic partitions: system inside a GPT disk's super"},
			{"udf ls ./app.exe", "py2exe: PE resources (PYTHONSCRIPT as .pyc) + appended module archive"},
			{"udf ls ./disk.qcow2", "disk image: list partitions / logical volumes"},
			{"udf cp ./disk.qcow2 /p1/etc/hostname .", "extract a single file from a disk image"},
			{"udf ./image.tar -o ./out", "extract the whole image (extract is the default command)"},
			{"udf serve --addr 127.0.0.1:8080", "start the HTTP REST service"},
			{"udf mcp stdio", "start the MCP stdio server"},
		},
		formats: `Supported inputs (detected by content, not extension):
  archives:      tar / tar.gz (tgz) / tar.xz / tar.bz2 / tar.zst / tar.lz4 / zip / 7z / rar /
                 cpio (cpio.gz/xz/zst, incl. initramfs) / asar / rpm / deb (ipk) / cab (incl. cabs inside .msi) / nar (nix) / xar (.pkg) / .ppkg, plus OCI layouts, oci-archive tars (incl. flatpak) and docker save dirs
  exe bundles:   pyinstaller (onefile, incl. PYZ, rebuilds .pyc) / py2exe (PYTHONSCRIPT decoded to .pyc,
                 appended module archive listed) / nuitka onefile (appended and embedded payloads) /
                 .NET single-file (bundle v1/v2/v6, incl. deflate) / zip self-extracting exes
  android:       boot.img (v0-v4: kernel/ramdisk/dtb and the ramdisk's own cpio files, incl. LZ4-legacy ramdisks) /
                 sparse img (disk container) / super partitions (dynamic partitions read through their extent mappings,
                 standalone or inside a GPT disk) / resources.arsc (resource table decoded, names and references readable)
  game assets:   Unity AssetBundles (UnityFS: per-node decompression, none/LZ4/LZMA; LZHAM and encryption are reported unsupported)
  disk images:   qcow2 / qcow1 / vmdk / vhd (vhdx) / vdi / qed / parallels / vma / ova (ovf) / sif / ffu /
                 wim (esd/swm) / raw (img, incl. GPT/MBR-partitioned disks) / ami / appimage / Android sparse img and super partitions
  filesystems:   ext2/3/4 / xfs / btrfs / ntfs / squashfs / iso9660 / udf / exfat / erofs (uncompressed) / fat12/16/32,
                 incl. LVM2 logical volumes`,
		options: `Built-in options:
  -h, --help         show help (overview or per-command)
  -v, --version      show version
  --json             print command results as JSON
  completion         generate shell completion: udf completion bash|zsh|fish
  --xyz.lang         interface language: en | zh-CN (default: auto-detect LANG/LC_ALL)
  serve and mcp(http/sse) accept --addr, --bearer, --cors, --tls-cert/--tls-key, --timeout, --log-level
  mcp also accepts --versions, --session-timeout; global equivalent: --xyz.<param> (see the xyz-go docs)
  cache and scratch: <system temp dir>/ejfkdev/udf (UDF_CACHE_DIR overrides; safe to delete, everything is rebuilt on demand)`,
		extractAfter: `extract is the default command and may be omitted:
  udf ./image.tar         same as  udf extract ./image.tar
with the shorthand form, put flags after the archive path (udf ./image.tar -o out); use the explicit form to lead with flags (udf extract -o out ./image.tar).`,
		catAfter: `cat behaves like the OS cat: it writes the file's bytes to stdout unchanged, ready for piping.
Prefer it for previewing small text files; avoid printing large binary files to the terminal (use xxd to inspect a bounded prefix instead).`,
		xxdAfter: `xxd prints the first bytes as hex plus ASCII, like the OS 'xxd -l' / 'hexdump -C', so you can
identify a file by its header. Default is 256 bytes; raise it with -n and start later in the file with -s.`,
	},
}

// helpTextFor returns the block set for the resolved interface language,
// falling back to English for unknown values.
func helpTextFor(l langx.Language) helpTexts {
	if t, ok := helpTextsByLang[l]; ok {
		return t
	}
	return helpTextsByLang[langx.En]
}

// effectiveLang resolves the interface language with the same precedence the
// dispatcher uses: --xyz.lang flag > LANG/LC_ALL detection > English. The
// flag is scanned here so udf's own help blocks stay in sync with the
// framework surfaces.
func effectiveLang() langx.Language {
	args := os.Args[1:]
	for i, arg := range args {
		switch {
		case strings.HasPrefix(arg, "--xyz.lang="):
			if l, ok := langx.Parse(strings.TrimPrefix(arg, "--xyz.lang=")); ok {
				return l
			}
		case arg == "--xyz.lang":
			if i+1 < len(args) {
				if l, ok := langx.Parse(args[i+1]); ok {
					return l
				}
			}
		}
	}
	return langx.Detect()
}

// helpBeforeBlock builds the overview header inserted via
// xyz.Config.HelpBefore: program name, description, version, repository and
// usage examples, localized for the given language.
func helpBeforeBlock(l langx.Language) string {
	t := helpTextFor(l)
	return fmt.Sprintf("%s — %s\n\n%s: %s\n%s: %s\n\n%s:\n%s",
		appName(), t.summary, t.versionLabel, version, t.repoLabel, helpRepoURL, t.examplesLabel, renderExamples(t.examples))
}

// renderExamples aligns the example commands in two columns, so a longer
// command never shifts the descriptions.
func renderExamples(lines []exampleLine) string {
	width := 0
	for _, line := range lines {
		if n := len(line.cmd); n > width {
			width = n
		}
	}
	var sb strings.Builder
	for i, line := range lines {
		if i > 0 {
			sb.WriteByte('\n')
		}
		fmt.Fprintf(&sb, "  %-*s  %s", width, line.cmd, line.what)
	}
	return sb.String()
}

// helpAfterBlock builds the footer inserted via xyz.Config.HelpAfter: the
// supported input formats followed by the built-in options.
func helpAfterBlock(l langx.Language) string {
	t := helpTextFor(l)
	return t.formats + "\n\n" + t.options
}

// appName derives the invoked binary name for the help header, falling back
// to "udf" when it cannot be determined.
func appName() string {
	name := filepath.Base(os.Args[0])
	if name == "" || name == "." || name == "/" {
		return "udf"
	}
	return name
}
