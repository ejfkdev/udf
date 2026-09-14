package fsview

import (
	"archive/tar"
	"bytes"
	"io"
	"sort"
	"strings"
	"testing"
	"time"
)

type layerEntry struct {
	Header *tar.Header
	Body   []byte
}

func TestBuildMergesLayersInOrder(t *testing.T) {
	root := buildTree(t, map[string][]layerEntry{
		"base.tar": {
			fileEntry("app/file.txt", "base"),
			dirEntry("app"),
			fileEntry("keep.txt", "keep"),
		},
		"top.tar": {
			fileEntry("app/file.txt", "top"),
		},
	})

	node := root.Resolve("app/file.txt")
	if node == nil {
		t.Fatal("expected app/file.txt to exist")
	}
	if node.Layer != "top.tar" {
		t.Fatalf("unexpected defining layer: %s", node.Layer)
	}
	if got := root.Resolve("keep.txt"); got == nil {
		t.Fatal("expected keep.txt from base layer to survive")
	}
}

func TestBuildAppliesWhiteout(t *testing.T) {
	root := buildTree(t, map[string][]layerEntry{
		"base.tar": {
			fileEntry("etc/greeting", "hello"),
		},
		"top.tar": {
			fileEntry("etc/.wh.greeting", ""),
		},
	})

	if got := root.Resolve("etc/greeting"); got != nil {
		t.Fatalf("expected whiteout to remove entry, got %+v", got)
	}
}

func TestBuildAppliesOpaqueDirectory(t *testing.T) {
	root := buildTree(t, map[string][]layerEntry{
		"base.tar": {
			fileEntry("app/old.txt", "old"),
			fileEntry("app/sub/deep.txt", "deep"),
		},
		"top.tar": {
			fileEntry("app/.wh..wh..opq", ""),
			fileEntry("app/new.txt", "new"),
		},
	})

	if got := root.Resolve("app/old.txt"); got != nil {
		t.Fatal("expected opaque whiteout to clear old file")
	}
	if got := root.Resolve("app/sub"); got != nil {
		t.Fatal("expected opaque whiteout to clear nested directory")
	}
	if got := root.Resolve("app/new.txt"); got == nil {
		t.Fatal("expected file after opaque whiteout to exist")
	}
}

func TestBuildLaterLayerDirectoryKeepsEarlierChildren(t *testing.T) {
	root := buildTree(t, map[string][]layerEntry{
		"base.tar": {
			fileEntry("app/one.txt", "1"),
		},
		"top.tar": {
			dirEntry("app"),
			fileEntry("app/two.txt", "2"),
		},
	})

	if got := root.Resolve("app/one.txt"); got == nil {
		t.Fatal("expected earlier layer child to survive later dir entry")
	}
	if got := root.Resolve("app/two.txt"); got == nil {
		t.Fatal("expected later layer child to exist")
	}
}

func TestBuildTracksSymlinkAndHardlink(t *testing.T) {
	root := buildTree(t, map[string][]layerEntry{
		"base.tar": {
			symlinkEntry("lib/current", "current-v2"),
			hardlinkEntry("bin/tool", "/lib/current"),
		},
	})

	current := root.Resolve("lib/current")
	if current == nil || current.Kind != KindSymlink || current.Linkname != "current-v2" {
		t.Fatalf("unexpected symlink node: %+v", current)
	}
	tool := root.Resolve("bin/tool")
	if tool == nil || tool.Kind != KindHardlink || tool.Linkname != "/lib/current" {
		t.Fatalf("unexpected hardlink node: %+v", tool)
	}
}

func TestBuildRejectsPathEscape(t *testing.T) {
	_, err := buildTreeErr(map[string][]layerEntry{
		"base.tar": {
			fileEntry("../escape.txt", "bad"),
		},
	})
	if err == nil {
		t.Fatal("expected path escape error")
	}
}

