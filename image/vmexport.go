package image

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/gzip"
)

// vmExportDiskSuffixes lists the disk-image filename suffixes extracted out of
// a VM export archive's inner disk section.
var vmExportDiskSuffixes = []string{
	".qcow2", ".qcow", ".qed", ".vmdk", ".vhd", ".vhdx", ".vdi", ".raw", ".img",
}

func isVMExportDiskName(name string) bool {
	n := strings.ToLower(name)
	for _, s := range vmExportDiskSuffixes {
		if strings.HasSuffix(n, s) {
			return true
		}
	}
	return false
}

// vmExportCfgLen parses the "<N>" of the export's "version:1.0\ncfg:<N>\n"
// header, which is the byte length of the metadata gzip section that follows.
func vmExportCfgLen(hdr []byte) (int64, error) {
	s := string(hdr)
	const prefix = "version:1.0\ncfg:"
	if !strings.HasPrefix(s, prefix) {
		return 0, fmt.Errorf("not a VM export archive (unexpected header)")
	}
	var n int64
	for _, c := range s[len(prefix):] {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int64(c-'0')
	}
	if n <= 0 {
		return 0, fmt.Errorf("invalid VM export cfg length")
	}
	return n, nil
}

// findGzipMember returns the offset of the first gzip stream at or after start,
// used to locate the disk section that follows the export's zero padding.
func findGzipMember(f *os.File, start, size int64) (int64, error) {
	const chunk = 1 << 16
	buf := make([]byte, chunk)
	for off := start; off+3 <= size; {
		n := int64(chunk)
		if size-off < n {
			n = size - off
		}
		if _, err := f.ReadAt(buf[:n], off); err != nil && err != io.EOF {
			return 0, err
		}
		for i := int64(0); i+2 < n; i++ {
			if buf[i] == 0x1f && buf[i+1] == 0x8b && buf[i+2] == 0x08 {
				return off + i, nil
			}
		}
		if n < chunk {
			break
		}
		off += n - 2 // overlap so a magic straddling the chunk edge is still seen
	}
	return 0, fmt.Errorf("no disk section found in VM export")
}

// extractVMExportDisks decompresses the export's disk section into destDir and
// returns the disk image paths it produced.
func extractVMExportDisks(path string, destDir string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()

	hdr := make([]byte, 2048)
	if _, err := io.ReadFull(f, hdr); err != nil {
		return nil, fmt.Errorf("read VM export header: %w", err)
	}
	cfgLen, err := vmExportCfgLen(hdr)
	if err != nil {
		return nil, err
	}
	diskOff, err := findGzipMember(f, 2048+cfgLen, size)
	if err != nil {
		return nil, err
	}

	gr, err := gzip.NewReader(io.NewSectionReader(f, diskOff, size-diskOff))
	if err != nil {
		return nil, fmt.Errorf("open VM export disk section: %w", err)
	}
	defer gr.Close()
	tr := tar.NewReader(gr)

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, err
	}

	var diskPaths []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read VM export disk tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			continue
		}
		if !isVMExportDiskName(hdr.Name) {
			continue
		}
		outPath := filepath.Join(destDir, filepath.Base(filepath.Clean(hdr.Name)))
		out, err := os.Create(outPath)
		if err != nil {
			return nil, err
		}
		if _, err := io.Copy(out, tr); err != nil {
			_ = out.Close()
			return nil, fmt.Errorf("extract disk %s: %w", hdr.Name, err)
		}
		if err := out.Close(); err != nil {
			return nil, fmt.Errorf("finalize disk %s: %w", hdr.Name, err)
		}
		diskPaths = append(diskPaths, outPath)
	}

	if len(diskPaths) == 0 {
		return nil, fmt.Errorf("no disk image found in VM export %s", filepath.Base(path))
	}
	return diskPaths, nil
}

// openVMExport opens the disk images from a "version:1.0\ncfg:<N>" VM export: a
// 2048-byte text header, a gzip tar metadata section, then (after zero padding)
// a gzip tar section holding the VM's disks (e.g. a qcow2). The disks are
// decompressed to a temporary directory (removed on Close) and opened through
// the normal disk-container path.
func openVMExport(path string) ([]*diskBackend, func() error, error) {
	dir, err := os.MkdirTemp("", "udf-vmexport-*")
	if err != nil {
		return nil, nil, err
	}

	diskPaths, err := extractVMExportDisks(path, dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, nil, err
	}

	var backs []*diskBackend
	var closes []func() error
	cleanup := func() error {
		var firstErr error
		for i := len(closes) - 1; i >= 0; i-- {
			if err := closes[i](); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		if err := os.RemoveAll(dir); err != nil && firstErr == nil {
			firstErr = err
		}
		return firstErr
	}
	for _, dp := range diskPaths {
		bs, cfn, err := openDisks(dp)
		if err != nil {
			_ = cleanup()
			return nil, nil, fmt.Errorf("open embedded disk %s: %w", filepath.Base(dp), err)
		}
		backs = append(backs, bs...)
		closes = append(closes, cfn)
	}
	return backs, cleanup, nil
}
