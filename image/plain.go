package image

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/ejfkdev/udf/fsutil"
	"github.com/ejfkdev/udf/fsview"
	appi18n "github.com/ejfkdev/udf/i18n"
	arch "github.com/ejfkdev/udf/image/archive"
)

// ClassifyInput returns the input class of path by content:
//
//	"disk"    — a virtual disk container or raw filesystem image
//	"image"   — a docker-save archive or OCI image layout (has manifest.json)
//	"archive" — a plain archive (tar/tar.gz/zip/7z/rar/cpio/asar/rpm)
//	""        — unsupported
//
// Unlike the docker-save path, a plain archive is a flat list of files with no
// layered rootfs, so it is listed and extracted through a single flat tree.
func ClassifyInput(path string) string {
	if arch.IsOCILayout(path) {
		return "image"
	}
	if c, err := detectDiskContainer(path); err == nil && c != "" {
		return "disk"
	}
	if a, err := arch.Detect(path); err == nil && a != "" {
		// Only a tar/tar.gz can be a docker-save archive; those carry a
		// top-level manifest.json, everything else is a plain archive.
		if a == "tar" || a == "tar.gz" {
			if ar, err := arch.Open(path); err == nil {
				if _, _, e := ar.Open("manifest.json"); e == nil {
					return "image"
				}
			}
		}
		return "archive"
	}
	if detectRawFilesystem(path) {
		return "disk"
	}
	return ""
}

// PlainInfo summarizes a plain archive for the info command.
type PlainInfo struct {
	Format    string `json:"format"`
	Files     int    `json:"files"`
	TotalSize int64  `json:"total_size"`
}

// PlainArchiveInfo builds a summary of a plain archive without extraction.
func PlainArchiveInfo(path string) (*PlainInfo, error) {
	ar, err := arch.Open(path)
	if err != nil {
		return nil, err
	}
	entries, err := ar.List()
	if err != nil {
		return nil, err
	}
	format, _ := arch.Detect(path)
	info := &PlainInfo{Format: format}
	for _, e := range entries {
		if e.Kind == fsview.KindFile {
			info.Files++
			info.TotalSize += e.Size
		}
	}
	return info, nil
}

// buildPlainTree opens a plain archive and returns its entries as a flat fsview
// tree. Each node's Layer and EntryName hold the entry's raw name so later
// arch.Open(...).Open can stream it; Path is the normalized slash-separated
// path used for traversal and resolution.
func buildPlainTree(archivePath string) (*fsview.Node, error) {
	ar, err := arch.Open(archivePath)
	if err != nil {
		return nil, err
	}
	entries, err := ar.List()
	if err != nil {
		return nil, err
	}

	root := &fsview.Node{Kind: fsview.KindDir, Mode: 0o755}
	for _, e := range entries {
		clean, err := fsview.CleanEntryName(e.Name)
		if err != nil || clean == "" {
			continue
		}
		parent := ensurePlainDir(root, path.Dir(clean))
		base := path.Base(clean)
		insertPlain(parent, &fsview.Node{
			Name:      base,
			Path:      joinPlain(parent.Path, base),
			EntryName: e.Name,
			Kind:      e.Kind,
			Size:      e.Size,
			Mode:      e.Mode,
			ModTime:   e.ModTime,
			UID:       e.UID,
			GID:       e.GID,
			Uname:     e.Uname,
			Gname:     e.Gname,
			Linkname:  e.Linkname,
			Layer:     e.Name,
		})
	}
	return root, nil
}

// ListPlainArchive lists the directory (or single entry) at treePath inside a
// plain archive, without extracting.
func ListPlainArchive(archivePath, treePath string) ([]FileEntry, error) {
	tree, err := buildPlainTree(archivePath)
	if err != nil {
		return nil, err
	}
	return ListEntries(tree, treePath)
}

