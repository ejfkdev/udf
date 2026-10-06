package image

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"

	"github.com/ejfkdev/udf/fsutil"
	"github.com/ejfkdev/udf/fsview"
	appi18n "github.com/ejfkdev/udf/i18n"
	arch "github.com/ejfkdev/udf/image/archive"
	"github.com/ejfkdev/udf/layer"
	"github.com/ejfkdev/udf/types"
)

// ExtractPath copies a single file or directory from the merged image
// filesystem to destPath, streaming only the entries that belong to the
// selection instead of extracting the whole image.
//
// Semantics mirror cp: a directory source copied into an existing directory
// lands at destPath/<basename>, while a non-existent destination receives the
// copy directly. Extracting the image root ("/" or ".") puts its contents
// into destPath itself. Returns the number of entries written.
func ExtractPath(imageTarPath string, meta *types.ImageMetadata, sourcePath, destPath string, bufferSize int) (int, error) {
	tree, err := BuildFileSystem(imageTarPath, meta)
	if err != nil {
		return 0, err
	}

	srcRel, err := fsview.NormalizePath(sourcePath)
	if err != nil {
		return 0, err
	}
	srcNode := tree.Resolve(srcRel)
	if srcNode == nil {
		return 0, appi18n.NewError("err_cp_src_not_found", map[string]any{"Path": sourcePath}, nil)
	}

	archive, err := arch.Open(imageTarPath)
	if err != nil {
		return 0, err
	}

	plan, err := makeExtractPlan(tree, meta, archive, srcNode, srcRel, destPath)
	if err != nil {
		return 0, err
	}

	if !plan.singleFile {
		if err := os.MkdirAll(plan.destRoot, 0o755); err != nil {
			return 0, fmt.Errorf("create destination directory %s: %w", plan.destRoot, err)
		}
	}

	buf := make([]byte, bufferSize)
	var dirs []fsutil.DirMetadata
	var count int

	// With an index each layer decodes on its own, so the selection is read
	// layer by layer; without one the entries are written in the archive's
	// physical order during a single sequential pass (manifest order rarely
	// matches archive order, and every random access would then cost a full
	// decompression).
	handled := false
	if ir, ok := ensureImageIndex(imageTarPath); ok {
		handled = true
		for _, layerName := range meta.LayerOrder {
			if len(plan.byLayer[layerName]) == 0 {
				continue
			}
			src, err := openIndexedLayer(ir, layerName)
			if err != nil {
				_ = ir.Close()
				return count, err
			}
			err = applyLayerEntries(plan, layerName, src, buf, &dirs, &count)
			_ = src.Close()
			if err != nil {
				_ = ir.Close()
				return count, err
			}
		}
		_ = ir.Close()
	}
	if !handled {
		if done, err := extractSelectionSeq(plan, archive, buf, &dirs, &count); err != nil {
			return count, err
		} else if !done {
			stream := newLayerStreamSource(archive)
			if stream != nil {
				defer stream.Close()
			}
			for _, layerName := range meta.LayerOrder {
				if len(plan.byLayer[layerName]) == 0 {
					continue
				}
				if err := extractLayerEntries(plan, stream, layerName, buf, &dirs, &count); err != nil {
					return count, err
				}
			}
		}
	}

	if err := plan.resolveDeferredLinks(buf); err != nil {
		return count, err
	}

	if err := fsutil.ApplyDirMetadata(dirs); err != nil {
		return count, fmt.Errorf("apply directory metadata: %w", err)
	}
	return count, nil
}

// extractSelectionSeq writes the whole selection during one sequential pass
// over the archive. It reports done=false when the archive has no sequential
// reader, leaving the caller to use per-layer opens.
func extractSelectionSeq(plan *extractPlan, archive arch.Archive, buf []byte, dirs *[]fsutil.DirMetadata, count *int) (bool, error) {
	seq := arch.SequentialOf(archive)
	if seq == nil {
		return false, nil
	}
	defer seq.Close()

	wanted := make(map[string]bool, len(plan.byLayer))
	for layerName, nodes := range plan.byLayer {
		if len(nodes) > 0 {
			wanted[layerName] = true
		}
	}

	for {
		name, _, r, err := seq.Next()
		if err != nil {
			break
		}
		if !wanted[name] {
			continue
		}
		if err := applyLayerEntries(plan, name, r, buf, dirs, count); err != nil {
			return true, err
		}
		delete(wanted, name)
		if len(wanted) == 0 {
			// Everything selected has been written; the rest of the stream
			// (often most of it) never needs decompressing.
			return true, nil
		}
	}
	if len(wanted) > 0 {
		missing := make([]string, 0, len(wanted))
		for name := range wanted {
			missing = append(missing, name)
		}
		sort.Strings(missing)
		return true, fmt.Errorf("layers not found in archive: %v", missing)
	}
	return true, nil
}

