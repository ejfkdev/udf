package image

import (
	"fmt"
	"io"
	"os"

	"github.com/ejfkdev/udf/fsutil"
	"github.com/ejfkdev/udf/fsview"
	arch "github.com/ejfkdev/udf/image/archive"
	"github.com/ejfkdev/udf/layer"
	"github.com/ejfkdev/udf/types"
)

type ProgressReporter interface {
	SetLayer(string)
	AddLayer()
	MarkDone()
}

func PrepareOutputDir(outputDir string, force bool) error {
	info, err := os.Stat(outputDir)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("output path exists and is not a directory: %s", outputDir)
		}

		entries, err := os.ReadDir(outputDir)
		if err != nil {
			return fmt.Errorf("read output directory: %w", err)
		}
		if len(entries) > 0 {
			if !force {
				return fmt.Errorf("output directory must be empty: %s", outputDir)
			}
		}
		return nil
	}

	if !os.IsNotExist(err) {
		return err
	}

	return os.MkdirAll(outputDir, 0o755)
}

// ApplyImage extracts the image's merged rootfs into outputDir. When the
// archive supports sequential reading, extraction happens in a single pass:
// the merged tree knows which layer defines each path, so only the winning
// entry of each path is written (once) rather than replaying every layer
// wholesale, and a compressed archive is decompressed once instead of once
// per layer.
func ApplyImage(imageTarPath string, meta *types.ImageMetadata, outputDir string, bufferSize int, progress ProgressReporter) error {
	if done, err := applyImageMerged(imageTarPath, meta, outputDir, bufferSize); done {
		return err
	}
	return applyImagePerLayer(imageTarPath, meta, outputDir, bufferSize, progress)
}

// applyImageMerged runs the single-pass extraction; done=false means the
// archive has no sequential reader and the caller should use the per-layer
// path instead.
func applyImageMerged(imageTarPath string, meta *types.ImageMetadata, outputDir string, bufferSize int) (bool, error) {
	archive, err := arch.Open(imageTarPath)
	if err != nil {
		return false, err
	}
	if arch.SequentialOf(archive) == nil {
		return false, nil
	}

	tree, err := BuildFileSystem(imageTarPath, meta)
	if err != nil {
		return true, err
	}

	plan := &extractPlan{
		tree:     tree,
		archive:  archive,
		destRoot: outputDir,
		selected: make(map[*fsview.Node]string),
		byLayer:  make(map[string]map[string]*fsview.Node),
	}
	plan.collectDir(tree, "")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return true, fmt.Errorf("create output directory %s: %w", outputDir, err)
	}

	buf := make([]byte, bufferSize)
	var dirs []fsutil.DirMetadata
	var count int

	// Layers are read through the index when available: each layer decodes
	// only its own span instead of a full pass over the archive.
	indexed := false
	if ir, ok := ensureImageIndex(imageTarPath); ok {
		indexed = true
		for _, layerName := range meta.LayerOrder {
			src, err := openIndexedLayer(ir, layerName)
			if err != nil {
				_ = ir.Close()
				return true, err
			}
			err = applyLayerEntries(plan, layerName, src, buf, &dirs, &count)
			_ = src.Close()
			if err != nil {
				_ = ir.Close()
				return true, err
			}
		}
		_ = ir.Close()
	}
	if !indexed {
		if _, err := extractSelectionSeq(plan, archive, buf, &dirs, &count); err != nil {
			return true, err
		}
	}
	if err := plan.resolveDeferredLinks(buf); err != nil {
		return true, err
	}
	if err := fsutil.ApplyDirMetadata(dirs); err != nil {
		return true, fmt.Errorf("apply final directory metadata: %w", err)
	}
	return true, nil
}

// applyImagePerLayer replays every layer over the output directory, in
// manifest order. It is the fallback for archives without a sequential
// reader (OCI layouts, for instance).
func applyImagePerLayer(imageTarPath string, meta *types.ImageMetadata, outputDir string, bufferSize int, progress ProgressReporter) error {
	archive, err := arch.Open(imageTarPath)
	if err != nil {
		return err
	}

	stream := newLayerStreamSource(archive)
	if stream != nil {
		defer stream.Close()
	}

	buf := make([]byte, bufferSize)
	dirState := make(map[string]fsutil.DirMetadata)

	for _, layerName := range meta.LayerOrder {
		if progress != nil {
			progress.SetLayer(layerName)
		}
		var rc io.Reader
		var closeFn func()
		if stream != nil {
			rc, closeFn, err = stream.OpenRaw(layerName)
		} else {
			var raw io.ReadCloser
			raw, _, err = archive.Open(layerName)
			if err != nil {
				return layerOpenError(meta, layerName, err)
			}
			if err == nil {
				rc, closeFn = raw, func() { _ = raw.Close() }
			}
		}
		if err != nil {
			return fmt.Errorf("open layer %s: %w", layerName, err)
		}
		dirs, err := layer.ApplyLayer(rc, outputDir, buf)
		if err != nil {
			closeFn()
			return fmt.Errorf("apply layer %s: %w", layerName, err)
		}
		closeFn()
		for _, dir := range dirs {
			dirState[dir.Path] = dir
		}
		if progress != nil {
			progress.AddLayer()
		}
	}

	if len(dirState) > 0 {
		finalDirs := make([]fsutil.DirMetadata, 0, len(dirState))
		for _, dir := range dirState {
			finalDirs = append(finalDirs, dir)
		}
		if err := fsutil.ApplyDirMetadata(finalDirs); err != nil {
			return fmt.Errorf("apply final directory metadata: %w", err)
		}
	}

	return nil
}
