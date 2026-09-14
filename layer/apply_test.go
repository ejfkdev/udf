package layer

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestApplyLayerOverridesFile(t *testing.T) {
	root := t.TempDir()

	if _, err := ApplyLayer(buildLayer(t, []tarEntry{
		fileEntry("app/file.txt", "base"),
	}), root, make([]byte, 32*1024)); err != nil {
		t.Fatalf("apply base layer: %v", err)
	}

	if _, err := ApplyLayer(buildLayer(t, []tarEntry{
		fileEntry("app/file.txt", "top"),
	}), root, make([]byte, 32*1024)); err != nil {
		t.Fatalf("apply top layer: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(root, "app/file.txt"))
	if err != nil {
		t.Fatalf("read merged file: %v", err)
	}
	if got := string(data); got != "top" {
		t.Fatalf("unexpected file content: %q", got)
	}
}

func TestApplyLayerWhiteoutRemovesFile(t *testing.T) {
	root := t.TempDir()

	if _, err := ApplyLayer(buildLayer(t, []tarEntry{
		fileEntry("app/file.txt", "base"),
	}), root, make([]byte, 32*1024)); err != nil {
		t.Fatalf("apply base layer: %v", err)
	}

	if _, err := ApplyLayer(buildLayer(t, []tarEntry{
		whiteoutEntry("app/.wh.file.txt"),
	}), root, make([]byte, 32*1024)); err != nil {
		t.Fatalf("apply whiteout layer: %v", err)
	}

	if _, err := os.Stat(filepath.Join(root, "app/file.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected file to be removed, stat err=%v", err)
	}
}

func TestApplyLayerOpaqueWhiteoutClearsDirectory(t *testing.T) {
	root := t.TempDir()

	if _, err := ApplyLayer(buildLayer(t, []tarEntry{
		fileEntry("app/keep-old.txt", "old"),
		fileEntry("app/sub/old.txt", "old"),
	}), root, make([]byte, 32*1024)); err != nil {
		t.Fatalf("apply base layer: %v", err)
	}

	if _, err := ApplyLayer(buildLayer(t, []tarEntry{
		whiteoutEntry("app/.wh..wh..opq"),
		fileEntry("app/new.txt", "new"),
	}), root, make([]byte, 32*1024)); err != nil {
		t.Fatalf("apply opaque whiteout layer: %v", err)
	}

	if _, err := os.Stat(filepath.Join(root, "app/keep-old.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected old file removed, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "app/sub")); !os.IsNotExist(err) {
		t.Fatalf("expected old nested directory removed, stat err=%v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "app/new.txt"))
	if err != nil {
		t.Fatalf("read new file: %v", err)
	}
	if string(data) != "new" {
		t.Fatalf("unexpected new file content: %q", string(data))
	}
}

func TestApplyLayerRejectsPathEscape(t *testing.T) {
	root := t.TempDir()

	_, err := ApplyLayer(buildLayer(t, []tarEntry{
		fileEntry("../escape.txt", "bad"),
	}), root, make([]byte, 32*1024))
	if err == nil {
		t.Fatal("expected path escape error")
	}
}

type tarEntry struct {
	Header *tar.Header
	Body   []byte
}

func fileEntry(name, body string) tarEntry {
	return tarEntry{
		Header: &tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(body)),
			Typeflag: tar.TypeReg,
		},
		Body: []byte(body),
	}
}

func whiteoutEntry(name string) tarEntry {
	return tarEntry{
		Header: &tar.Header{
			Name:     name,
			Mode:     0o000,
			Size:     0,
			Typeflag: tar.TypeReg,
		},
	}
}

func buildLayer(t *testing.T, entries []tarEntry) *bytes.Reader {
	t.Helper()

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, entry := range entries {
		if err := tw.WriteHeader(entry.Header); err != nil {
			t.Fatalf("write header: %v", err)
		}
		if len(entry.Body) > 0 {
			if _, err := tw.Write(entry.Body); err != nil {
				t.Fatalf("write body: %v", err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}

	return bytes.NewReader(buf.Bytes())
}

func TestApplyLayerSkipsDeviceNodes(t *testing.T) {
	root := t.TempDir()

	if _, err := ApplyLayer(buildLayer(t, []tarEntry{
		fileEntry("hello.txt", "hi"),
		{Header: &tar.Header{
			Name: "dev/console", Mode: 0o600,
			Typeflag: tar.TypeChar, Devmajor: 5, Devminor: 1,
		}},
		{Header: &tar.Header{Name: "dev/fifo", Mode: 0o644, Typeflag: tar.TypeFifo}},
	}), root, make([]byte, 32*1024)); err != nil {
		t.Fatalf("apply layer with device nodes: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(root, "hello.txt"))
	if err != nil || string(data) != "hi" {
		t.Fatalf("regular file not extracted: %v %q", err, data)
	}
	if _, err := os.Lstat(filepath.Join(root, "dev", "console")); !os.IsNotExist(err) {
		t.Fatalf("device node should be skipped, Lstat err = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "dev", "fifo")); !os.IsNotExist(err) {
		t.Fatalf("fifo should be skipped, Lstat err = %v", err)
	}
}
