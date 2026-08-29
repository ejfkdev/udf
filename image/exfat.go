package image

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/dsoprea/go-exfat"
)

func baseName(p string) string {
	p = strings.Trim(p, "/")
	if p == "" || p == "." {
		return "."
	}
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// exfatFS adapts the go-exfat reader to the diskVolumeReader interface. exFAT
// has no symlinks; modes are synthesized (exFAT has no Unix permissions).
type exfatFS struct {
	er   *exfat.ExfatReader
	root *exfat.TreeNode
}

func openExFATFS(rs io.ReadSeeker) (*exfatFS, error) {
	er := exfat.NewExfatReader(rs)
	if err := er.Parse(); err != nil {
		return nil, err
	}
	tree := exfat.NewTree(er)
	if err := tree.Load(); err != nil {
		return nil, err
	}
	root, err := tree.Lookup(nil)
	if err != nil {
		return nil, err
	}
	return &exfatFS{er: er, root: root}, nil
}

func (f *exfatFS) resolve(path string) (*exfat.TreeNode, error) {
	rel := strings.Trim(path, "/")
	node := f.root
	if rel == "" || rel == "." {
		return node, nil
	}
	for _, comp := range strings.Split(rel, "/") {
		if comp == "" || comp == "." {
			continue
		}
		child := node.GetChild(comp)
		if child == nil {
			return nil, fs.ErrNotExist
		}
		node = child
	}
	return node, nil
}

func exfatNodeSize(node *exfat.TreeNode) int64 {
	if sede := node.StreamDirectoryEntry(); sede != nil {
		return int64(sede.DataLength)
	}
	return 0
}

func (f *exfatFS) info(node *exfat.TreeNode, name string) fs.FileInfo {
	isDir := node.IsDirectory()
	mode := os.FileMode(0o644)
	if isDir {
		mode = os.ModeDir | 0o755
	}
	return &exfatFileInfo{name: name, size: exfatNodeSize(node), mode: mode}
}

// ReadDir lists the immediate children of a directory.
func (f *exfatFS) ReadDir(path string) ([]fs.DirEntry, error) {
	node, err := f.resolve(path)
	if err != nil {
		return nil, err
	}
	if !node.IsDirectory() {
		return nil, fmt.Errorf("exfat: %s is not a directory", path)
	}
	var out []fs.DirEntry
	for _, name := range node.ChildFolders() {
		out = append(out, &exfatDirEntry{name: name, isDir: true})
	}
	for _, name := range node.ChildFiles() {
		out = append(out, &exfatDirEntry{name: name, isDir: false})
	}
	return out, nil
}

// Stat returns metadata for path with lstat semantics.
func (f *exfatFS) Stat(path string) (fs.FileInfo, error) {
	node, err := f.resolve(path)
	if err != nil {
		return nil, err
	}
	return f.info(node, baseName(path)), nil
}

// Open opens a regular file for reading.
func (f *exfatFS) Open(path string) (fs.File, error) {
	node, err := f.resolve(path)
	if err != nil {
		return nil, err
	}
	if node.IsDirectory() {
		return nil, fmt.Errorf("exfat: %s is a directory", path)
	}
	sede := node.StreamDirectoryEntry()
	if sede == nil {
		return nil, fmt.Errorf("exfat: %s has no data stream", path)
	}
	var buf bytes.Buffer
	if _, _, err := f.er.WriteFromClusterChain(sede.FirstCluster, sede.DataLength, true, &buf); err != nil {
		return nil, err
	}
	return &exfatFile{Reader: bytes.NewReader(buf.Bytes()), fi: f.info(node, baseName(path))}, nil
}

// Readlink always fails: exFAT has no symlinks.
func (f *exfatFS) Readlink(path string) (string, error) {
	return "", fmt.Errorf("exfat: %s is not a symlink", path)
}

// Close releases no resources.
func (f *exfatFS) Close() error { return nil }

type exfatFileInfo struct {
	name string
	size int64
	mode os.FileMode
}

func (i *exfatFileInfo) Name() string       { return i.name }
func (i *exfatFileInfo) Size() int64        { return i.size }
func (i *exfatFileInfo) Mode() fs.FileMode  { return i.mode }
func (i *exfatFileInfo) IsDir() bool        { return i.mode.IsDir() }
func (i *exfatFileInfo) ModTime() time.Time { return time.Time{} }
func (i *exfatFileInfo) Sys() any           { return nil }

type exfatDirEntry struct {
	name  string
	isDir bool
}

func (d *exfatDirEntry) Name() string { return d.name }
func (d *exfatDirEntry) IsDir() bool  { return d.isDir }
func (d *exfatDirEntry) Type() fs.FileMode {
	if d.isDir {
		return fs.ModeDir
	}
	return 0
}
func (d *exfatDirEntry) Info() (fs.FileInfo, error) {
	mode := fs.FileMode(0o644)
	if d.isDir {
		mode = fs.ModeDir | 0o755
	}
	return &exfatFileInfo{name: d.name, mode: mode}, nil
}

type exfatFile struct {
	*bytes.Reader
	fi fs.FileInfo
}

func (f *exfatFile) Stat() (fs.FileInfo, error) { return f.fi, nil }
func (f *exfatFile) Close() error               { return nil }
