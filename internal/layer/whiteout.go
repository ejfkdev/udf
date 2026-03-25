package layer

import (
	"fmt"
	"os"
	"path"
	"strings"

	"udf/internal/fsutil"
)

const opaqueWhiteout = ".wh..wh..opq"

func HandleWhiteout(outputDir, entryName string) (bool, error) {
	base := path.Base(entryName)
	if !strings.HasPrefix(base, ".wh.") {
		return false, nil
	}

	dirName := path.Dir(entryName)
	if dirName == "." {
		dirName = ""
	}

	if base == opaqueWhiteout {
		targetDir, err := fsutil.ResolveSafePath(outputDir, dirName)
		if err != nil {
			return true, err
		}
		return true, clearDirectory(targetDir)
	}

	targetName := strings.TrimPrefix(base, ".wh.")
	targetPath, err := fsutil.ResolveSafePath(outputDir, path.Join(dirName, targetName))
	if err != nil {
		return true, err
	}
	if err := os.RemoveAll(targetPath); err != nil && !os.IsNotExist(err) {
		return true, fmt.Errorf("remove whiteout target %s: %w", targetPath, err)
	}
	return true, nil
}

func clearDirectory(targetDir string) error {
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return err
	}

	entries, err := os.ReadDir(targetDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := os.RemoveAll(path.Join(targetDir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}
