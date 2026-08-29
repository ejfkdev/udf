package image

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// buildAppImage crafts a minimal 64-bit LE ELF whose one PT_LOAD segment spans
// [0, 0x1000), then appends a "hsqs" SquashFS magic at 0x1000 to mimic the
// AppImage layout, returning the path and the expected SquashFS offset.
func buildAppImage(t *testing.T) (string, int64) {
	t.Helper()
	ehdr := make([]byte, 64)
	ehdr[0], ehdr[1], ehdr[2], ehdr[3] = 0x7f, 'E', 'L', 'F'
	ehdr[4] = 2 // ELFCLASS64
	ehdr[5] = 1 // ELFDATA2LSB
	ehdr[6] = 1
	binary.LittleEndian.PutUint64(ehdr[32:40], 64) // e_phoff = 64
	binary.LittleEndian.PutUint16(ehdr[54:56], 56) // e_phentsize = 56
	binary.LittleEndian.PutUint16(ehdr[56:58], 1)  // e_phnum = 1

	ph := make([]byte, 56)
	binary.LittleEndian.PutUint32(ph[0:4], 1)        // p_type = PT_LOAD
	binary.LittleEndian.PutUint64(ph[8:16], 0)       // p_offset = 0
	binary.LittleEndian.PutUint64(ph[32:40], 0x1000) // p_filesz = 0x1000

	out := append(ehdr, ph...)
	out = append(out, make([]byte, 0x1000-len(out))...)
	out = append(out, []byte("hsqs")...)
	out = append(out, make([]byte, 64)...)

	path := filepath.Join(t.TempDir(), "app.AppImage")
	if err := os.WriteFile(path, out, 0o755); err != nil {
		t.Fatal(err)
	}
	return path, 0x1000
}

func TestAppImageDetect(t *testing.T) {
	path, want := buildAppImage(t)
	if f, err := detectDiskContainer(path); err != nil || f != "appimage" {
		t.Fatalf("detectDiskContainer = %q, %v; want appimage", f, err)
	}
	if !IsDiskImage(path) {
		t.Fatalf("IsDiskImage = false")
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	off, err := appimageSquashfsOffset(f, st.Size())
	if err != nil || off != want {
		t.Fatalf("appimageSquashfsOffset = %d, %v; want %d", off, err, want)
	}
}
