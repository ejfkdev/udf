package image

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

const rpmFixture = "testdata/simple-1.0.1-1.i386.rpm"

func TestRPMListAndOpen(t *testing.T) {
	if c := ClassifyInput(rpmFixture); c != "archive" {
		t.Fatalf("ClassifyInput = %q, want archive", c)
	}

	info, err := PlainArchiveInfo(rpmFixture)
	if err != nil {
		t.Fatal(err)
	}
	if info.Format != "rpm" {
		t.Fatalf("format = %q, want rpm", info.Format)
	}

	entries, err := ListPlainArchive(rpmFixture, "")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name] = true
	}
	if !names["config"] || !names["normal"] {
		t.Fatalf("missing config/normal in listing: %v", names)
	}

	rc, size, err := ReadPlainArchiveFile(rpmFixture, "config")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || string(data) != "config\n" || size != 7 {
		t.Fatalf("config = %q (size %d, err %v)", data, size, err)
	}
}

func TestRPMExtract(t *testing.T) {
	dest := t.TempDir()
	if _, err := ExtractPlainArchive(rpmFixture, dest, 4096); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "config")); err != nil || string(b) != "config\n" {
		t.Fatalf("config = %q (err %v)", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "normal")); err != nil || len(b) != 7 {
		t.Fatalf("normal = %q (err %v)", b, err)
	}
}
