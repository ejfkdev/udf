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

// helpRepoURL is the project home shown in the root help.
const helpRepoURL = "https://github.com/ejfkdev/udf"

func main() {
	xyz.Version = version
	lang := effectiveLang()

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
		xyz.CliHints{Usage: "extract <archive...>", Default: true, After: helpTextFor(lang).extractAfter},
		xyz.HTTPHints{Method: "POST", Path: "/extract"},
		[]string{"write"})

	cfg := xyz.Config{
		// 界面语言与派发器保持一致（--xyz.lang 旗标优先于环境检测）。
		Lang: lang.String(),
		// 总览开头：程序名、描述、版本号、仓库地址与示例；结尾：内置选项清单。
		HelpBefore: helpBeforeBlock(lang),
		HelpAfter:  helpTextFor(lang).options,
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
