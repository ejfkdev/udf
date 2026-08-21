package image

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	appi18n "github.com/ejfkdev/udf/i18n"
)

func TestExtractPathSingleFile(t *testing.T) {
	f := buildTestImage(t, map[string][]layerTarEntry{
		"a.tar": {
			imgDir("etc"),
			imgFile("etc/passwd", "old"),
		},
		"b.tar": {
			imgFile("etc/passwd", "new"),
		},
	})

	dest := filepath.Join(t.TempDir(), "passwd.out")
	count, err := ExtractPath(f.ImagePath, f.Meta, "/etc/passwd", dest, 1<<16)
	if err != nil {
		t.Fatalf("extract single file: %v", err)
	}
	if count != 1 {
		t.Fatalf("unexpected entry count: %d", count)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read extracted file: %v", err)
	}
	if string(data) != "new" {
		t.Fatalf("unexpected content: %q", string(data))
	}
}

func TestExtractPathSingleFileIntoExistingDir(t *testing.T) {
	f := buildTestImage(t, map[string][]layerTarEntry{
		"a.tar": {imgFile("etc/passwd", "root:x:0:0")},
	})

	destDir := t.TempDir()
	if _, err := ExtractPath(f.ImagePath, f.Meta, "/etc/passwd", destDir, 1<<16); err != nil {
		t.Fatalf("extract single file into dir: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(destDir, "passwd"))
	if err != nil {
		t.Fatalf("read extracted file: %v", err)
	}
	if string(data) != "root:x:0:0" {
		t.Fatalf("unexpected content: %q", string(data))
	}
}

func TestExtractPathWhiteoutSourceNotFound(t *testing.T) {
	f := buildTestImage(t, map[string][]layerTarEntry{
		"a.tar": {imgFile("etc/motd", "welcome")},
		"b.tar": {imgWhiteout("etc/.wh.motd")},
	})

	_, err := ExtractPath(f.ImagePath, f.Meta, "/etc/motd", filepath.Join(t.TempDir(), "motd"), 1<<16)
	var le *appi18n.LocalizedError
	if !errors.As(err, &le) || le.Key != "err_cp_src_not_found" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestExtractPathDirectory(t *testing.T) {
	f := buildTestImage(t, map[string][]layerTarEntry{
		"a.tar": {
			imgFile("app/config/x.conf", "x"),
			imgFile("app/secret/removed.txt", "secret"),
		},
		"b.tar": {
			imgWhiteout("app/.wh.secret"),
			imgFile("app/config/y.conf", "y"),
		},
	})

	dest := filepath.Join(t.TempDir(), "app-out")
	count, err := ExtractPath(f.ImagePath, f.Meta, "/app", dest, 1<<16)
	if err != nil {
		t.Fatalf("extract dir: %v", err)
	}
	if count == 0 {
		t.Fatal("expected entries to be extracted")
	}

	xData, err := os.ReadFile(filepath.Join(dest, "config", "x.conf"))
	if err != nil {
		t.Fatalf("read x.conf: %v", err)
	}
	if string(xData) != "x" {
		t.Fatalf("unexpected x.conf content: %q", string(xData))
	}
	if _, err := os.ReadFile(filepath.Join(dest, "config", "y.conf")); err != nil {
		t.Fatalf("read y.conf: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "secret")); !os.IsNotExist(err) {
		t.Fatalf("expected whiteout dir not extracted, stat err=%v", err)
	}
}

func TestExtractPathDirectoryIntoExistingDir(t *testing.T) {
	f := buildTestImage(t, map[string][]layerTarEntry{
		"a.tar": {imgFile("app/config/x.conf", "x")},
	})

	destDir := t.TempDir()
	if _, err := ExtractPath(f.ImagePath, f.Meta, "/app", destDir, 1<<16); err != nil {
		t.Fatalf("extract dir into existing dir: %v", err)
	}
	if _, err := os.ReadFile(filepath.Join(destDir, "app", "config", "x.conf")); err != nil {
		t.Fatalf("expected app/config/x.conf under dest dir: %v", err)
	}
}

func TestExtractPathHardlinkInsideSelection(t *testing.T) {
	f := buildTestImage(t, map[string][]layerTarEntry{
		"a.tar": {
			imgDir("lib"),
			imgFile("lib/real.txt", "data"),
			imgHardlink("lib/linked.txt", "/lib/real.txt"),
		},
	})

	dest := filepath.Join(t.TempDir(), "lib-out")
	if _, err := ExtractPath(f.ImagePath, f.Meta, "/lib", dest, 1<<16); err != nil {
		t.Fatalf("extract dir with hardlink: %v", err)
	}

	realInfo, err := os.Stat(filepath.Join(dest, "real.txt"))
	if err != nil {
		t.Fatalf("stat real.txt: %v", err)
	}
	linkInfo, err := os.Stat(filepath.Join(dest, "linked.txt"))
	if err != nil {
		t.Fatalf("stat linked.txt: %v", err)
	}
	if !os.SameFile(realInfo, linkInfo) {
		t.Fatal("expected linked.txt to be a hardlink of real.txt")
	}
}

func TestExtractPathHardlinkSourceOutsideSelection(t *testing.T) {
	f := buildTestImage(t, map[string][]layerTarEntry{
		"a.tar": {
			imgFile("lib/real.txt", "shared-data"),
			imgHardlink("bin/tool", "/lib/real.txt"),
		},
	})

	dest := filepath.Join(t.TempDir(), "bin-out")
	if _, err := ExtractPath(f.ImagePath, f.Meta, "/bin", dest, 1<<16); err != nil {
		t.Fatalf("extract hardlink with external source: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dest, "tool"))
	if err != nil {
		t.Fatalf("read tool: %v", err)
	}
	if string(data) != "shared-data" {
		t.Fatalf("unexpected content: %q", string(data))
	}
}

func TestExtractPathSymlink(t *testing.T) {
	f := buildTestImage(t, map[string][]layerTarEntry{
		"a.tar": {imgSymlink("bin/sh", "bash")},
	})

	dest := filepath.Join(t.TempDir(), "sh")
	if _, err := ExtractPath(f.ImagePath, f.Meta, "/bin/sh", dest, 1<<16); err != nil {
		t.Fatalf("extract symlink: %v", err)
	}

	target, err := os.Readlink(dest)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if target != "bash" {
		t.Fatalf("unexpected link target: %q", target)
	}
}

func TestExtractPathImageRoot(t *testing.T) {
	f := buildTestImage(t, map[string][]layerTarEntry{
		"a.tar": {
			imgFile("etc/passwd", "root:x:0:0"),
			imgFile("usr/bin/tool", "#!/bin/sh"),
		},
	})

	dest := filepath.Join(t.TempDir(), "rootfs")
	if _, err := ExtractPath(f.ImagePath, f.Meta, "/", dest, 1<<16); err != nil {
		t.Fatalf("extract image root: %v", err)
	}

	if _, err := os.ReadFile(filepath.Join(dest, "etc", "passwd")); err != nil {
		t.Fatalf("expected etc/passwd at dest root: %v", err)
	}
	if _, err := os.ReadFile(filepath.Join(dest, "usr", "bin", "tool")); err != nil {
		t.Fatalf("expected usr/bin/tool at dest root: %v", err)
	}
}
