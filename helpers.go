package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ejfkdev/udf/image"
	arch "github.com/ejfkdev/udf/image/archive"
	"github.com/ejfkdev/udf/types"
)

// resolveInputs turns the raw archive inputs (glob patterns, directories or
// single files, comma-split by the CLI frontend) into a deduplicated,
// sorted list of supported archive files.
func resolveInputs(inputs []string) ([]string, error) {
	seen := make(map[string]struct{})
	var results []string

	for _, input := range inputs {
		paths, err := expandInput(input)
		if err != nil {
			return nil, err
		}
		for _, p := range paths {
			if _, ok := seen[p]; ok {
				continue
			}
			seen[p] = struct{}{}
			results = append(results, p)
		}
	}
	sort.Strings(results)
	return results, nil
}

func expandInput(input string) ([]string, error) {
	if hasGlob(input) {
		matches, err := filepath.Glob(input)
		if err != nil {
			return nil, fmt.Errorf("parse glob pattern %s: %w", input, err)
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf("glob pattern matched no files: %s", input)
		}
		return filterArchives(matches)
	}

	info, err := os.Stat(input)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		entries, err := os.ReadDir(input)
		if err != nil {
			return nil, err
		}
		var paths []string
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			paths = append(paths, filepath.Join(input, entry.Name()))
		}
		return filterArchives(paths)
	}

	return filterArchives([]string{input})
}

func filterArchives(paths []string) ([]string, error) {
	var filtered []string
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if info.IsDir() {
			continue
		}
		if isSupportedArchive(p) {
			filtered = append(filtered, p)
		}
	}
	return filtered, nil
}

// resolveDiskOrArchiveInputs resolves an extract input expression: a single
// archive/disk image, a glob, a directory of such files, or an OCI image layout
// directory (which is treated as one image, not a batch directory).
func resolveDiskOrArchiveInputs(input string) ([]string, error) {
	if arch.IsOCILayout(input) {
		return []string{input}, nil
	}
	return resolveInputs([]string{input})
}

func hasGlob(s string) bool {
	return strings.ContainsAny(s, "*?[")
}

// isSupportedArchive reports whether path is a recognized input, determined by
// file content (magic) rather than extension.
func isSupportedArchive(path string) bool {
	return image.DetectInput(path) != ""
}

func resolveOutputDir(imagePath, parentDir string, meta *types.ImageMetadata) string {
	if parentDir == "" {
		parentDir = filepath.Dir(imagePath)
	}

	baseDir := filepath.Join(parentDir, imageBaseName(imagePath))
	if meta == nil || meta.Total <= 1 {
		return baseDir
	}

	return filepath.Join(baseDir, imageVariantName(meta))
}

func imageVariantName(meta *types.ImageMetadata) string {
	if meta == nil {
		return "index-0"
	}
	if len(meta.RepoTags) > 0 && strings.TrimSpace(meta.RepoTags[0]) != "" {
		return sanitizeDirName(meta.RepoTags[0])
	}
	return fmt.Sprintf("index-%d", meta.Index)
}

func sanitizeDirName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "untagged"
	}

	replacer := strings.NewReplacer(
		"/", "_",
		"\\", "_",
		":", "_",
		"@", "_",
		" ", "_",
	)
	value = replacer.Replace(value)

	var b strings.Builder
	lastUnderscore := false
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_' {
			b.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if !lastUnderscore {
			b.WriteByte('_')
			lastUnderscore = true
		}
	}

	result := strings.Trim(b.String(), "._-")
	if result == "" {
		return "untagged"
	}
	return result
}

func imageBaseName(imagePath string) string {
	base := filepath.Base(imagePath)
	lower := strings.ToLower(base)
	switch {
	case strings.HasSuffix(lower, ".tar.gz"):
		base = base[:len(base)-len(".tar.gz")]
	case strings.HasSuffix(lower, ".tgz"):
		base = base[:len(base)-len(".tgz")]
	case strings.HasSuffix(lower, ".zip"):
		base = base[:len(base)-len(".zip")]
	case strings.HasSuffix(lower, ".qcow2"):
		base = base[:len(base)-len(".qcow2")]
	case strings.HasSuffix(lower, ".vmdk"):
		base = base[:len(base)-len(".vmdk")]
	case strings.HasSuffix(lower, ".vhd"):
		base = base[:len(base)-len(".vhd")]
	case strings.HasSuffix(lower, ".vhdx"):
		base = base[:len(base)-len(".vhdx")]
	case strings.HasSuffix(lower, ".ova"):
		base = base[:len(base)-len(".ova")]
	case strings.HasSuffix(lower, ".vma"):
		base = base[:len(base)-len(".vma")]
	default:
		base = strings.TrimSuffix(base, filepath.Ext(base))
	}
	if base == "" || base == "." {
		return "rootfs"
	}
	return base
}
