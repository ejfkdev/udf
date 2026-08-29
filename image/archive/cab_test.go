package archive

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// buildCAB constructs a minimal single-folder cabinet holding the given
// (name, content) pairs, using the requested compression (0 = none, 1 = MSZIP),
// and returns its path. Names use CAB's backslash separator.
func buildCAB(t *testing.T, compression int, files ...[2]string) string {
	t.Helper()

	var folderStream, cfiles []byte
	for i := range files {
		name, content := files[i][0], []byte(files[i][1])
		hdr := make([]byte, 16)
		binary.LittleEndian.PutUint32(hdr[0:4], uint32(len(content)))
		binary.LittleEndian.PutUint32(hdr[4:8], uint32(len(folderStream))) // offset in folder
		binary.LittleEndian.PutUint16(hdr[8:10], 0)                        // iFolder
		binary.LittleEndian.PutUint16(hdr[14:16], 0x20)                    // archive attribute
		cfiles = append(cfiles, hdr...)
		cfiles = append(cfiles, name...)
		cfiles = append(cfiles, 0)
		folderStream = append(folderStream, content...)
	}

	// One CFDATA block covers the whole (small) folder.
	var cbData []byte
	switch compression {
	case 0:
		cbData = folderStream
	case 1:
		var buf bytes.Buffer
		fw, err := flate.NewWriter(&buf, flate.DefaultCompression)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write(folderStream); err != nil {
			t.Fatal(err)
		}
		if err := fw.Close(); err != nil {
			t.Fatal(err)
		}
		cbData = buf.Bytes()
	}
	cfdata := make([]byte, 8, 8+len(cbData))
	binary.LittleEndian.PutUint16(cfdata[4:6], uint16(len(cbData)))
	binary.LittleEndian.PutUint16(cfdata[6:8], uint16(len(folderStream)))
	cfdata = append(cfdata, cbData...)

	coffFiles := 36 + 8 // CFHEADER + CFFOLDER
	coffCabStart := coffFiles + len(cfiles)

	cfolder := make([]byte, 8)
	binary.LittleEndian.PutUint32(cfolder[0:4], uint32(coffCabStart))
	binary.LittleEndian.PutUint16(cfolder[4:6], 1) // cCFData
	binary.LittleEndian.PutUint16(cfolder[6:8], uint16(compression))

	hdr := make([]byte, 36)
	copy(hdr[0:4], "MSCF")
	binary.LittleEndian.PutUint32(hdr[16:20], uint32(coffFiles))
	hdr[24], hdr[25] = 3, 1
	binary.LittleEndian.PutUint16(hdr[26:28], 1)                  // cFolders
	binary.LittleEndian.PutUint16(hdr[28:30], uint16(len(files))) // cFiles

	out := make([]byte, 0, 4096)
	out = append(out, hdr...)
	out = append(out, cfolder...)
	out = append(out, cfiles...)
	out = append(out, cfdata...)
	binary.LittleEndian.PutUint32(out[8:12], uint32(len(out))) // cbCabinet

	path := filepath.Join(t.TempDir(), "test.cab")
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCABListOpen(t *testing.T) {
	for _, tc := range []struct {
		name string
		comp int
	}{
		{"none", 0},
		{"mszip", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := buildCAB(t, tc.comp,
				[2]string{"a.txt", "hello"},
				[2]string{`sub\b.txt`, "world"},
			)
			if f, err := Detect(path); err != nil || f != "cab" {
				t.Fatalf("Detect = %q, %v; want cab", f, err)
			}
			ar, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			entries, err := ar.List()
			if err != nil {
				t.Fatal(err)
			}
			names := map[string]bool{}
			for _, e := range entries {
				names[e.Name] = true
			}
			if !names["a.txt"] || !names["sub/b.txt"] {
				t.Fatalf("listing = %v", names)
			}
			rc, size, err := ar.Open("sub/b.txt")
			if err != nil {
				t.Fatal(err)
			}
			b, err := io.ReadAll(rc)
			rc.Close()
			if err != nil || string(b) != "world" || size != 5 {
				t.Fatalf("sub/b.txt = %q (size %d, err %v)", b, size, err)
			}
		})
	}
}
