package main

import (
	xyz "github.com/ejfkdev/xyz-go"
)

// version is stamped at build time by the release workflow:
//
//	go build -ldflags "-X main.version=v0.2.0"
var version = "dev"

func main() {
	xyz.Version = version

	xyz.Define("info", infoImage).
		Summary("Show image archive metadata").
		CLI(xyz.CliHints{Usage: "info <archive>"}).
		HTTP(xyz.HTTPHints{Method: "GET", Path: "/info"}).
		MCP(xyz.MCPHints{Annotations: []string{"read"}}).
		Also(
			xyz.Define("ls", listImage).
				Summary("List directory contents of the merged image filesystem").
				CLI(xyz.CliHints{Usage: "ls <archive> [path]"}).
				HTTP(xyz.HTTPHints{Method: "GET", Path: "/ls"}).
				MCP(xyz.MCPHints{Annotations: []string{"read"}}),

			xyz.Define("cp", copyEntry).
				Summary("Extract a single file or directory from the image").
				CLI(xyz.CliHints{Usage: "cp <archive> <src-path> <dest>"}).
				HTTP(xyz.HTTPHints{Method: "POST", Path: "/cp"}).
				MCP(xyz.MCPHints{Annotations: []string{"write"}}),

			xyz.Define("extract", extractImages).
				Summary("Extract the merged rootfs of one or more image archives").
				CLI(xyz.CliHints{Usage: "extract <archive...>", Default: true}).
				HTTP(xyz.HTTPHints{Method: "POST", Path: "/extract"}).
				MCP(xyz.MCPHints{Annotations: []string{"write"}}),
		).
		Run()
}
