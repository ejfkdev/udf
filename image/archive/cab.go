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
	src       io.ReaderAt // the cabinet's byte space (file, or an embedded stream)
	size      int64
	files     []cabFile
	folders   []cabFolder
	resFolder int // abReserve bytes after each CFFOLDER
	resData   int // abReserve bytes after each CFDATA
}

// loadCAB reads and parses a .cab file. Only the header region (folders and
// file entries) is read; CFDATA blocks are fetched on demand.
func loadCAB(path string) (*cabData, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	d, err := parseCabFrom(f, st.Size())
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return d, nil
}

// parseCabBytes parses a cabinet from its raw bytes.
func parseCabBytes(raw []byte) (*cabData, error) {
	return parseCabFrom(bytes.NewReader(raw), int64(len(raw)))
}

// parseCabFrom parses a cabinet from any random-access source, reading only
// the header region (CFHEADER, CFFOLDERs, CFFILEs) and never the CFDATA
// blocks — those are fetched on demand from src, so a listing of an MSI's
// embedded cabinets costs kilobytes regardless of payload size.
func parseCabFrom(src io.ReaderAt, size int64) (*cabData, error) {
	if size < 36 {
		return nil, fmt.Errorf("not a cabinet file")
	}
	head := make([]byte, 36)
	if _, err := src.ReadAt(head, 0); err != nil {
		return nil, err
	}
	if string(head[0:4]) != cabMagic {
		return nil, fmt.Errorf("not a cabinet file")
	}

	flags := binary.LittleEndian.Uint16(head[30:32])
	cFolders := int(binary.LittleEndian.Uint16(head[26:28]))
	cFiles := int(binary.LittleEndian.Uint16(head[28:30]))

	d := &cabData{src: src, size: size}
	pos := int64(36)
	if flags&0x0004 != 0 { // RESERVE_PRESENT
		if pos+4 > size {
			return nil, fmt.Errorf("truncated cabinet header")
		}
		var rsv [4]byte
		if _, err := src.ReadAt(rsv[:], pos); err != nil {
			return nil, err
		}
		cbCFHeader := int(binary.LittleEndian.Uint16(rsv[0:2]))
		d.resFolder = int(rsv[2])
		d.resData = int(rsv[3])
		pos += 4 + int64(cbCFHeader)
	}

	// Folders and file entries follow; file entry names have variable length,
	// so read through them in growing windows.
	const window = 64 << 10
	bufStart, bufEnd := pos, pos
	buf := make([]byte, 0, window)
	readMore := func(end int64) error {
		if end <= bufEnd {
			return nil
		}
		if end > size {
			end = size
		}
		chunk := make([]byte, end-bufEnd)
		if _, err := src.ReadAt(chunk, bufEnd); err != nil {
			return err
		}
		buf = append(buf, chunk...)
		bufEnd = end
		return nil
	}

	foldersLeft := cFolders
	for i := 0; i < foldersLeft; i++ {
		if pos+8+int64(d.resFolder) > size {
			return nil, fmt.Errorf("truncated CFFOLDER")
		}
		if err := readMore(pos + 8 + int64(d.resFolder)); err != nil {
			return nil, err
		}
		cf := buf[pos-bufStart:]
		tc := binary.LittleEndian.Uint16(cf[6:8])
		d.folders = append(d.folders, cabFolder{
			coffCabStart:  int64(binary.LittleEndian.Uint32(cf[0:4])),
			cCFData:       int(binary.LittleEndian.Uint16(cf[4:6])),
			typeCompress:  int16(tc & 0x000F),
			continuedPrev: tc&0x8000 != 0,
		})
		pos += 8 + int64(d.resFolder)
	}

	filesLeft := cFiles
	for filesLeft > 0 {
		if err := readMore(pos + 16); err != nil {
			return nil, err
		}
		if pos+16 > bufEnd {
			return nil, fmt.Errorf("truncated CFFILE")
		}
		cf := buf[pos-bufStart:]
		size := int64(binary.LittleEndian.Uint32(cf[0:4]))
		uoff := int64(binary.LittleEndian.Uint32(cf[4:8]))
		iFolder := int(binary.LittleEndian.Uint16(cf[8:10]))
		attribs := int16(binary.LittleEndian.Uint16(cf[14:16]))
		pos += 16

		// The NUL-terminated name follows; make sure it is inside the window.
		for {
			if pos >= bufEnd {
				if err := readMore(bufEnd + window); err != nil {
					return nil, err
				}
				continue
			}
			if bytes.IndexByte(buf[pos-bufStart:bufEnd-bufStart], 0) >= 0 {
				break
			}
			if err := readMore(bufEnd + window); err != nil {
				return nil, err
			}
		}
		end := bytes.IndexByte(buf[pos-bufStart:bufEnd-bufStart], 0)
		name := strings.ReplaceAll(string(buf[pos-bufStart:pos-bufStart+int64(end)]), "\\", "/")
		pos += int64(end) + 1

		d.files = append(d.files, cabFile{
			name:      name,
			size:      size,
			uoffStart: uoff,
			folder:    iFolder,
			attribs:   attribs,
		})
		filesLeft--
	}
	return d, nil
}

