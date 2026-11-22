package image

import (
	"os"
	"path/filepath"
	"testing"
)

// The tiny DMG fixtures live in the dmg package's testdata: 2 MiB HFS+ images
// written by hdiutil in every UDIF compression format, holding a.txt, d/b.txt
// and a symlink. They were verified against mounted volumes before being
// checked in, so the end-to-end assertions here are the mounted truth.
//
// The full-size fixtures (GPT/MBR/APM/no-partition layouts, ~3 MiB) are
// env-gated below; hdiutil only exists on macOS.
func dmgFixture(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("..", "dmg", "testdata", name)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return path
}

var dmgTinyFixtures = []string{
	"tiny-UDZO.dmg", // zlib
	"tiny-UDBZ.dmg", // bzip2
	"tiny-ULMO.dmg", // xz
	"tiny-ULFO.dmg", // lzfse
	"tiny-UDCO.dmg", // ADC
	"tiny-UDRO.dmg", // raw
}

func TestDMGDetectedAndListed(t *testing.T) {
	for _, name := range dmgTinyFixtures {
		t.Run(name, func(t *testing.T) {
			path := dmgFixture(t, name)
			if got := DetectInput(path); got != "disk" {
				t.Fatalf("DetectInput = %q, want disk", got)
			}
			meta, err := ScanDiskMetadata(path)
			if err != nil {
				t.Fatalf("ScanDiskMetadata: %v", err)
			}
			if len(meta.Disks) != 1 || meta.Disks[0].Format != "dmg" {
				t.Fatalf("metadata: %+v", meta)
			}
			vols := fsVolumes(meta.Disks[0].Volumes)
			if len(vols) != 1 || vols[0].FSType != "hfsplus" {
				t.Fatalf("volumes: %+v", meta.Disks[0].Volumes)
			}
			entries, err := ListDisk(path, "/")
			if err != nil {
				t.Fatalf("ListDisk: %v", err)
			}
			var names []string
			for _, e := range entries {
				names = append(names, e.Name)
			}
			for _, want := range []string{"a.txt", "d", "link"} {
				found := false
				for _, n := range names {
					if n == want {
						found = true
					}
				}
				if !found {
					t.Fatalf("root is missing %s: %v", want, names)
				}
			}
			for _, e := range entries {
				if e.Name == "link" && (e.Type != "symlink" || e.Target != "a.txt") {
					t.Fatalf("link entry: %+v", e)
				}
			}
		})
	}
}

func TestDMGExtractMatchesMounted(t *testing.T) {
	for _, name := range dmgTinyFixtures {
		t.Run(name, func(t *testing.T) {
			path := dmgFixture(t, name)
			dest := t.TempDir()
			n, err := ExtractDiskPath(path, "/p1", dest, 64<<10)
			if err != nil {
				t.Fatalf("extract: %v", err)
			}
			if n != 4 { // a.txt, d/, d/b.txt, link
				t.Fatalf("extracted %d entries, want 4", n)
			}
			assertFile(t, filepath.Join(dest, "a.txt"), "tiny dmg\n", 0o644)
			assertFile(t, filepath.Join(dest, "d", "b.txt"), "nested\n", 0o644)
			assertSymlink(t, filepath.Join(dest, "link"), "a.txt")
		})
	}
}

// TestDMGSingleVolumeImplicitPath pins the friendly behavior: with one
// filesystem volume, "ls image" is the volume root without naming it.
func TestDMGSingleVolumeImplicitPath(t *testing.T) {
	path := dmgFixture(t, "tiny-UDZO.dmg")
	entries, err := ListDisk(path, "")
	if err != nil {
		t.Fatalf("ListDisk with empty path: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("empty-path listing came back empty")
	}
}

// The fixtures below cover the four partition layouts at full size. Point
// UDF_DMG_DIR at a directory holding them (hdiutil-created; see
// dmg/testdata for how they were built).
func requireDMGDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("UDF_DMG_DIR")
	if dir == "" {
		t.Skip("set UDF_DMG_DIR to a directory with hdiutil-made DMGs")
	}
	return dir
}

func TestDMGPartitionLayouts(t *testing.T) {
	dir := requireDMGDir(t)
	cases := []struct {
		file string
		vol  string // expected single filesystem volume name
	}{
		{"test-UDZO.dmg", "p1"}, // GPT layout
		{"mbr.dmg", "p1"},       // MBR layout
		{"apm.dmg", "p1"},       // Apple Partition Map
		{"bare.dmg", "disk"},    // no partition map
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			path := filepath.Join(dir, tc.file)
			if _, err := os.Stat(path); err != nil {
				t.Skipf("no fixture at %s", path)
			}
			meta, err := ScanDiskMetadata(path)
			if err != nil {
				t.Fatalf("ScanDiskMetadata: %v", err)
			}
			vols := fsVolumes(meta.Disks[0].Volumes)
			if len(vols) != 1 || vols[0].Name != tc.vol || vols[0].FSType != "hfsplus" {
				t.Fatalf("volumes: %+v, want [%s(hfsplus)]", meta.Disks[0].Volumes, tc.vol)
			}
			entries, err := ListDisk(path, "/")
			if err != nil {
				t.Fatalf("ListDisk: %v", err)
			}
			if len(entries) < 3 {
				t.Fatalf("root lists %d entries: %+v", len(entries), entries)
			}
		})
	}
}

// TestDMGBigFixtureExtraction runs a content-level differential against the
// source directory when UDF_DMG_DIR/bigsrc exists, mirroring how the fixtures
// were verified during development.
func TestDMGBigFixtureExtraction(t *testing.T) {
	dir := requireDMGDir(t)
	src := filepath.Join(dir, "bigsrc")
	if _, err := os.Stat(src); err != nil {
		t.Skip("no bigsrc directory with the source files")
	}
	path := filepath.Join(dir, "big.dmg")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("no fixture at %s", path)
	}
	dest := t.TempDir()
	if _, err := ExtractDiskPath(path, "/p1", dest, 1<<20); err != nil {
		t.Fatalf("extract: %v", err)
	}
	if err := compareTrees(src, dest); err != nil {
		t.Fatalf("extracted tree differs: %v", err)
	}
}

// ---------- local helpers for the fixtures ----------

func assertFile(t *testing.T, path, want string, perm os.FileMode) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(data) != want {
		t.Fatalf("%s = %q, want %q", path, data, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != perm {
		t.Fatalf("%s mode %v, want %v", path, info.Mode().Perm(), perm)
	}
}

func assertSymlink(t *testing.T, path, wantTarget string) {
	t.Helper()
	target, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("readlink %s: %v", path, err)
	}
	if target != wantTarget {
		t.Fatalf("%s -> %q, want %q", path, target, wantTarget)
	}
}

// compareTrees checks that dest holds exactly src's regular-file contents
// (recursive; symlinks and empty directories are compared structurally).
func compareTrees(src, dest string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		other := filepath.Join(dest, rel)
		o, oerr := os.Lstat(other)
		if oerr != nil {
			return oerr
		}
		if info.IsDir() {
			if !o.IsDir() {
				return os.ErrInvalid
			}
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			want, err := os.Readlink(p)
			if err != nil {
				return err
			}
			if got, err := os.Readlink(other); err != nil || got != want {
				return os.ErrInvalid
			}
			return nil
		}
		want, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		got, err := os.ReadFile(other)
		if err != nil {
			return err
		}
		if !bytesEqual(want, got) {
			return os.ErrInvalid
		}
		return nil
	})
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