type extractPlan struct {
	tree       *fsview.Node
	archive    arch.Archive
	meta       *types.ImageMetadata
	destRoot   string
	singleFile bool
	fileTarget string
	selected   map[*fsview.Node]string            // node -> path relative to destRoot
	byLayer    map[string]map[string]*fsview.Node // layer -> clean entry path
	deferred   []deferredLink                     // hardlinks waiting for their source to land
}

func makeExtractPlan(tree *fsview.Node, meta *types.ImageMetadata, archive arch.Archive, srcNode *fsview.Node, srcRel, destPath string) (*extractPlan, error) {
	plan := &extractPlan{
		tree:     tree,
		meta:     meta,
		archive:  archive,
		selected: make(map[*fsview.Node]string),
		byLayer:  make(map[string]map[string]*fsview.Node),
	}

	// 提取镜像根：内容直接放入 destPath
	if srcNode.Kind == fsview.KindDir && srcRel == "" {
		plan.destRoot = destPath
		plan.collectDir(srcNode, "")
		return plan, nil
	}

	if srcNode.Kind == fsview.KindDir {
		info, err := os.Stat(destPath)
		switch {
		case err == nil && info.IsDir():
			plan.destRoot = filepath.Join(destPath, path.Base(srcRel))
		case err == nil:
			return nil, appi18n.NewError("err_cp_dest_conflict", map[string]any{"Path": destPath}, nil)
		case os.IsNotExist(err):
			plan.destRoot = destPath
		case err != nil:
			return nil, err
		}
		plan.collectDir(srcNode, "")
		return plan, nil
	}

	// 单个文件/符号链接/硬链接
	plan.singleFile = true
	info, err := os.Stat(destPath)
	switch {
	case err == nil && info.IsDir():
		plan.fileTarget = filepath.Join(destPath, path.Base(srcRel))
	case err == nil:
		plan.fileTarget = destPath
	case os.IsNotExist(err):
		plan.fileTarget = destPath
	default:
		return nil, err
	}
	plan.selected[srcNode] = ""
	plan.addToLayerIndex(srcNode)
	return plan, nil
}

func (p *extractPlan) collectDir(node *fsview.Node, rel string) {
	p.selected[node] = rel
	p.addToLayerIndex(node)
	for _, child := range node.Children {
		p.collectDir(child, path.Join(rel, child.Name))
	}
}

func (p *extractPlan) addToLayerIndex(node *fsview.Node) {
	if node.Layer == "" {
		return
	}
	byPath, ok := p.byLayer[node.Layer]
	if !ok {
		byPath = make(map[string]*fsview.Node)
		p.byLayer[node.Layer] = byPath
	}
	byPath[node.Path] = node
}

func (p *extractPlan) inSelection(node *fsview.Node) bool {
	_, ok := p.selected[node]
	return ok
}

// extractLayerEntries streams one layer once, applying the tar entries that
// define nodes of the selection. The layer bytes come from the shared
// sequential source when available, otherwise from a random-access open.
func extractLayerEntries(plan *extractPlan, stream *layerStreamSource, layerName string, buf []byte, dirs *[]fsutil.DirMetadata, count *int) error {
	var raw io.Reader
	var closeFn func()
	if stream != nil {
		var err error
		raw, closeFn, err = stream.OpenRaw(layerName)
		if err != nil {
			return err
		}
	} else {
		rc, _, err := plan.archive.Open(layerName)
		if err != nil {
			return layerOpenError(plan.meta, layerName, err)
		}
		raw, closeFn = rc, func() { _ = rc.Close() }
	}
	defer closeFn()

	return applyLayerEntries(plan, layerName, raw, buf, dirs, count)
}

// applyLayerEntries applies the selected entries of one layer, reading the
// layer's stored bytes from r (compression is handled here).
func applyLayerEntries(plan *extractPlan, layerName string, raw io.Reader, buf []byte, dirs *[]fsutil.DirMetadata, count *int) error {
	r, closeFn, err := layer.OpenLayerReader(raw)
	if err != nil {
		return fmt.Errorf("open layer %s: %w", layerName, err)
	}
	defer closeFn()

	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read layer %s: %w", layerName, err)
		}
		clean, err := fsview.CleanEntryName(hdr.Name)
		if err != nil {
			return err
		}
		node := plan.byLayer[layerName][clean]
		if node == nil {
			continue
		}
		if fsview.IsDeviceKind(node.Kind) {
			// 设备节点/FIFO 无法在非特权环境重建，跳过且不计入提取数。
			continue
		}
		if err := applyNode(plan, node, hdr, tr, buf, dirs); err != nil {
			return fmt.Errorf("extract %s from layer %s: %w", hdr.Name, layerName, err)
		}
		*count++
	}
}

