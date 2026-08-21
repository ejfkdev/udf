package fsutil

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

func ResolveSafePath(root, name string) (string, error) {
	cleanName := path.Clean(strings.TrimPrefix(name, "/"))
	if cleanName == "." {
		return root, nil
	}

	if strings.HasPrefix(cleanName, "../") || cleanName == ".." {
		return "", fmt.Errorf("path escapes root: %s", name)
	}

	target := filepath.Join(root, filepath.FromSlash(cleanName))
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return "", fmt.Errorf("resolve path %s: %w", name, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes root: %s", name)
	}

	return target, nil
}
