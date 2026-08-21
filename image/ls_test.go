package image

import (
	"errors"
	"strings"
	"testing"

	"github.com/ejfkdev/udf/fsview"
	appi18n "github.com/ejfkdev/udf/i18n"
	"github.com/ejfkdev/udf/types"
)

type testImageFixture struct {
	ImagePath string
	Meta      *types.ImageMetadata
	Root      *fsview.Node
}

func buildTestImage(t *testing.T, layers map[string][]layerTarEntry) testImageFixture {
	t.Helper()
	imagePath := writeTestImage(t, layers)
	meta := scanTestMeta(t, imagePath)
	root, err := BuildFileSystem(imagePath, meta)
	if err != nil {
		t.Fatalf("build filesystem: %v", err)
	}
	return testImageFixture{ImagePath: imagePath, Meta: meta, Root: root}
}

func TestFormatListingShowsMergedView(t *testing.T) {
	f := buildTestImage(t, map[string][]layerTarEntry{
		"a.tar": {
			imgDir("etc"),
			imgFile("etc/passwd", "root:x:0:0"),
			imgFile("etc/deleted.txt", "bye"),
			imgFile("readme.txt", "read me"),
		},
		"b.tar": {
			imgWhiteout("etc/.wh.deleted.txt"),
			imgFile("usr/bin/tool", "#!/bin/sh"),
			imgSymlink("bin", "usr/bin"),
		},
	})

	out, err := FormatListing(f.Root, "/")
	if err != nil {
		t.Fatalf("format listing: %v", err)
	}

	for _, want := range []string{
		"total ",
		"-rw-r--r--",
		"etc",
		"usr",
		"bin -> usr/bin",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected listing to contain %q, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "deleted.txt") {
		t.Errorf("whiteout file should not appear in listing:\n%s", out)
	}
}

func TestFormatListingDirectoryArgument(t *testing.T) {
	f := buildTestImage(t, map[string][]layerTarEntry{
		"a.tar": {
			imgDir("etc"),
			imgFile("etc/passwd", "root:x:0:0"),
			imgFile("etc/group", "root:x:0:"),
		},
	})

	out, err := FormatListing(f.Root, "/etc")
	if err != nil {
		t.Fatalf("format listing: %v", err)
	}
	if !strings.Contains(out, "passwd") || !strings.Contains(out, "group") {
		t.Errorf("expected passwd and group entries, got:\n%s", out)
	}

	single, err := FormatListing(f.Root, "/etc/passwd")
	if err != nil {
		t.Fatalf("format single file listing: %v", err)
	}
	if !strings.Contains(single, "passwd") || strings.Contains(single, "total ") {
		t.Errorf("unexpected single file listing:\n%s", single)
	}
}

func TestFormatListingPathNotFound(t *testing.T) {
	f := buildTestImage(t, map[string][]layerTarEntry{
		"a.tar": {imgFile("etc/passwd", "x")},
	})

	_, err := FormatListing(f.Root, "/no/such/path")
	var le *appi18n.LocalizedError
	if !errors.As(err, &le) || le.Key != "err_ls_path_not_found" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestFormatListingShowsSymlinkAndHardlink(t *testing.T) {
	f := buildTestImage(t, map[string][]layerTarEntry{
		"a.tar": {
			imgDir("lib"),
			imgFile("lib/real.txt", "data"),
			imgHardlink("lib/linked.txt", "/lib/real.txt"),
			imgSymlink("latest", "lib/real.txt"),
		},
	})

	out, err := FormatListing(f.Root, "/lib")
	if err != nil {
		t.Fatalf("format listing: %v", err)
	}
	if !strings.Contains(out, "real.txt") || !strings.Contains(out, "linked.txt") {
		t.Errorf("expected lib entries, got:\n%s", out)
	}

	out, err = FormatListing(f.Root, "/latest")
	if err != nil {
		t.Fatalf("format symlink listing: %v", err)
	}
	if !strings.Contains(out, "latest -> lib/real.txt") {
		t.Errorf("expected symlink target in listing, got:\n%s", out)
	}
}