// ReadPlainArchiveFile opens one regular file inside a plain archive, following
// symlink and hardlink nodes to the file that holds the content.
func ReadPlainArchiveFile(archivePath, sourcePath string) (io.ReadCloser, int64, error) {
	tree, err := buildPlainTree(archivePath)
	if err != nil {
		return nil, 0, err
	}
	srcRel, err := fsview.NormalizePath(sourcePath)
	if err != nil {
		return nil, 0, err
	}
	srcNode, err := resolveContentNode(tree, tree.Resolve(srcRel))
	if err != nil {
		return nil, 0, err
	}
	if srcNode == nil {
		return nil, 0, appi18n.NewError("err_cp_src_not_found", map[string]any{"Path": sourcePath}, nil)
	}

	ar, err := arch.Open(archivePath)
	if err != nil {
		return nil, 0, err
	}
	rc, size, err := ar.Open(srcNode.Layer)
	if err != nil {
		return nil, 0, fmt.Errorf("open %s: %w", srcNode.Layer, err)
	}
	return rc, size, nil
}

// ExtractPlainArchive extracts the entire plain archive into destDir, writing
// files, directories and symlinks and returning the number of entries written.
func ExtractPlainArchive(archivePath, destDir string, bufferSize int) (int, error) {
	tree, err := buildPlainTree(archivePath)
	if err != nil {
		return 0, err
	}
	ar, err := arch.Open(archivePath)
	if err != nil {
		return 0, err
	}
	buf := make([]byte, bufferSize)
	var dirs []fsutil.DirMetadata
	count := 0
	if err := materializeNode(ar, tree, tree, destDir, &dirs, buf, &count); err != nil {
		return count, err
	}
	if err := fsutil.ApplyDirMetadata(dirs); err != nil {
		return count, err
	}
	return count, nil
}

// ExtractPlainPath copies a single file or directory from a plain archive to a
// local destination, mirroring cp semantics: a directory copied into an
// existing directory lands at dest/<basename>. Returns the count of entries
// written.
func ExtractPlainPath(archivePath, source, dest string, bufferSize int) (int, error) {
	tree, err := buildPlainTree(archivePath)
	if err != nil {
		return 0, err
	}
	srcRel, err := fsview.NormalizePath(source)
	if err != nil {
		return 0, err
	}
	srcNode := tree.Resolve(srcRel)
	if srcNode == nil {
		return 0, appi18n.NewError("err_cp_src_not_found", map[string]any{"Path": source}, nil)
	}

	ar, err := arch.Open(archivePath)
	if err != nil {
		return 0, err
	}

	destTarget := dest
	if srcNode.Kind == fsview.KindDir {
		info, statErr := os.Stat(dest)
		switch {
		case statErr == nil && info.IsDir():
			if srcRel != "" {
				destTarget = filepath.Join(dest, filepath.Base(srcRel))
			}
		case statErr == nil:
			return 0, appi18n.NewError("err_cp_dest_conflict", map[string]any{"Path": dest}, nil)
		case os.IsNotExist(statErr):
			destTarget = dest
		default:
			return 0, statErr
		}
	} else {
		info, statErr := os.Stat(dest)
		switch {
		case statErr == nil && info.IsDir():
			destTarget = filepath.Join(dest, filepath.Base(srcRel))
		case statErr == nil:
			destTarget = dest
		case os.IsNotExist(statErr):
			destTarget = dest
		default:
			return 0, statErr
		}
	}

	buf := make([]byte, bufferSize)
	var dirs []fsutil.DirMetadata
	count := 0
	if err := materializeNode(ar, tree, srcNode, destTarget, &dirs, buf, &count); err != nil {
		return count, err
	}
	if err := fsutil.ApplyDirMetadata(dirs); err != nil {
		return count, err
	}
	return count, nil
}

