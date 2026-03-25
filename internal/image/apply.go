package image

import (
	"fmt"
	"os"

	"udf/internal/fsutil"
	"udf/internal/layer"
	"udf/internal/types"
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

func ApplyImage(imageTarPath string, meta *types.ImageMetadata, outputDir string, bufferSize int, progress ProgressReporter) error {
	archive, err := openArchive(imageTarPath)
	if err != nil {
		return err
	}

	buf := make([]byte, bufferSize)
	dirState := make(map[string]fsutil.DirMetadata)

	for _, layerName := range meta.LayerOrder {
		if progress != nil {
			progress.SetLayer(layerName)
		}
		rc, _, err := archive.Open(layerName)
		if err != nil {
			return fmt.Errorf("open layer %s: %w", layerName, err)
		}
		dirs, err := layer.ApplyLayer(rc, outputDir, buf)
		if err != nil {
			_ = rc.Close()
			return fmt.Errorf("apply layer %s: %w", layerName, err)
		}
		if err := rc.Close(); err != nil {
			return fmt.Errorf("close layer %s: %w", layerName, err)
		}
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
