package image

import (
	"io"
	"path/filepath"
	"strings"
	"testing"
)

// The fixtures in testdata/gcr come from go-containerregistry (see its README):
// docker-save archives built by another project's tooling. The assertions here
// are the ones its own tests make, so udf's layer merge is checked against
// bytes it did not produce.

// TestWhiteoutFixtureFromGoContainerRegistry: a whiteout must remove the entry
// it names, and no whiteout entry may surface in the merged listing.
func TestWhiteoutFixtureFromGoContainerRegistry(t *testing.T) {
	fixture := filepath.Join("testdata", "gcr", "whiteout_image.tar")
	meta, err := ScanImageMetadata(fixture, Selection{ImageIndex: -1})
	if err != nil {
		t.Fatal(err)
	}
	tree, err := BuildFileSystem(fixture, meta)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := ListEntries(tree, "/")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name)
		if strings.Contains(e.Name, "foo") {
			t.Errorf("whiteout file or its target surfaced in the listing: %v", e.Name)
		}
	}
	if len(names) == 0 {
		t.Fatal("the fixture listed nothing")
	}
	// The whiteout must not have taken the sibling with it.
	found := false
	for _, n := range names {
		if n == "bar.txt" {
			found = true
		}
	}
	if !found {
		t.Fatalf("bar.txt is missing after the whiteout: %v", names)
	}
}

// TestWhiteoutDirFixtureFromGoContainerRegistry: a whiteout naming a directory
// removes that directory rather than leaving an empty one.
func TestWhiteoutDirFixtureFromGoContainerRegistry(t *testing.T) {
	fixture := filepath.Join("testdata", "gcr", "whiteout_dir.tar")
	meta, err := ScanImageMetadata(fixture, Selection{ImageIndex: -1})
	if err != nil {
		t.Fatal(err)
	}
	tree, err := BuildFileSystem(fixture, meta)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := ListEntries(tree, "/")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name == "foo" {
			t.Fatalf("the whiteouted directory survived: %v", entries)
		}
	}
}

// TestOverwrittenFileFixtureFromGoContainerRegistry: the later layer's version
// of a file wins, and following the symlink that replaced it must not reach the
// overwritten bytes.
func TestOverwrittenFileFixtureFromGoContainerRegistry(t *testing.T) {
	fixture := filepath.Join("testdata", "gcr", "overwritten_file.tar")
	meta, err := ScanImageMetadata(fixture, Selection{ImageIndex: -1})
	if err != nil {
		t.Fatal(err)
	}
	rc, _, err := ReadArchiveFile(fixture, meta, "/foo.txt")
	if err != nil {
		t.Fatalf("read foo.txt: %v", err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "foo") {
		t.Fatalf("the overwritten content came back: %q", body)
	}
}
