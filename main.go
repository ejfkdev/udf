package main

import (
	"log"
	"os"

	xyz "github.com/ejfkdev/xyz-go"
	"github.com/ejfkdev/xyz-go/registry"
)

// version is stamped at build time by the release workflow:
//
//	go build -ldflags "-X main.version=v0.2.0"
var version = "dev"

// helpSummary is the short description shown at the top of the root help.
const helpSummary = "将 Harbor / docker save 导出的镜像归档解包为合并后的 rootfs；一个二进制提供 CLI、HTTP REST、MCP 三种接口，所有输入都是本机文件路径，不做上传"

// helpRepoURL is the project home shown in the root help.
const helpRepoURL = "https://github.com/ejfkdev/udf"

// extractHelpAfter is appended to `udf extract -h`: extract is the default
// subcommand, and the forwarding rule prefers flags after the archive path.
const extractHelpAfter = `extract 是默认命令，可省略子命令：
  udf ./image.tar         等同  udf extract ./image.tar
省略写法下 flag 请放在路径之后（udf ./image.tar -o out）；需要 flag 前置时使用显式写法（udf extract -o out ./image.tar）。`

func main() {
	xyz.Version = version

	reg := registry.New()
	register(reg, "info", "Show image archive metadata", infoImage,
		xyz.CliHints{Usage: "info <archive>"},
		xyz.HTTPHints{Method: "GET", Path: "/info"},
		[]string{"read"})
	register(reg, "ls", "List directory contents of the merged image filesystem", listImage,
		xyz.CliHints{Usage: "ls <archive> [path]"},
		xyz.HTTPHints{Method: "GET", Path: "/ls"},
		[]string{"read"})
	register(reg, "cp", "Extract a single file or directory from the image", copyEntry,
		xyz.CliHints{Usage: "cp <archive> <src-path> <dest>"},
		xyz.HTTPHints{Method: "POST", Path: "/cp"},
		[]string{"write"})
	register(reg, "extract", "Extract the merged rootfs of one or more image archives", extractImages,
		xyz.CliHints{Usage: "extract <archive...>", Default: true, After: extractHelpAfter},
		xyz.HTTPHints{Method: "POST", Path: "/extract"},
		[]string{"write"})

	cfg := xyz.Config{
		// 总览开头：程序名、描述、版本号、仓库地址与示例；结尾：内置选项清单。
		HelpBefore: helpBeforeBlock(),
		HelpAfter:  helpOptionsBlock,
	}
	os.Exit(xyz.RunConfig(reg, os.Args[1:], cfg))
}

// register declares one command into the registry; the same definition
// backs its CLI, HTTP and MCP interfaces.
func register[T, R any](reg *registry.Registry, name, summary string, h xyz.Handler[T, R], cli xyz.CliHints, httpHints xyz.HTTPHints, mcpAnnotations []string) {
	_, err := xyz.Define(name, h).
		Summary(summary).
		CLI(cli).
		HTTP(httpHints).
		MCP(xyz.MCPHints{Annotations: mcpAnnotations}).
		Register(reg)
	if err != nil {
		log.Fatal(err)
	}
}
