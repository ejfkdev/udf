package image

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// writeVMExport builds a minimal "version:1.0\ncfg:<N>" VM export: a 2048-byte
// header, a gzip metadata section, zero padding, then a gzip tar holding one
// disk image.
func writeVMExport(t *testing.T, diskName string, diskBytes []byte) string {
	t.Helper()

	// Metadata section: a gzip'd empty tar (its content is irrelevant; openVMExport
	// only uses its declared length to skip past it).
	var metaTar bytes.Buffer
	mtw := tar.NewWriter(&metaTar)
	if err := mtw.Close(); err != nil {
		t.Fatalf("close meta tar: %v", err)
	}
	metaGz := gzipBytes(t, metaTar.Bytes())

	// Disk section: a gzip'd tar with a single disk entry.
	var diskTar bytes.Buffer
	dtw := tar.NewWriter(&diskTar)
	if err := dtw.WriteHeader(&tar.Header{
		Name: diskName, Mode: 0o644, Size: int64(len(diskBytes)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatalf("write disk header: %v", err)
	}
	if _, err := dtw.Write(diskBytes); err != nil {
		t.Fatalf("write disk body: %v", err)
	}
	if err := dtw.Close(); err != nil {
		t.Fatalf("close disk tar: %v", err)
	}
	diskGz := gzipBytes(t, diskTar.Bytes())

	var hdr [2048]byte
	copy(hdr[:], fmt.Sprintf("version:1.0\ncfg:%d\n", len(metaGz)))

	var out bytes.Buffer
	out.Write(hdr[:])
	out.Write(metaGz)
	out.Write(make([]byte, 1<<20)) // zero padding between sections
	out.Write(diskGz)

	path := filepath.Join(t.TempDir(), "export.vma")
	if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
		t.Fatalf("write export fixture: %v", err)
	}
	return path
}

func gzipBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(b); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func TestVMExportExtract(t *testing.T) {
	rawPath := createExt4Image(t)
	qcow2Path := writeQCow2Image(t, mustReadFile(t, rawPath), 16)
	exportPath := writeVMExport(t, "vm-disk-1.qcow2", mustReadFile(t, qcow2Path))

	meta, err := ScanDiskMetadata(exportPath)
	if err != nil {
		t.Fatalf("scan vm export metadata: %v", err)
	}
	if len(meta.Disks) != 1 || meta.Disks[0].Format != "qcow2" || meta.Disks[0].Filesystem != "ext4" {
		t.Fatalf("unexpected vm export metadata: %+v", meta)
	}

	dest := filepath.Join(t.TempDir(), "passwd.out")
	if _, err := ExtractDiskPath(exportPath, "/etc/passwd", dest, 1<<16); err != nil {
		t.Fatalf("extract vm export file: %v", err)
	}
	if data, _ := os.ReadFile(dest); string(data) != "root:x:0:0\n" {
		t.Fatalf("unexpected extracted passwd %q", data)
	}
}
