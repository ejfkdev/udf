package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ejfkdev/xyz-go/langx"
)

// helpTexts collects the custom help blocks for one interface language.
type helpTexts struct {
	summary       string
	versionLabel  string
	repoLabel     string
	examplesLabel string
	examples      string
	options       string
	extractAfter  string
}

// helpTextsByLang holds the udf-owned help content per interface language;
// the framework strings themselves are localized by xyz-go.
var helpTextsByLang = map[langx.Language]helpTexts{
	langx.ZhCn: {
		summary:       "将 Harbor / docker save 导出的镜像归档解包为合并后的 rootfs；一个二进制提供 CLI、HTTP REST、MCP 三种接口，所有输入都是本机文件路径，不做上传",
		versionLabel:  "版本",
		repoLabel:     "仓库",
		examplesLabel: "示例",
		examples: `  udf info ./image.tar                       查看镜像元数据
  udf ls ./image.tar /etc                   列出镜像内 /etc（不落盘）
  udf cp ./image.tar /etc/passwd ./passwd   单独提取一个文件
  udf ./image.tar -o ./out                  解包整镜像（extract 为默认命令）
  udf serve --addr 127.0.0.1:8080           启动 HTTP REST 服务
  udf mcp stdio                             启动 MCP stdio 服务`,
		options: `内置选项:
  -h, --help         显示帮助（总览或当前子命令）
  -v, --version      显示版本号
  --json             命令结果以 JSON 输出
  completion         生成 shell 补全脚本: udf completion bash|zsh|fish
  --xyz.lang         界面语言: en | zh-CN（默认自动检测 LANG/LC_ALL）
  serve 与 mcp(http/sse) 支持 --addr、--bearer、--cors、--tls-cert/--tls-key、--timeout、--log-level
  mcp 另有 --versions、--session-timeout；全局等价写法 --xyz.<参数>（详见 xyz-go 文档）`,
		extractAfter: `extract 是默认命令，可省略子命令：
  udf ./image.tar         等同  udf extract ./image.tar
省略写法下 flag 请放在路径之后（udf ./image.tar -o out）；需要 flag 前置时使用显式写法（udf extract -o out ./image.tar）。`,
	},
	langx.En: {
		summary:       "Extract Harbor / docker-save image archives into a merged rootfs; one binary speaks CLI, HTTP REST and MCP, and all inputs are local file paths - nothing is uploaded",
		versionLabel:  "Version",
		repoLabel:     "Repository",
		examplesLabel: "Examples",
		examples: `  udf info ./image.tar                       show image metadata
  udf ls ./image.tar /etc                   list /etc inside the image (no extraction)
  udf cp ./image.tar /etc/passwd ./passwd   extract a single file
  udf ./image.tar -o ./out                  extract the whole image (extract is the default command)
  udf serve --addr 127.0.0.1:8080           start the HTTP REST service
  udf mcp stdio                             start the MCP stdio server`,
		options: `Built-in options:
  -h, --help         show help (overview or per-command)
  -v, --version      show version
  --json             print command results as JSON
  completion         generate shell completion: udf completion bash|zsh|fish
  --xyz.lang         interface language: en | zh-CN (default: auto-detect LANG/LC_ALL)
  serve and mcp(http/sse) accept --addr, --bearer, --cors, --tls-cert/--tls-key, --timeout, --log-level
  mcp also accepts --versions, --session-timeout; global equivalent: --xyz.<param> (see the xyz-go docs)`,
		extractAfter: `extract is the default command and may be omitted:
  udf ./image.tar         same as  udf extract ./image.tar
with the shorthand form, put flags after the archive path (udf ./image.tar -o out); use the explicit form to lead with flags (udf extract -o out ./image.tar).`,
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
		appName(), t.summary, t.versionLabel, version, t.repoLabel, helpRepoURL, t.examplesLabel, t.examples)
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