func applyNode(plan *extractPlan, node *fsview.Node, hdr *tar.Header, r io.Reader, buf []byte, dirs *[]fsutil.DirMetadata) error {
	target, err := plan.targetFor(node)
	if err != nil {
		return err
	}

	switch node.Kind {
	case fsview.KindDir:
		if err := fsutil.ReplaceWithDir(target, os.FileMode(hdr.Mode).Perm()); err != nil {
			return fmt.Errorf("create directory: %w", err)
		}
		*dirs = append(*dirs, fsutil.DirMetadata{
			Path:    target,
			Mode:    os.FileMode(hdr.Mode),
			ModTime: hdr.ModTime,
		})
	case fsview.KindFile:
		if err := fsutil.ReplaceWithFile(target, hdr, r, buf); err != nil {
			return fmt.Errorf("write file: %w", err)
		}
	case fsview.KindSymlink:
		if err := fsutil.ReplaceWithSymlink(target, hdr); err != nil {
			return fmt.Errorf("create symlink: %w", err)
		}
	case fsview.KindHardlink:
		if err := plan.applyHardlink(node, target, hdr, buf); err != nil {
			return err
		}
	}
	return nil
}

func (p *extractPlan) targetFor(node *fsview.Node) (string, error) {
	if p.singleFile {
		return p.fileTarget, nil
	}
	return fsutil.ResolveSafePath(p.destRoot, p.selected[node])
}

func (p *extractPlan) applyHardlink(node *fsview.Node, target string, hdr *tar.Header, buf []byte) error {
	src, err := p.resolveHardlinkSource(node)
	if err != nil {
		return err
	}

	// 源在选择集内时直接建链，源目标位置由 selected 映射得出；失败或源在
	// 选择集外时降级为复制源内容。
	if !p.singleFile && p.inSelection(src) {
		srcTarget, err := fsutil.ResolveSafePath(p.destRoot, p.selected[src])
		if err != nil {
			return err
		}
		if err := linkTo(srcTarget, target); err == nil {
			return nil
		} else if _, statErr := os.Lstat(srcTarget); os.IsNotExist(statErr) {
			// 条目按归档物理顺序写出，源可能还没落盘：延后到整轮结束后补链，
			// 避免为此做一次昂贵的随机访问拷贝。
			p.deferred = append(p.deferred, deferredLink{src: src, node: node, target: target})
			return nil
		}
	}
	return p.copySourceContent(src, target, buf)
}

// deferredLink is a hardlink whose in-selection source was not yet written
// when the entry was processed (archive order differs from layer order).
type deferredLink struct {
	src    *fsview.Node
	node   *fsview.Node
	target string
}

// resolveDeferredLinks links deferred hardlinks now that every selected path
// exists on disk, falling back to a content copy when linking fails.
func (p *extractPlan) resolveDeferredLinks(buf []byte) error {
	for _, d := range p.deferred {
		srcTarget, err := fsutil.ResolveSafePath(p.destRoot, p.selected[d.src])
		if err != nil {
			return err
		}
		if err := linkTo(srcTarget, d.target); err == nil {
			continue
		}
		if err := p.copySourceContent(d.src, d.target, buf); err != nil {
			return fmt.Errorf("resolve hardlink %s: %w", d.node.Path, err)
		}
	}
	p.deferred = nil
	return nil
}

// linkTo creates a hardlink at target pointing to srcTarget, with semantics
// matching fsutil.ReplaceWithHardlink but with an explicitly resolved source.
func linkTo(srcTarget, target string) error {
	if err := fsutil.EnsureParentDir(target); err != nil {
		return err
	}
	if err := os.RemoveAll(target); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Link(srcTarget, target)
}

func (p *extractPlan) resolveHardlinkSource(node *fsview.Node) (*fsview.Node, error) {
	cur := node
	seen := make(map[*fsview.Node]bool)
	for cur.Kind == fsview.KindHardlink {
		if seen[cur] {
			return nil, fmt.Errorf("hardlink cycle detected at %s", node.Path)
		}
		seen[cur] = true
		next := p.tree.Resolve(cur.Linkname)
		if next == nil {
			return nil, fmt.Errorf("hardlink source not found: %s -> %s", cur.Path, cur.Linkname)
		}
		cur = next
	}
	if cur.Kind == fsview.KindDir {
		return nil, fmt.Errorf("hardlink source is a directory: %s", node.Path)
	}
	return cur, nil
}

// copySourceContent streams the source node's content out of its defining
// layer, following the resolved chain to the final regular file or symlink.
func (p *extractPlan) copySourceContent(src *fsview.Node, target string, buf []byte) error {
	rc, _, err := p.archive.Open(src.Layer)
	if err != nil {
		return fmt.Errorf("open source layer %s: %w", src.Layer, err)
	}
	r, closeFn, err := layer.OpenLayerReader(rc)
	if err != nil {
		_ = rc.Close()
		return err
	}
	defer closeFn()

	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("source entry %s not found in layer %s", src.EntryName, src.Layer)
		}
		if err != nil {
			return err
		}
		if hdr.Name != src.EntryName {
			continue
		}
		switch src.Kind {
		case fsview.KindFile:
			return fsutil.ReplaceWithFile(target, hdr, tr, buf)
		case fsview.KindSymlink:
			return fsutil.ReplaceWithSymlink(target, hdr)
		}
		return fmt.Errorf("unsupported hardlink source kind for %s", src.Path)
	}
}
