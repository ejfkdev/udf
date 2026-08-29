package image

import (
	"archive/tar"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/ejfkdev/udf/fsview"
	appi18n "github.com/ejfkdev/udf/i18n"
	arch "github.com/ejfkdev/udf/image/archive"
	"github.com/ejfkdev/udf/layer"
	"github.com/ejfkdev/udf/types"
)

// closeReadCloser adapts any io.Reader to io.ReadCloser, releasing the
// underlying resources (layer, volume and image handles) on Close.
type closeReadCloser struct {
	io.Reader
	closeFn func() error
}

func (r *closeReadCloser) Close() error { return r.closeFn() }

// ReadDiskFile opens the content of a single regular file inside a disk image
// (qcow2/vmdk/vhd/vhdx/ova/...) at virtual path vp. The returned reader must
// be closed to release the image, its volumes and the opened file; size is the
// file length in bytes. Symlinks are not followed; use a filesystem path.
func ReadDiskFile(path, vp string) (io.ReadCloser, int64, error) {
	img, err := openDiskImage(path)
	if err != nil {
		return nil, 0, err
	}

	t, err := img.resolveVirtual(vp)
	if err != nil {
		_ = img.Close()
		return nil, 0, err
	}
	if t.level != "fs" {
		_ = img.Close()
		return nil, 0, fmt.Errorf("path %q does not point into a filesystem; list the image first to see its disks and volumes", vp)
	}

	reader, closeVol, err := img.openVolumeReader(t.diskIdx, t.vol)
	if err != nil {
		_ = img.Close()
		return nil, 0, err
	}

	e, ok, err := statDiskEntry(reader, t.rel)
	if err != nil {
		_ = closeVol()
		_ = img.Close()
		return nil, 0, err
	}
	if !ok {
		_ = closeVol()
		_ = img.Close()
		return nil, 0, appi18n.NewError("err_cp_src_not_found", map[string]any{"Path": vp}, nil)
	}
	if e.Kind != fsview.KindFile {
		_ = closeVol()
		_ = img.Close()
		return nil, 0, fmt.Errorf("%s is not a regular file", vp)
	}

	f, err := reader.Open(fsPathFor(t.rel))
	if err != nil {
		_ = closeVol()
		_ = img.Close()
		return nil, 0, err
	}

	return &closeReadCloser{
		Reader: f,
		closeFn: func() error {
			_ = f.Close()
			_ = closeVol()
			return img.Close()
		},
	}, e.Size, nil
}

// ReadArchiveFile opens the content of a single regular file inside an archive
// image (tar/tar.gz/tgz/zip/7z/cpio/rar), following hardlink and symlink nodes
// to the regular file they point at. The returned reader must be closed to
// release the opened layer; size is the file length in bytes.
func ReadArchiveFile(path string, meta *types.ImageMetadata, sourcePath string) (io.ReadCloser, int64, error) {
	tree, err := BuildFileSystem(path, meta)
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

	archive, err := arch.Open(path)
	if err != nil {
		return nil, 0, err
	}

	rc, _, err := archive.Open(srcNode.Layer)
	if err != nil {
		return nil, 0, fmt.Errorf("open layer %s: %w", srcNode.Layer, err)
	}
	r, closeLayer, err := layer.OpenLayerReader(rc)
	if err != nil {
		_ = rc.Close()
		return nil, 0, fmt.Errorf("open layer %s: %w", srcNode.Layer, err)
	}

	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			closeLayer()
			_ = rc.Close()
			return nil, 0, fmt.Errorf("read layer %s: %w", srcNode.Layer, err)
		}
		if hdr.Name != srcNode.EntryName {
			continue
		}
		return &closeReadCloser{
			Reader: io.LimitReader(tr, hdr.Size),
			closeFn: func() error {
				closeLayer()
				return rc.Close()
			},
		}, hdr.Size, nil
	}

	closeLayer()
	_ = rc.Close()
	return nil, 0, fmt.Errorf("entry %s not found in layer %s", srcNode.EntryName, srcNode.Layer)
}

// resolveContentNode follows hardlink and symlink nodes until reaching the
// regular file that actually holds the content, guarding against link cycles.
// A nil node in returns a nil node so the caller can report "not found".
func resolveContentNode(tree *fsview.Node, node *fsview.Node) (*fsview.Node, error) {
	if node == nil {
		return nil, nil
	}
	seen := make(map[*fsview.Node]bool)
	for {
		if seen[node] {
			return nil, fmt.Errorf("link cycle detected at %s", node.Path)
		}
		seen[node] = true

		switch node.Kind {
		case fsview.KindFile:
			return node, nil
		case fsview.KindHardlink:
			from, target := node.Path, node.Linkname
			node = tree.Resolve(target)
			if node == nil {
				return nil, fmt.Errorf("hardlink target not found: %s -> %s", from, target)
			}
		case fsview.KindSymlink:
			// A POSIX symlink target is absolute, or relative to the link's
			// directory. Resolve treats a leading slash as the tree root, so
			// absolute targets are passed through as-is.
			from, target := node.Path, node.Linkname
			if !strings.HasPrefix(target, "/") {
				target = path.Join(path.Dir(from), target)
			}
			node = tree.Resolve(target)
			if node == nil {
				return nil, fmt.Errorf("symlink target not found: %s -> %s", from, target)
			}
		default:
			return nil, fmt.Errorf("%s is not a regular file", node.Path)
		}
	}
}
