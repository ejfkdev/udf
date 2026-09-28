package image

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeQCow2Header builds the head of a qcow2 file with (or without) a backing
// file name, the way the emulator writes its userdata delta.
func writeQCow2Header(t *testing.T, backing string) string {
	t.Helper()
	buf := make([]byte, 4096)
	copy(buf, magicQCow)
	binary.BigEndian.PutUint32(buf[4:8], 3)
	if backing != "" {
		const nameOff = 512
		binary.BigEndian.PutUint64(buf[8:16], nameOff)
		binary.BigEndian.PutUint32(buf[16:20], uint32(len(backing)))
		copy(buf[nameOff:], backing)
	}
	path := filepath.Join(t.TempDir(), "overlay.qcow2")
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestQCow2BackingFile(t *testing.T) {
	with := writeQCow2Header(t, "userdata-qemu.img")
	if got := qcow2BackingFile(with); got != "userdata-qemu.img" {
		t.Fatalf("backing file = %q, want userdata-qemu.img", got)
	}
	if hint := noFilesystemHint(with); !strings.Contains(hint, "userdata-qemu.img") {
		t.Fatalf("hint does not name the backing file: %q", hint)
	}

	plain := writeQCow2Header(t, "")
	if got := qcow2BackingFile(plain); got != "" {
		t.Fatalf("a qcow2 without a backing file reported %q", got)
	}
	if hint := noFilesystemHint(plain); hint != "" {
		t.Fatalf("unexpected hint for a standalone qcow2: %q", hint)
	}

	// Anything that is not a qcow2 has no backing file to report.
	other := filepath.Join(t.TempDir(), "raw.img")
	if err := os.WriteFile(other, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := qcow2BackingFile(other); got != "" {
		t.Fatalf("a raw image reported a backing file %q", got)
	}
	if hint := noFilesystemHint(other); hint != "" {
		t.Fatalf("unexpected hint for a raw image: %q", hint)
	}
}
