package fsutil

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

type DirMetadata struct {
	Path    string
	Mode    os.FileMode
	ModTime time.Time
}

func EnsureParentDir(targetPath string) error {
	return os.MkdirAll(filepath.Dir(targetPath), 0o755)
}

func ReplaceWithDir(targetPath string, mode os.FileMode) error {
	info, err := os.Lstat(targetPath)
	if err == nil {
		if info.IsDir() {
			return os.Chmod(targetPath, ensureTraversableMode(mode.Perm()))
		}
		if err := os.RemoveAll(targetPath); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.MkdirAll(targetPath, ensureTraversableMode(mode.Perm()))
}

func ReplaceWithFile(targetPath string, hdr *tar.Header, r io.Reader, buf []byte) error {
	if err := EnsureParentDir(targetPath); err != nil {
		return err
	}
	if err := os.RemoveAll(targetPath); err != nil && !os.IsNotExist(err) {
		return err
	}

	f, err := os.OpenFile(targetPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, hdr.FileInfo().Mode().Perm())
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := io.CopyBuffer(f, r, buf); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	return applyFileMetadata(targetPath, hdr)
}

func ReplaceWithSymlink(targetPath string, hdr *tar.Header) error {
	if err := EnsureParentDir(targetPath); err != nil {
		return err
	}
	if err := os.RemoveAll(targetPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Symlink(hdr.Linkname, targetPath)
}

func ReplaceWithHardlink(root, targetPath string, hdr *tar.Header) error {
	sourcePath, err := ResolveSafePath(root, hdr.Linkname)
	if err != nil {
		return fmt.Errorf("resolve hardlink source: %w", err)
	}
	if err := EnsureParentDir(targetPath); err != nil {
		return err
	}
	if err := os.RemoveAll(targetPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Link(sourcePath, targetPath); err != nil {
		if supportsHardlinkFallback(err) {
			return copyFile(sourcePath, targetPath)
		}
		return err
	}
	return nil
}

func applyFileMetadata(targetPath string, hdr *tar.Header) error {
	if err := os.Chmod(targetPath, hdr.FileInfo().Mode().Perm()); err != nil {
		return err
	}
	return os.Chtimes(targetPath, hdr.ModTime, hdr.ModTime)
}

func ApplyDirMetadata(dirs []DirMetadata) error {
	sort.Slice(dirs, func(i, j int) bool {
		di := strings.Count(filepath.Clean(dirs[i].Path), string(filepath.Separator))
		dj := strings.Count(filepath.Clean(dirs[j].Path), string(filepath.Separator))
		if di == dj {
			return dirs[i].Path > dirs[j].Path
		}
		return di > dj
	})

	for _, dir := range dirs {
		info, err := os.Lstat(dir.Path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			continue
		}
		if err := os.Chmod(dir.Path, dir.Mode.Perm()); err != nil {
			return err
		}
		if err := os.Chtimes(dir.Path, dir.ModTime, dir.ModTime); err != nil {
			return err
		}
	}
	return nil
}

func ensureTraversableMode(mode os.FileMode) os.FileMode {
	return mode | 0o700
}

func supportsHardlinkFallback(err error) bool {
	return errors.Is(err, syscall.EXDEV) ||
		errors.Is(err, syscall.EPERM) ||
		errors.Is(err, syscall.ENOTSUP) ||
		errors.Is(err, syscall.EOPNOTSUPP)
}

func copyFile(sourcePath, targetPath string) error {
	src, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer src.Close()

	info, err := src.Stat()
	if err != nil {
		return err
	}

	dst, err := os.OpenFile(targetPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}

	return os.Chtimes(targetPath, info.ModTime(), info.ModTime())
}