// materializeNode writes one node (and, for a directory, its subtree) at
// target. Symlinks are preserved as links; hardlinks are resolved and their
// content copied.
func materializeNode(ar arch.Archive, tree, node *fsview.Node, target string, dirs *[]fsutil.DirMetadata, buf []byte, count *int) error {
	switch node.Kind {
	case fsview.KindDir:
		if err := os.MkdirAll(target, os.FileMode(node.Mode&0o7777).Perm()); err != nil {
			return fmt.Errorf("create directory: %w", err)
		}
		*dirs = append(*dirs, fsutil.DirMetadata{Path: target, Mode: os.FileMode(node.Mode & 0o7777), ModTime: node.ModTime})
		for _, c := range node.SortedChildren() {
			if err := materializeNode(ar, tree, c, filepath.Join(target, filepath.FromSlash(c.Name)), dirs, buf, count); err != nil {
				return err
			}
		}
	case fsview.KindFile:
		if err := materializeFile(ar, node, target, buf); err != nil {
			return err
		}
	case fsview.KindSymlink:
		if err := fsutil.EnsureParentDir(target); err != nil {
			return err
		}
		if err := os.RemoveAll(target); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := os.Symlink(node.Linkname, target); err != nil {
			return fmt.Errorf("create symlink: %w", err)
		}
	case fsview.KindHardlink:
		src := resolveHardlinkFinal(tree, node)
		if src == nil || src == node {
			return fmt.Errorf("unresolved hardlink: %s", node.Path)
		}
		if src.Kind == fsview.KindSymlink {
			if err := fsutil.EnsureParentDir(target); err != nil {
				return err
			}
			if err := os.RemoveAll(target); err != nil && !os.IsNotExist(err) {
				return err
			}
			if err := os.Symlink(src.Linkname, target); err != nil {
				return fmt.Errorf("create symlink: %w", err)
			}
		} else if err := materializeFile(ar, src, target, buf); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported node kind for %s", node.Path)
	}
	*count = *count + 1
	return nil
}

func materializeFile(ar arch.Archive, node *fsview.Node, target string, buf []byte) error {
	rc, _, err := ar.Open(node.Layer)
	if err != nil {
		return fmt.Errorf("open %s: %w", node.Layer, err)
	}
	defer rc.Close()

	if err := fsutil.EnsureParentDir(target); err != nil {
		return err
	}
	if err := os.RemoveAll(target); err != nil && !os.IsNotExist(err) {
		return err
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(node.Mode&0o7777).Perm())
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	if _, err := io.CopyBuffer(out, rc, buf); err != nil {
		_ = out.Close()
		return fmt.Errorf("write file: %w", err)
	}
	return out.Close()
}

func resolveHardlinkFinal(tree *fsview.Node, node *fsview.Node) *fsview.Node {
	seen := make(map[*fsview.Node]bool)
	for node != nil && node.Kind == fsview.KindHardlink {
		if seen[node] {
			return nil
		}
		seen[node] = true
		node = tree.Resolve(node.Linkname)
	}
	return node
}

// ensurePlainDir mirrors fsview's layer merge for a flat archive: it creates
// missing directory components, replacing a non-directory that blocks the way.
func ensurePlainDir(root *fsview.Node, rel string) *fsview.Node {
	cur := root
	if rel == "." || rel == "" {
		return cur
	}
	for _, comp := range strings.Split(rel, "/") {
		next := plainChild(cur, comp)
		if next == nil {
			next = &fsview.Node{Name: comp, Path: joinPlain(cur.Path, comp), Kind: fsview.KindDir, Mode: 0o755}
			cur.Children = append(cur.Children, next)
		} else if next.Kind != fsview.KindDir {
			*next = fsview.Node{Name: comp, Path: joinPlain(cur.Path, comp), Kind: fsview.KindDir, Mode: 0o755}
		}
		cur = next
	}
	return cur
}

func plainChild(n *fsview.Node, name string) *fsview.Node {
	for _, c := range n.Children {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func insertPlain(p *fsview.Node, node *fsview.Node) {
	for i, c := range p.Children {
		if c.Name != node.Name {
			continue
		}
		if node.Kind == fsview.KindDir && c.Kind == fsview.KindDir {
			c.Size = node.Size
			c.Mode = node.Mode
			c.ModTime = node.ModTime
			c.UID = node.UID
			c.GID = node.GID
			c.Uname = node.Uname
			c.Gname = node.Gname
			c.EntryName = node.EntryName
			c.Layer = node.Layer
			return
		}
		p.Children[i] = node
		return
	}
	p.Children = append(p.Children, node)
}

func joinPlain(parent, name string) string {
	if parent == "" {
		return name
	}
	return parent + "/" + name
}
