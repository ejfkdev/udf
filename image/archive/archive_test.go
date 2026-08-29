package archive

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// createSevenZip builds a .7z holding hello.txt and sub/nested.txt and returns
// its path. It requires 7z and skips otherwise.
func createSevenZip(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("7z")
	if err != nil {
		t.Skip("7z not available to build a .7z fixture")
	}

	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "hello.txt"), []byte("hello 7z\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "nested.txt"), []byte("nested 7z\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "test.7z")
	cmd := exec.Command(p, "a", "-t7z", out, ".")
	cmd.Dir = src
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("7z a: %v: %s", err, b)
	}
	return out
}

func TestSevenZipListAndOpen(t *testing.T) {
	a := &sevenzipArchive{path: createSevenZip(t)}

	entries, err := a.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name] = true
	}
	for _, want := range []string{"hello.txt", "sub/nested.txt", "sub/"} {
		if !names[want] {
			t.Fatalf("missing %q in 7z listing, got %v", want, names)
		}
	}

	rc, size, err := a.Open("hello.txt")
	if err != nil {
		t.Fatalf("Open hello.txt: %v", err)
	}
	data, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatalf("read hello.txt: %v", err)
	}
	if string(data) != "hello 7z\n" || size != int64(len("hello 7z\n")) {
		t.Fatalf("hello.txt = %q (size %d)", data, size)
	}
}

// createCPIO builds a newc-format cpio archive holding hello.txt and
// sub/nested.txt and returns its path. It requires the cpio CLI and skips
// otherwise.
func createCPIO(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("cpio"); err != nil {
		t.Skip("cpio not available to build a fixture")
	}

	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "hello.txt"), []byte("hello cpio\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "nested.txt"), []byte("nested cpio\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "test.cpio")
	cmd := exec.Command("sh", "-c", "find . | cpio -o -H newc > '"+out+"'")
	cmd.Dir = src
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cpio -o: %v: %s", err, b)
	}
	return out
}

func TestCPIOListAndOpen(t *testing.T) {
	a := &cpioArchive{path: createCPIO(t)}

	entries, err := a.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name] = true
		// GNU cpio strips the "./" prefix that BSD cpio keeps, so the fixture's
		// entry names differ by platform; normalize both spellings.
		names[strings.TrimPrefix(e.Name, "./")] = true
	}
	if !names["hello.txt"] || !names["sub/nested.txt"] {
		t.Fatalf("expected hello.txt and sub/nested.txt in listing, got %v", names)
	}

	rc, size, err := a.Open("hello.txt")
	if err != nil {
		rc, size, err = a.Open("./hello.txt")
		if err != nil {
			t.Fatalf("Open hello.txt: %v", err)
		}
	}
	data, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatalf("read hello.txt: %v", err)
	}
	if string(data) != "hello cpio\n" || size != int64(len("hello cpio\n")) {
		t.Fatalf("hello.txt = %q (size %d)", data, size)
	}
}

// createRAR builds a .rar holding hello.txt and sub/nested.txt and returns its
// path. It requires the rar CLI (test-only fixture creation) and skips
// otherwise.
func createRAR(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("rar")
	if err != nil {
		t.Skip("rar not available to build a .rar fixture")
	}

	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "hello.txt"), []byte("hello rar\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "nested.txt"), []byte("nested rar\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "test.rar")
	cmd := exec.Command(p, "a", "-r", "-idq", out, "hello.txt", "sub")
	cmd.Dir = src
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("rar a: %v: %s", err, b)
	}
	return out
}

func TestRARListAndOpen(t *testing.T) {
	a := &rarArchive{path: createRAR(t)}

	entries, err := a.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name] = true
	}
	if !names["hello.txt"] || !names["sub/nested.txt"] {
		t.Fatalf("expected hello.txt and sub/nested.txt in listing, got %v", names)
	}

	rc, size, err := a.Open("hello.txt")
	if err != nil {
		t.Fatalf("Open hello.txt: %v", err)
	}
	data, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatalf("read hello.txt: %v", err)
	}
	if string(data) != "hello rar\n" || size != int64(len("hello rar\n")) {
		t.Fatalf("hello.txt = %q (size %d)", data, size)
	}
}
