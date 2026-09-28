package archive

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/pierrec/lz4/v4"
	"github.com/ulikunitz/xz/lzma"
)

// unityFixture describes a bundle to build: the node contents plus the
// compression to use for the block information and for the data blocks.
type unityFixture struct {
	nodeNames []string
	nodeData  [][]byte
	infoComp  uint32
	blockComp uint32
}

// writeUnityBundle assembles a UnityFS bundle byte by byte, big-endian like the
// container is, so the fixture is an independent check of the reader.
func writeUnityBundle(t *testing.T, fx unityFixture) string {
	t.Helper()

	var stream bytes.Buffer
	offsets := make([]int64, len(fx.nodeData))
	for i, d := range fx.nodeData {
		offsets[i] = int64(stream.Len())
		stream.Write(d)
	}

	// Block information: hash, one block, then the nodes.
	var info bytes.Buffer
	info.Write(make([]byte, 16))
	be32 := func(v uint32) { binary.Write(&info, binary.BigEndian, v) }
	be64 := func(v int64) { binary.Write(&info, binary.BigEndian, v) }
	be32(1) // one block
	be32(uint32(stream.Len()))
	be32(uint32(stream.Len())) // compressed size (patched below)
	binary.Write(&info, binary.BigEndian, uint16(fx.blockComp))
	be32(uint32(len(fx.nodeNames)))
	for i, name := range fx.nodeNames {
		be64(offsets[i])
		be64(int64(len(fx.nodeData[i])))
		be32(0)
		info.WriteString(name)
		info.WriteByte(0)
	}

	blockData := compressUnity(t, stream.Bytes(), fx.blockComp)
	// Patch the block's compressed size now that it is known.
	binary.BigEndian.PutUint32(info.Bytes()[24:28], uint32(len(blockData)))
	infoBytes := compressUnity(t, info.Bytes(), fx.infoComp)

	var out bytes.Buffer
	out.WriteString(unityFSMagic)
	binary.Write(&out, binary.BigEndian, uint32(6)) // version
	out.WriteString("5.x.x\x00")
	out.WriteString("2019.4.15f1\x00") // engine: aligned, new flag set
	// size is patched below; header fields: size, info comp, info unc, flags
	sizeAt := out.Len()
	binary.Write(&out, binary.BigEndian, uint32(0))
	binary.Write(&out, binary.BigEndian, uint32(0))
	binary.Write(&out, binary.BigEndian, uint32(len(infoBytes)))
	binary.Write(&out, binary.BigEndian, uint32(len(info.Bytes())))
	binary.Write(&out, binary.BigEndian, uint32(0x40|fx.infoComp)) // combined, not at end
	for out.Len()%16 != 0 {
		out.WriteByte(0)
	}
	// The data blocks follow the information directly: only the 0x200 flag
	// asks for padding, and this fixture leaves it unset.
	out.Write(infoBytes)
	out.Write(blockData)

	full := out.Bytes()
	binary.BigEndian.PutUint64(full[sizeAt:], uint64(len(full)))

	path := filepath.Join(t.TempDir(), "bundle.ab")
	if err := os.WriteFile(path, full, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// compressUnity compresses one blob the way a bundle would.
func compressUnity(t *testing.T, data []byte, compression uint32) []byte {
	t.Helper()
	switch compression {
	case unityCompressionNone:
		return data
	case unityCompressionLZ4, unityCompressionLZ4HC:
		out := make([]byte, lz4.CompressBlockBound(len(data)))
		n, err := lz4.CompressBlock(data, out, nil)
		if err != nil {
			t.Fatalf("lz4 compress: %v", err)
		}
		if n == 0 { // incompressible: the format would store it
			return data
		}
		return out[:n]
	case unityCompressionLZMA:
		var buf bytes.Buffer
		w, err := lzma.NewWriter(&buf)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		// Unity stores the five properties bytes and the raw stream.
		full := buf.Bytes()
		return append(append([]byte{}, full[:5]...), full[13:]...)
	}
	t.Fatalf("unhandled compression %d", compression)
	return nil
}

func TestUnityFSReadsBlocksAndNodes(t *testing.T) {
	cab := bytes.Repeat([]byte("serialized asset payload "), 500)
	res := bytes.Repeat([]byte{0xAB, 0xCD, 0xEF, 0x01}, 2000)
	for _, tc := range []struct {
		name      string
		infoComp  uint32
		blockComp uint32
	}{
		{"stored", unityCompressionNone, unityCompressionNone},
		{"lz4", unityCompressionLZ4, unityCompressionLZ4},
		{"lzma", unityCompressionLZMA, unityCompressionLZMA},
		{"mixed", unityCompressionLZ4, unityCompressionNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeUnityBundle(t, unityFixture{
				nodeNames: []string{"CAB-1234", "CAB-1234.resource"},
				nodeData:  [][]byte{cab, res},
				infoComp:  tc.infoComp,
				blockComp: tc.blockComp,
			})
			format, err := Detect(path)
			if err != nil || format != "unity" {
				t.Fatalf("Detect = %q, %v", format, err)
			}
			a, err := Open(path)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			entries, err := a.List()
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 2 || entries[0].Name != "CAB-1234" || entries[0].Size != int64(len(cab)) {
				t.Fatalf("unexpected listing: %+v", entries)
			}
			for i, want := range [][]byte{cab, res} {
				rc, size, err := a.Open(entries[i].Name)
				if err != nil {
					t.Fatalf("open %s: %v", entries[i].Name, err)
				}
				got, _ := io.ReadAll(rc)
				rc.Close()
				if size != int64(len(want)) || !bytes.Equal(got, want) {
					t.Fatalf("%s: got %d bytes, want %d (equal=%v)", entries[i].Name, len(got), len(want), bytes.Equal(got, want))
				}
			}
		})
	}
}

// TestUnityFSRealBundle reads a real asset bundle when UDF_UNITY points at one.
func TestUnityFSRealBundle(t *testing.T) {
	path := os.Getenv("UDF_UNITY")
	if path == "" {
		t.Skip("set UDF_UNITY to a UnityFS .bundle/.ab to read a real bundle")
	}
	a, err := openUnityFS(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	entries, err := a.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatalf("bundle lists no files")
	}
	t.Logf("unity %s (%s): %d nodes, %d bytes of data", a.player, a.engine, len(entries), a.total)
	for _, e := range entries[:min(len(entries), 8)] {
		t.Logf("  %s  %d bytes (flags resolved at extraction)", e.Name, e.Size)
	}
	// Every node must extract to exactly its declared size.
	for _, e := range entries {
		rc, size, err := a.Open(e.Name)
		if err != nil {
			t.Fatalf("open %s: %v", e.Name, err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil || int64(len(data)) != size {
			t.Fatalf("%s: read %d bytes of %d (%v)", e.Name, len(data), size, err)
		}
	}
	t.Logf("all %d nodes extracted at their declared sizes", len(entries))
}