// folderData decompresses one folder to its full uncompressed byte stream.
func (d *cabData) folderData(fi int) ([]byte, error) {
	total, err := d.folderUncompressedSize(fi)
	if err != nil {
		return nil, err
	}
	return d.folderRange(fi, 0, total)
}

// folderUncompressedSize walks the folder's CFDATA headers (reading only the
// 8-byte headers, not the payload) to total its uncompressed size.
func (d *cabData) folderUncompressedSize(fi int) (int64, error) {
	if fi < 0 || fi >= len(d.folders) {
		return 0, fmt.Errorf("folder index out of range: %d", fi)
	}
	folder := d.folders[fi]
	var total int64
	pos := folder.coffCabStart
	for i := 0; i < folder.cCFData; i++ {
		hdr, err := d.readDataHeader(pos)
		if err != nil {
			return 0, err
		}
		total += int64(hdr.uncomp)
		pos = hdr.end
	}
	return total, nil
}

// cabDataHeader is one CFDATA block's header.
type cabDataHeader struct {
	comp, uncomp int
	start, end   int64 // file-space range of the compressed payload
}

func (d *cabData) readDataHeader(pos int64) (cabDataHeader, error) {
	var hdr [8]byte
	if pos+8+int64(d.resData) > d.size {
		return cabDataHeader{}, fmt.Errorf("truncated CFDATA at %d", pos)
	}
	if _, err := d.src.ReadAt(hdr[:], pos); err != nil {
		return cabDataHeader{}, err
	}
	comp := int(binary.LittleEndian.Uint16(hdr[4:6]))
	uncomp := int(binary.LittleEndian.Uint16(hdr[6:8]))
	start := pos + 8 + int64(d.resData)
	return cabDataHeader{comp: comp, uncomp: uncomp, start: start, end: start + int64(comp)}, nil
}

// folderRange returns the folder's uncompressed bytes for [start, start+length),
// decompressing only the CFDATA blocks that intersect the range. MSZIP blocks
// are independent deflate streams, so blocks outside the range cost nothing
// but an 8-byte header read each.
func (d *cabData) folderRange(fi int, start, length int64) ([]byte, error) {
	if fi < 0 || fi >= len(d.folders) {
		return nil, fmt.Errorf("folder index out of range: %d", fi)
	}
	folder := d.folders[fi]
	if start < 0 || length < 0 {
		return nil, fmt.Errorf("negative folder range")
	}

	endWanted := start + length
	out := make([]byte, 0, length)
	var produced int64
	pos := folder.coffCabStart
	for i := 0; i < folder.cCFData && produced < endWanted; i++ {
		hdr, err := d.readDataHeader(pos)
		if err != nil {
			return nil, err
		}
		blockEnd := produced + int64(hdr.uncomp)
		pos = hdr.end

		if blockEnd <= start {
			produced = blockEnd
			continue // entirely before the window: header only
		}

		block := make([]byte, hdr.comp)
		if _, err := d.src.ReadAt(block, hdr.start); err != nil {
			return nil, fmt.Errorf("read CFDATA at %d: %w", hdr.start, err)
		}
		decoded, err := d.decodeBlock(folder, hdr, block)
		if err != nil {
			return nil, err
		}
		if int64(len(decoded)) != int64(hdr.uncomp) {
			return nil, fmt.Errorf("CFDATA block decoded %d bytes, want %d", len(decoded), hdr.uncomp)
		}

		// Take the part of this block inside the window.
		lo := start - produced
		if lo < 0 {
			lo = 0
		}
		hi := endWanted - produced
		if hi > int64(len(decoded)) {
			hi = int64(len(decoded))
		}
		if hi > lo {
			out = append(out, decoded[lo:hi]...)
		}
		produced = blockEnd
	}
	if int64(len(out)) != length {
		return nil, fmt.Errorf("folder data ended after %d bytes, want %d", len(out), length)
	}
	return out, nil
}

// decodeBlock decompresses one CFDATA block per the folder's codec.
func (d *cabData) decodeBlock(folder cabFolder, hdr cabDataHeader, block []byte) ([]byte, error) {
	switch folder.typeCompress {
	case 0: // none
		return block, nil
	case 1: // MSZIP
		if hdr.uncomp == 0x8000 && hdr.comp == 0x8000 &&
			!(len(block) >= 2 && block[0] == 'C' && block[1] == 'K') {
			// A full-size chunk without the MSZIP signature is stored raw
			// (the compressor gave up on incompressible data).
			return block, nil
		}
		if len(block) >= 2 && block[0] == 'C' && block[1] == 'K' {
			// Every MSZIP block starts with the 0x43 0x4B signature, then
			// the deflate stream ([MS-MCIA] 2.2).
			block = block[2:]
		}
		fr := flate.NewReader(bytes.NewReader(block))
		defer fr.Close()
		return io.ReadAll(fr)
	case 2:
		return nil, fmt.Errorf("Quantum compression is not supported")
	case 3:
		return nil, fmt.Errorf("LZX compression is not supported yet")
	default:
		return nil, fmt.Errorf("unknown cabinet compression type %d", folder.typeCompress)
	}
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
