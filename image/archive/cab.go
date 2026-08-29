package archive

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ejfkdev/udf/fsview"
)

// Windows Cabinet (.cab) support. A cabinet holds one or more "folders"; each
// folder is a sequence of CFDATA blocks whose decompressed concatenation forms
// a byte stream, and each CFFILE is a slice of that stream. This reader covers
// the uncompressed (typeCompress 0) and MSZIP (typeCompress 1, raw DEFLATE per
// block) folders produced by makecab defaults and embedded in .msi packages.
// LZX and Quantum compression are recognized but not yet decoded.
//
// Magic signature and layout follow [MS-CAB] (https://learn.microsoft.com/openspecs/windows_protocols/ms-cab/).

const cabMagic = "MSCF"

// cabFile mirrors one CFFILE entry.
type cabFile struct {
	name      string
	size      int64 // cbFile
	uoffStart int64 // offset into the folder's uncompressed stream
	folder    int   // iFolder index
	attribs   int16
}

// cabFolder mirrors one CFFOLDER entry.
type cabFolder struct {
	coffCabStart  int64
	cCFData       int
	typeCompress  int16 // 0 none, 1 MSZIP, 2 Quantum, 3 LZX
	continuedPrev bool  // files in this folder continue from a previous cabinet
}

type cabData struct {
	raw       []byte
	files     []cabFile
	folders   []cabFolder
	resFolder int // abReserve bytes after each CFFOLDER
	resData   int // abReserve bytes after each CFDATA
}

// loadCAB reads and parses a .cab into its files, folders and raw bytes.
func loadCAB(path string) (*cabData, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) < 36 || string(raw[:4]) != cabMagic {
		return nil, fmt.Errorf("not a cabinet file")
	}

	flags := binary.LittleEndian.Uint16(raw[30:32])
	cFolders := int(binary.LittleEndian.Uint16(raw[26:28]))
	cFiles := int(binary.LittleEndian.Uint16(raw[28:30]))

	d := &cabData{raw: raw}
	pos := 36
	if flags&0x0004 != 0 { // RESERVE_PRESENT
		if pos+4 > len(raw) {
			return nil, fmt.Errorf("truncated cabinet header")
		}
		cbCFHeader := int(binary.LittleEndian.Uint16(raw[pos : pos+2]))
		d.resFolder = int(raw[pos+2])
		d.resData = int(raw[pos+3])
		pos += 4 + cbCFHeader
	}

	for i := 0; i < cFolders; i++ {
		if pos+8 > len(raw) {
			return nil, fmt.Errorf("truncated CFFOLDER")
		}
		cf := raw[pos : pos+8]
		tc := binary.LittleEndian.Uint16(cf[6:8])
		d.folders = append(d.folders, cabFolder{
			coffCabStart:  int64(binary.LittleEndian.Uint32(cf[0:4])),
			cCFData:       int(binary.LittleEndian.Uint16(cf[4:6])),
			typeCompress:  int16(tc & 0x000F),
			continuedPrev: tc&0x8000 != 0,
		})
		pos += 8 + d.resFolder
	}

	for i := 0; i < cFiles; i++ {
		if pos+16 > len(raw) {
			return nil, fmt.Errorf("truncated CFFILE")
		}
		cf := raw[pos : pos+16]
		size := int64(binary.LittleEndian.Uint32(cf[0:4]))
		uoff := int64(binary.LittleEndian.Uint32(cf[4:8]))
		iFolder := int(binary.LittleEndian.Uint16(cf[8:10]))
		attribs := int16(binary.LittleEndian.Uint16(cf[14:16]))
		pos += 16

		end := bytes.IndexByte(raw[pos:], 0)
		if end < 0 {
			return nil, fmt.Errorf("unterminated CFFILE name")
		}
		name := strings.ReplaceAll(string(raw[pos:pos+end]), "\\", "/")
		pos += end + 1

		d.files = append(d.files, cabFile{
			name:      name,
			size:      size,
			uoffStart: uoff,
			folder:    iFolder,
			attribs:   attribs,
		})
	}
	return d, nil
}

// folderData decompresses one folder to its full uncompressed byte stream.
func (d *cabData) folderData(fi int) ([]byte, error) {
	if fi < 0 || fi >= len(d.folders) {
		return nil, fmt.Errorf("folder index out of range: %d", fi)
	}
	folder := d.folders[fi]
	var out bytes.Buffer

	pos := folder.coffCabStart
	for i := 0; i < folder.cCFData; i++ {
		if pos+8 > int64(len(d.raw)) {
			return nil, fmt.Errorf("truncated CFDATA at %d", pos)
		}
		cbData := int(binary.LittleEndian.Uint16(d.raw[pos+4 : pos+6]))
		cbUncomp := int(binary.LittleEndian.Uint16(d.raw[pos+6 : pos+8]))
		start := pos + 8 + int64(d.resData)
		end := start + int64(cbData)
		if start < 0 || end > int64(len(d.raw)) {
			return nil, fmt.Errorf("CFDATA block out of range at %d", pos)
		}
		block := d.raw[start:end]

		switch folder.typeCompress {
		case 0: // none
			if len(block) != cbUncomp {
				return nil, fmt.Errorf("uncompressed CFDATA size mismatch")
			}
			out.Write(block)
		case 1: // MSZIP: one raw DEFLATE stream per block
			if cbUncomp == 0x8000 {
				return nil, fmt.Errorf("spanned MSZIP CFDATA blocks are not supported")
			}
			fr := flate.NewReader(bytes.NewReader(block))
			if _, err := io.Copy(&out, fr); err != nil {
				_ = fr.Close()
				return nil, fmt.Errorf("decompress MSZIP block: %w", err)
			}
			_ = fr.Close()
		case 2:
			return nil, fmt.Errorf("Quantum compression is not supported")
		case 3:
			return nil, fmt.Errorf("LZX compression is not supported yet")
		default:
			return nil, fmt.Errorf("unknown cabinet compression type %d", folder.typeCompress)
		}
		pos = end
	}
	return out.Bytes(), nil
}

type cabArchive struct {
	path string
}

func (a *cabArchive) List() ([]Entry, error) {
	d, err := loadCAB(a.path)
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(d.files))
	for _, f := range d.files {
		if f.name == "" {
			continue // cabinet-set continuation marker
		}
		kind := fsview.KindFile
		mode := int64(0o644)
		if f.attribs&0x10 != 0 { // directory
			kind = fsview.KindDir
			mode = 0o755
		}
		entries = append(entries, Entry{
			Name: f.name,
			Size: f.size,
			Kind: kind,
			Mode: mode,
		})
	}
	return entries, nil
}

func (a *cabArchive) Open(name string) (io.ReadCloser, int64, error) {
	d, err := loadCAB(a.path)
	if err != nil {
		return nil, 0, err
	}
	for _, f := range d.files {
		if f.name != name {
			continue
		}
		stream, err := d.folderData(f.folder)
		if err != nil {
			return nil, 0, err
		}
		if f.uoffStart < 0 || f.uoffStart+f.size > int64(len(stream)) {
			return nil, 0, fmt.Errorf("file %s extends beyond folder data", name)
		}
		content := stream[f.uoffStart : f.uoffStart+f.size]
		return io.NopCloser(bytes.NewReader(content)), f.size, nil
	}
	return nil, 0, fmt.Errorf("entry %s not found in archive", name)
}