func TestNormalizePath(t *testing.T) {
	cases := map[string]string{
		"/etc/passwd":  "etc/passwd",
		"etc/passwd":   "etc/passwd",
		"./etc/passwd": "etc/passwd",
		"/":            "",
		".":            "",
	}
	for input, want := range cases {
		got, err := NormalizePath(input)
		if err != nil {
			t.Fatalf("normalize %q: %v", input, err)
		}
		if got != want {
			t.Fatalf("normalize %q = %q, want %q", input, got, want)
		}
	}

	if _, err := NormalizePath("../etc"); err == nil {
		t.Fatal("expected error for escaping path")
	}
}

func buildTree(t *testing.T, layers map[string][]layerEntry) *Node {
	t.Helper()
	root, err := buildTreeErr(layers)
	if err != nil {
		t.Fatalf("build tree: %v", err)
	}
	return root
}

func buildTreeErr(layers map[string][]layerEntry) (*Node, error) {
	var order []string
	for name := range layers {
		order = append(order, name)
	}
	sort.Strings(order)
	open := func(name string) (io.Reader, func(), error) {
		return bytes.NewReader(buildLayerBytes(layers[name])), func() {}, nil
	}
	return Build(order, open)
}

func buildLayerBytes(entries []layerEntry) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, entry := range entries {
		_ = tw.WriteHeader(entry.Header)
		if len(entry.Body) > 0 {
			_, _ = tw.Write(entry.Body)
		}
	}
	_ = tw.Close()
	return buf.Bytes()
}

func fileEntry(name, body string) layerEntry {
	return layerEntry{
		Header: &tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(body)),
			ModTime:  time.Unix(1600000000, 0),
			Uid:      0,
			Gid:      0,
			Uname:    "root",
			Gname:    "root",
			Typeflag: tar.TypeReg,
		},
		Body: []byte(body),
	}
}

func dirEntry(name string) layerEntry {
	return layerEntry{
		Header: &tar.Header{
			Name:     strings.TrimSuffix(name, "/") + "/",
			Mode:     0o755,
			Typeflag: tar.TypeDir,
		},
	}
}

func symlinkEntry(name, target string) layerEntry {
	return layerEntry{
		Header: &tar.Header{
			Name:     name,
			Mode:     0o777,
			Linkname: target,
			Typeflag: tar.TypeSymlink,
		},
	}
}

func hardlinkEntry(name, target string) layerEntry {
	return layerEntry{
		Header: &tar.Header{
			Name:     name,
			Mode:     0o644,
			Linkname: target,
			Typeflag: tar.TypeLink,
		},
	}
}

func TestBuildKeepsDeviceNodes(t *testing.T) {
	root := buildTree(t, map[string][]layerEntry{
		"base.tar": {
			dirEntry("dev"),
			{Header: &tar.Header{
				Name: "dev/console", Mode: 0o600,
				Typeflag: tar.TypeChar, Devmajor: 5, Devminor: 1,
			}},
			{Header: &tar.Header{
				Name: "dev/fifo", Mode: 0o644, Typeflag: tar.TypeFifo,
			}},
			fileEntry("keep.txt", "x"),
		},
	})

	console := root.Resolve("dev/console")
	if console == nil {
		t.Fatal("expected dev/console to be merged into the tree")
	}
	if console.Kind != KindCharDev {
		t.Fatalf("dev/console kind = %v, want KindCharDev", console.Kind)
	}
	if console.Devmajor != 5 || console.Devminor != 1 {
		t.Fatalf("dev/console device = %d,%d, want 5,1", console.Devmajor, console.Devminor)
	}
	if !IsDeviceKind(console.Kind) {
		t.Fatal("IsDeviceKind(KindCharDev) = false")
	}

	fifo := root.Resolve("dev/fifo")
	if fifo == nil || fifo.Kind != KindFifo {
		t.Fatalf("dev/fifo = %+v, want KindFifo node", fifo)
	}
	if !IsDeviceKind(fifo.Kind) {
		t.Fatal("IsDeviceKind(KindFifo) = false")
	}
}
