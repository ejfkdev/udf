package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// helpExamplesBlock lists quick-start usage lines shown at the top of the
// root help overview.
const helpExamplesBlock = `  udf info ./image.tar                       查看镜像元数据
  udf ls ./image.tar /etc                   列出镜像内 /etc（不落盘）
  udf cp ./image.tar /etc/passwd ./passwd   单独提取一个文件
  udf ./image.tar -o ./out                  解包整镜像（extract 为默认命令）
  udf serve --addr 127.0.0.1:8080           启动 HTTP REST 服务
  udf mcp stdio                             启动 MCP stdio 服务`

// helpBeforeBlock builds the overview header inserted via
// xyz.Config.HelpBefore: program name, description, version, repository and
// usage examples.
func helpBeforeBlock() string {
	return fmt.Sprintf("%s — %s\n\n版本: %s\n仓库: %s\n\n示例:\n%s",
		appName(), helpSummary, version, helpRepoURL, helpExamplesBlock)
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

// helpOptionsBlock is appended at the bottom of the root help overview via
// xyz.Config.HelpAfter: it documents the built-in flags and the network-mode
// parameters provided by xyz-go.
const helpOptionsBlock = `内置选项:
  -h, --help         显示帮助（总览或当前子命令）
  -v, --version      显示版本号
  --json             命令结果以 JSON 输出
  completion         生成 shell 补全脚本: udf completion bash|zsh|fish
  serve 与 mcp(http/sse) 支持 --addr、--bearer、--cors、--tls-cert/--tls-key、--timeout、--log-level
  mcp 另有 --versions、--session-timeout；全局等价写法 --xyz.<参数>（详见 xyz-go 文档）`
