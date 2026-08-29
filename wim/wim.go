// Package wim implements a WIM file parser.
//
// WIM files are used to distribute Windows file system and container images.
// They are documented at https://msdn.microsoft.com/en-us/library/windows/desktop/dd861280.aspx.
package wim

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // not used for secure application
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"
	"unicode/utf16"
)

// File attribute constants from Windows.
//
//nolint:revive // var-naming: ALL_CAPS
const (
	FILE_ATTRIBUTE_READONLY            = 0x00000001
	FILE_ATTRIBUTE_HIDDEN              = 0x00000002
	FILE_ATTRIBUTE_SYSTEM              = 0x00000004
	FILE_ATTRIBUTE_DIRECTORY           = 0x00000010
	FILE_ATTRIBUTE_ARCHIVE             = 0x00000020
	FILE_ATTRIBUTE_DEVICE              = 0x00000040
	FILE_ATTRIBUTE_NORMAL              = 0x00000080
	FILE_ATTRIBUTE_TEMPORARY           = 0x00000100
	FILE_ATTRIBUTE_SPARSE_FILE         = 0x00000200
	FILE_ATTRIBUTE_REPARSE_POINT       = 0x00000400
	FILE_ATTRIBUTE_COMPRESSED          = 0x00000800
	FILE_ATTRIBUTE_OFFLINE             = 0x00001000
	FILE_ATTRIBUTE_NOT_CONTENT_INDEXED = 0x00002000
	FILE_ATTRIBUTE_ENCRYPTED           = 0x00004000
	FILE_ATTRIBUTE_INTEGRITY_STREAM    = 0x00008000
	FILE_ATTRIBUTE_VIRTUAL             = 0x00010000
	FILE_ATTRIBUTE_NO_SCRUB_DATA       = 0x00020000
	FILE_ATTRIBUTE_EA                  = 0x00040000
)

// Windows processor architectures.
//
//nolint:revive // var-naming: ALL_CAPS
const (
	PROCESSOR_ARCHITECTURE_INTEL         = 0
	PROCESSOR_ARCHITECTURE_MIPS          = 1
	PROCESSOR_ARCHITECTURE_ALPHA         = 2
	PROCESSOR_ARCHITECTURE_PPC           = 3
	PROCESSOR_ARCHITECTURE_SHX           = 4
	PROCESSOR_ARCHITECTURE_ARM           = 5
	PROCESSOR_ARCHITECTURE_IA64          = 6
	PROCESSOR_ARCHITECTURE_ALPHA64       = 7
	PROCESSOR_ARCHITECTURE_MSIL          = 8
	PROCESSOR_ARCHITECTURE_AMD64         = 9
	PROCESSOR_ARCHITECTURE_IA32_ON_WIN64 = 10
	PROCESSOR_ARCHITECTURE_NEUTRAL       = 11
	PROCESSOR_ARCHITECTURE_ARM64         = 12
)

var wimImageTag = [...]byte{'M', 'S', 'W', 'I', 'M', 0, 0, 0}

// todo: replace this with pkg/guid.GUID (and add tests to make sure nothing breaks)

type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

func (g guid) String() string {
	return fmt.Sprintf("%08x-%04x-%04x-%02x%02x-%02x%02x%02x%02x%02x%02x",
		g.Data1,
		g.Data2,
		g.Data3,
		g.Data4[0],
		g.Data4[1],
		g.Data4[2],
		g.Data4[3],
		g.Data4[4],
		g.Data4[5],
		g.Data4[6],
		g.Data4[7])
}

type resourceDescriptor struct {
	FlagsAndCompressedSize uint64
	Offset                 int64
	OriginalSize           int64
}

type resFlag byte

//nolint:deadcode,varcheck // need unused variables for iota to work
const (
	resFlagFree resFlag = 1 << iota
	resFlagMetadata
	resFlagCompressed
	resFlagSpanned
)

const validate = false

const supportedResFlags = resFlagMetadata | resFlagCompressed

func (r *resourceDescriptor) Flags() resFlag {
	return resFlag(r.FlagsAndCompressedSize >> 56)
}

func (r *resourceDescriptor) CompressedSize() int64 {
	return int64(r.FlagsAndCompressedSize & 0xffffffffffffff)
}

func (r *resourceDescriptor) String() string {
	s := fmt.Sprintf("%d bytes at %d", r.CompressedSize(), r.Offset)
	if r.Flags()&4 != 0 {
		s += fmt.Sprintf(" (uncompresses to %d)", r.OriginalSize)
	}
	return s
}

// SHA1Hash contains the SHA1 hash of a file or stream.
type SHA1Hash [20]byte

type streamDescriptor struct {
	resourceDescriptor
	PartNumber uint16
	RefCount   uint32
	Hash       SHA1Hash
}

type hdrFlag uint32

//nolint:deadcode,varcheck // need unused variables for iota to work
const (
	hdrFlagReserved hdrFlag = 1 << iota
	hdrFlagCompressed
	hdrFlagReadOnly
	hdrFlagSpanned
	hdrFlagResourceOnly
	hdrFlagMetadataOnly
	hdrFlagWriteInProgress
	hdrFlagRpFix
)

//nolint:deadcode,varcheck // need unused variables for iota to work
const (
	hdrFlagCompressReserved hdrFlag = 1 << (iota + 16)
	hdrFlagCompressXpress
	hdrFlagCompressLzx
	hdrFlagCompressLzms
)

// The low header-flag bits (read-only, spanned, resource-only, reparse-point
// fixup, etc.) are all informational for reading and appear together in split
// WIMs; the only bits we reject are compression types we cannot decode
// (reserved, XPRESS).
const informationalHdrFlags = hdrFlagReserved | hdrFlagCompressed | hdrFlagReadOnly |
	hdrFlagSpanned | hdrFlagResourceOnly | hdrFlagMetadataOnly |
	hdrFlagWriteInProgress | hdrFlagRpFix

const supportedHdrFlags = informationalHdrFlags | hdrFlagCompressXpress | hdrFlagCompressLzx | hdrFlagCompressLzms

type wimHeader struct {
	ImageTag        [8]byte
	Size            uint32
	Version         uint32
	Flags           hdrFlag
	CompressionSize uint32
	WIMGuid         guid
	PartNumber      uint16
	TotalParts      uint16
	ImageCount      uint32
	OffsetTable     resourceDescriptor
	XMLData         resourceDescriptor
	BootMetadata    resourceDescriptor
	BootIndex       uint32
	Padding         uint32
	Integrity       resourceDescriptor
	Unused          [60]byte
}

type securityblockDisk struct {
	TotalLength uint32
	NumEntries  uint32
}

const securityblockDiskSize = 8

type direntry struct {
	Attributes       uint32
	SecurityID       uint32
	SubdirOffset     int64
	Unused1, Unused2 int64
	CreationTime     Filetime
	LastAccessTime   Filetime
	LastWriteTime    Filetime
	Hash             SHA1Hash
	Padding          uint32
	ReparseHardLink  int64
	StreamCount      uint16
	ShortNameLength  uint16
	FileNameLength   uint16
}

var direntrySize = int64(binary.Size(direntry{}) + 8) // includes an 8-byte length prefix

type streamentry struct {
	Unused     int64
	Hash       SHA1Hash
	NameLength int16
}

var streamentrySize = int64(binary.Size(streamentry{}) + 8) // includes an 8-byte length prefix

// Filetime represents a Windows time.
type Filetime struct {
	LowDateTime  uint32
	HighDateTime uint32
}

// Time returns the time as time.Time.
func (ft *Filetime) Time() time.Time {
	// 100-nanosecond intervals since January 1, 1601
	nsec := int64(ft.HighDateTime)<<32 + int64(ft.LowDateTime)
	// change starting time to the Epoch (00:00:00 UTC, January 1, 1970)
	nsec -= 116444736000000000
	// convert into nanoseconds
	nsec *= 100
	return time.Unix(0, nsec)
}

// UnmarshalXML unmarshalls the time from a WIM XML blob.
func (ft *Filetime) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	type Time struct {
		Low  string `xml:"LOWPART"`
		High string `xml:"HIGHPART"`
	}
	var t Time
	err := d.DecodeElement(&t, &start)
	if err != nil {
		return err
	}

	low, err := strconv.ParseUint(t.Low, 0, 32)
	if err != nil {
		return err
	}
	high, err := strconv.ParseUint(t.High, 0, 32)
	if err != nil {
		return err
	}

	ft.LowDateTime = uint32(low)
	ft.HighDateTime = uint32(high)
	return nil
}

type info struct {
	Image []ImageInfo `xml:"IMAGE"`
}

// ImageInfo contains information about the image.
type ImageInfo struct {
	Name         string       `xml:"NAME"`
	Index        int          `xml:"INDEX,attr"`
	CreationTime Filetime     `xml:"CREATIONTIME"`
	ModTime      Filetime     `xml:"LASTMODIFICATIONTIME"`
	Windows      *WindowsInfo `xml:"WINDOWS"`
}

// WindowsInfo contains information about the Windows installation in the image.
type WindowsInfo struct {
	Arch             byte     `xml:"ARCH"`
	ProductName      string   `xml:"PRODUCTNAME"`
	EditionID        string   `xml:"EDITIONID"`
	InstallationType string   `xml:"INSTALLATIONTYPE"`
	ProductType      string   `xml:"PRODUCTTYPE"`
	Languages        []string `xml:"LANGUAGES>LANGUAGE"`
	DefaultLanguage  string   `xml:"LANGUAGES>DEFAULT"`
	Version          Version  `xml:"VERSION"`
	SystemRoot       string   `xml:"SYSTEMROOT"`
}

// Version represents a Windows build version.
type Version struct {
	Major   int `xml:"MAJOR"`
	Minor   int `xml:"MINOR"`
	Build   int `xml:"BUILD"`
	SPBuild int `xml:"SPBUILD"`
	SPLevel int `xml:"SPLEVEL"`
}

// ParseError is returned when the WIM cannot be parsed.
type ParseError struct {
	Oper string
	Path string
	Err  error
}

func (e *ParseError) Error() string {
	if e.Path == "" {
		return "WIM parse error at " + e.Oper + ": " + e.Err.Error()
	}
	return fmt.Sprintf("WIM parse error: %s %s: %s", e.Oper, e.Path, e.Err.Error())
}

func (e *ParseError) Unwrap() error { return e.Err }

// Reader provides functions to read a WIM file, including split (multi-part)
// WIMs where each part is a separate file.
type Reader struct {
	hdr      wimHeader
	r        io.ReaderAt // first part; parts[0]
	parts    []io.ReaderAt
	fileData map[SHA1Hash]streamDescriptor

	XMLInfo string   // The XML information about the WIM.
	Image   []*Image // The WIM's images.
}

// Image represents an image within a WIM file.
type Image struct {
	wim        *Reader
	offset     resourceDescriptor
	sds        [][]byte
	rootOffset int64
	r          io.ReadCloser
	curOffset  int64
	m          sync.Mutex

	ImageInfo
}

// StreamHeader contains alternate data stream metadata.
type StreamHeader struct {
	Name string
	Hash SHA1Hash
	Size int64
}

// Stream represents an alternate data stream or reparse point data stream.
type Stream struct {
	StreamHeader
	wim        *Reader
	offset     resourceDescriptor
	partNumber uint16
}

// FileHeader contains file metadata.
type FileHeader struct {
	Name               string
	ShortName          string
	Attributes         uint32
	SecurityDescriptor []byte
	CreationTime       Filetime
	LastAccessTime     Filetime
	LastWriteTime      Filetime
	Hash               SHA1Hash
	Size               int64
	LinkID             int64
	ReparseTag         uint32
	ReparseReserved    uint32
}

// File represents a file or directory in a WIM image.
type File struct {
	FileHeader
	Streams      []*Stream
	offset       resourceDescriptor
	partNumber   uint16
	img          *Image
	subdirOffset int64
}

// NewReader returns a Reader for a single-part WIM file.
func NewReader(f io.ReaderAt) (*Reader, error) {
	return NewReaderParts([]io.ReaderAt{f})
}

// NewReaderParts returns a Reader for a WIM that may consist of multiple parts
// (a split WIM). parts[0] is part 1; subsequent entries are the continuation
// parts in order.
func NewReaderParts(parts []io.ReaderAt) (*Reader, error) {
	if len(parts) == 0 {
		return nil, errors.New("no WIM parts")
	}
	r := &Reader{r: parts[0], parts: parts}
	section := io.NewSectionReader(parts[0], 0, 0xffff)
	err := binary.Read(section, binary.LittleEndian, &r.hdr)
	if err != nil {
		return nil, err
	}

	if r.hdr.ImageTag != wimImageTag {
		return nil, &ParseError{Oper: "image tag", Err: errors.New("not a WIM file")}
	}

	if r.hdr.Flags&^supportedHdrFlags != 0 {
		return nil, fmt.Errorf("unsupported WIM flags %x", r.hdr.Flags&^supportedHdrFlags)
	}

	// The chunk size may be 0 (uncompressed WIMs, e.g. 7-Zip's "Copy" method) or
	// a power of two in [32768, 64 MiB]; XPRESS/LZX use 32768, LZMS/ESD commonly
	// uses larger chunks.
	if cs := r.hdr.CompressionSize; cs != 0 {
		if cs < 0x8000 || cs > 0x4000000 || cs&(cs-1) != 0 {
			return nil, fmt.Errorf("unsupported compression size %d", cs)
		}
	}

	fileData, images, err := r.readOffsetTable(&r.hdr.OffsetTable)
	if err != nil {
		return nil, err
	}

	xmlinfo, err := r.readXML()
	if err != nil {
		return nil, err
	}

	var inf info
	err = xml.Unmarshal([]byte(xmlinfo), &inf)
	if err != nil {
		return nil, &ParseError{Oper: "XML info", Err: err}
	}

	for i, img := range images {
		for _, imgInfo := range inf.Image {
			if imgInfo.Index == i+1 {
				img.ImageInfo = imgInfo
				break
			}
		}
	}

	r.fileData = fileData
	r.Image = images
	r.XMLInfo = xmlinfo
	return r, nil
}

// Close releases resources associated with the Reader.
func (r *Reader) Close() error {
	for _, img := range r.Image {
		img.reset()
	}
	return nil
}

// partReaderAt returns the reader for a 1-based part number, defaulting to the
// first part when the number is out of range (metdata resources reference part
// 1, which some images leave as 0).
func (r *Reader) partReaderAt(partNumber uint16) io.ReaderAt {
	if partNumber == 0 || int(partNumber-1) >= len(r.parts) {
		return r.r
	}
	return r.parts[partNumber-1]
}

func (r *Reader) resourceReader(hdr *resourceDescriptor) (io.ReadCloser, error) {
	return r.resourceReaderWithOffset(r.r, hdr, 0)
}

func (r *Reader) resourceReaderAt(partNumber uint16, hdr *resourceDescriptor, offset int64) (io.ReadCloser, error) {
	return r.resourceReaderWithOffset(r.partReaderAt(partNumber), hdr, offset)
}

func (r *Reader) resourceReaderWithOffset(ra io.ReaderAt, hdr *resourceDescriptor, offset int64) (io.ReadCloser, error) {
	var sr io.ReadCloser
	section := io.NewSectionReader(ra, hdr.Offset, hdr.CompressedSize())
	if hdr.Flags()&resFlagCompressed == 0 {
		_, _ = section.Seek(offset, 0)
		sr = io.NopCloser(section)
	} else {
		cr, err := newCompressedReader(section, hdr.OriginalSize, offset, r.hdr.Flags&(hdrFlagCompressXpress|hdrFlagCompressLzx|hdrFlagCompressLzms), int64(r.hdr.CompressionSize))
		if err != nil {
			return nil, err
		}
		sr = cr
	}

	return sr, nil
}

func (r *Reader) readResource(hdr *resourceDescriptor) ([]byte, error) {
	rsrc, err := r.resourceReader(hdr)
	if err != nil {
		return nil, err
	}
	defer rsrc.Close()
	return io.ReadAll(rsrc)
}

func (r *Reader) readXML() (string, error) {
	if r.hdr.XMLData.CompressedSize() == 0 {
		return "", nil
	}
	rsrc, err := r.resourceReader(&r.hdr.XMLData)
	if err != nil {
		return "", err
	}
	defer rsrc.Close()

	xmlData := make([]uint16, r.hdr.XMLData.OriginalSize/2)
	err = binary.Read(rsrc, binary.LittleEndian, xmlData)
	if err != nil {
		return "", &ParseError{Oper: "XML data", Err: err}
	}

	// The BOM will always indicate little-endian UTF-16.
	if xmlData[0] != 0xfeff {
		return "", &ParseError{Oper: "XML data", Err: errors.New("invalid BOM")}
	}
	return string(utf16.Decode(xmlData[1:])), nil
}

func (r *Reader) readOffsetTable(res *resourceDescriptor) (map[SHA1Hash]streamDescriptor, []*Image, error) {
	fileData := make(map[SHA1Hash]streamDescriptor)
	var images []*Image

	// Part 1's lookup table, then each continuation part's own lookup table.
	if err := r.mergeOffsetTable(r.parts[0], res, fileData, &images, 0); err != nil {
		return nil, nil, &ParseError{Oper: "offset table", Err: err}
	}
	for i := 1; i < len(r.parts); i++ {
		var phdr wimHeader
		if err := binary.Read(io.NewSectionReader(r.parts[i], 0, 0xffff), binary.LittleEndian, &phdr); err != nil {
			return nil, nil, &ParseError{Oper: "offset table", Err: err}
		}
		if err := r.mergeOffsetTable(r.parts[i], &phdr.OffsetTable, fileData, &images, i); err != nil {
			return nil, nil, &ParseError{Oper: "offset table", Err: err}
		}
	}

	if len(images) != int(r.hdr.ImageCount) {
		return nil, nil, &ParseError{Oper: "offset table", Err: errors.New("mismatched image count")}
	}

	return fileData, images, nil
}

// mergeOffsetTable reads one part's lookup table and merges its stream entries.
// A resource's data lives in the part whose lookup table lists it, so the part
// number is derived from the part being read.
func (r *Reader) mergeOffsetTable(ra io.ReaderAt, res *resourceDescriptor, fileData map[SHA1Hash]streamDescriptor, images *[]*Image, partIndex int) error {
	rsrc, err := r.resourceReaderWithOffset(ra, res, 0)
	if err != nil {
		return err
	}
	defer rsrc.Close()

	table, err := io.ReadAll(rsrc)
	if err != nil {
		return err
	}

	br := bytes.NewReader(table)
	for {
		var res streamDescriptor
		err := binary.Read(br, binary.LittleEndian, &res)
		if err == io.EOF { //nolint:errorlint
			break
		}
		if err != nil {
			return err
		}
		if res.Flags()&^supportedResFlags != 0 {
			return errors.New("unsupported resource flag")
		}
		res.PartNumber = uint16(partIndex + 1)

		// Validation for ad-hoc testing
		if validate {
			sec, err := r.resourceReaderAt(res.PartNumber, &res.resourceDescriptor, 0)
			if err != nil {
				panic(fmt.Sprint(err))
			}
			hash := sha1.New() //nolint:gosec // not used for secure application
			_, err = io.Copy(hash, sec)
			sec.Close()
			if err != nil {
				panic(fmt.Sprint(err))
			}
			var cmphash SHA1Hash
			copy(cmphash[:], hash.Sum(nil))
			if cmphash != res.Hash {
				panic("hash mismatch")
			}
		}

		if res.Flags()&resFlagMetadata != 0 {
			*images = append(*images, &Image{
				wim:    r,
				offset: res.resourceDescriptor,
			})
		} else {
			fileData[res.Hash] = res
		}
	}
	return nil
}

func (*Reader) readSecurityDescriptors(rsrc io.Reader) (sds [][]byte, n int64, err error) {
	var secBlock securityblockDisk
	err = binary.Read(rsrc, binary.LittleEndian, &secBlock)
	if err != nil {
		return sds, 0, &ParseError{Oper: "security table", Err: err}
	}

	n += securityblockDiskSize

	secSizes := make([]int64, secBlock.NumEntries)
	err = binary.Read(rsrc, binary.LittleEndian, &secSizes)
	if err != nil {
		return sds, n, &ParseError{Oper: "security table sizes", Err: err}
	}

	n += int64(secBlock.NumEntries * 8)

	sds = make([][]byte, secBlock.NumEntries)
	for i, size := range secSizes {
		sd := make([]byte, size&0xffffffff)
		_, err = io.ReadFull(rsrc, sd)
		if err != nil {
			return sds, n, &ParseError{Oper: "security descriptor", Err: err}
		}
		n += int64(len(sd))
		sds[i] = sd
	}

	secsize := int64((secBlock.TotalLength + 7) &^ 7)
	if n > secsize {
		return sds, n, &ParseError{Oper: "security descriptor", Err: errors.New("security descriptor table too small")}
	}

	_, err = io.CopyN(io.Discard, rsrc, secsize-n)
	if err != nil {
		return sds, n, err
	}

	n = secsize
	return sds, n, nil
}

// Open parses the image and returns the root directory.
func (img *Image) Open() (*File, error) {
	if img.sds == nil {
		rsrc, err := img.wim.resourceReaderWithOffset(img.wim.r, &img.offset, img.rootOffset)
		if err != nil {
			return nil, err
		}
		sds, n, err := img.wim.readSecurityDescriptors(rsrc)
		if err != nil {
			rsrc.Close()
			return nil, err
		}
		img.sds = sds
		img.r = rsrc
		img.rootOffset = n
		img.curOffset = n
	}

	f, err := img.readdir(img.rootOffset)
	if err != nil {
		return nil, err
	}
	if len(f) != 1 {
		return nil, &ParseError{Oper: "root directory", Err: errors.New("expected exactly 1 root directory entry")}
	}
	return f[0], err
}

func (img *Image) reset() {
	if img.r != nil {
		img.r.Close()
		img.r = nil
	}
	img.curOffset = -1
}

func (img *Image) readdir(offset int64) ([]*File, error) {
	img.m.Lock()
	defer img.m.Unlock()

	if offset < img.curOffset || offset > img.curOffset+chunkSize {
		// Reset to seek backward or to seek forward very far.
		img.reset()
	}
	if img.r == nil {
		rsrc, err := img.wim.resourceReaderWithOffset(img.wim.r, &img.offset, offset)
		if err != nil {
			return nil, err
		}
		img.r = rsrc
		img.curOffset = offset
	}
	if offset > img.curOffset {
		_, err := io.CopyN(io.Discard, img.r, offset-img.curOffset)
		if err != nil {
			img.reset()
			if err == io.EOF { //nolint:errorlint
				err = io.ErrUnexpectedEOF
			}
			return nil, err
		}
	}

	var entries []*File
	for {
		e, n, err := img.readNextEntry(img.r)
		img.curOffset += n
		if err == io.EOF { //nolint:errorlint
			break
		}
		if err != nil {
			img.reset()
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, nil
}

func (img *Image) readNextEntry(r io.Reader) (*File, int64, error) {
	var length int64
	err := binary.Read(r, binary.LittleEndian, &length)
	if err != nil {
		return nil, 0, &ParseError{Oper: "directory length check", Err: err}
	}

	if length == 0 {
		return nil, 8, io.EOF
	}

	left := length
	if left < direntrySize {
		return nil, 0, &ParseError{Oper: "directory entry", Err: errors.New("size too short")}
	}

	var dentry direntry
	err = binary.Read(r, binary.LittleEndian, &dentry)
	if err != nil {
		return nil, 0, &ParseError{Oper: "directory entry", Err: err}
	}

	left -= direntrySize

	namesLen := int64(dentry.FileNameLength + 2 + dentry.ShortNameLength)
	if left < namesLen {
		return nil, 0, &ParseError{Oper: "directory entry", Err: errors.New("size too short for names")}
	}

	names := make([]uint16, namesLen/2)
	err = binary.Read(r, binary.LittleEndian, names)
	if err != nil {
		return nil, 0, &ParseError{Oper: "file name", Err: err}
	}

	left -= namesLen

	var name, shortName string
	if dentry.FileNameLength > 0 {
		name = string(utf16.Decode(names[:dentry.FileNameLength/2]))
	}

	if dentry.ShortNameLength > 0 {
		shortName = string(utf16.Decode(names[dentry.FileNameLength/2+1:]))
	}

	var sd streamDescriptor
	zerohash := SHA1Hash{}
	if dentry.Hash != zerohash {
		var ok bool
		sd, ok = img.wim.fileData[dentry.Hash]
		if !ok {
			return nil, 0, &ParseError{
				Oper: "directory entry",
				Path: name,
				Err:  fmt.Errorf("could not find file data matching hash %#v", dentry),
			}
		}
	}

	f := &File{
		FileHeader: FileHeader{
			Attributes:     dentry.Attributes,
			CreationTime:   dentry.CreationTime,
			LastAccessTime: dentry.LastAccessTime,
			LastWriteTime:  dentry.LastWriteTime,
			Hash:           dentry.Hash,
			Size:           sd.OriginalSize,
			Name:           name,
			ShortName:      shortName,
		},

		offset:       sd.resourceDescriptor,
		partNumber:   sd.PartNumber,
		img:          img,
		subdirOffset: dentry.SubdirOffset,
	}

	isDir := false

	if dentry.Attributes&FILE_ATTRIBUTE_REPARSE_POINT == 0 {
		f.LinkID = dentry.ReparseHardLink
		if dentry.Attributes&FILE_ATTRIBUTE_DIRECTORY != 0 {
			isDir = true
		}
	} else {
		f.ReparseTag = uint32(dentry.ReparseHardLink)
		f.ReparseReserved = uint32(dentry.ReparseHardLink >> 32)
	}

	if isDir && f.subdirOffset == 0 {
		return nil, 0, &ParseError{Oper: "directory entry", Path: name, Err: errors.New("no subdirectory data for directory")}
	} else if !isDir && f.subdirOffset != 0 {
		return nil, 0, &ParseError{Oper: "directory entry", Path: name, Err: errors.New("unexpected subdirectory data for non-directory")}
	}

	if dentry.SecurityID != 0xffffffff {
		f.SecurityDescriptor = img.sds[dentry.SecurityID]
	}

	_, err = io.CopyN(io.Discard, r, left)
	if err != nil {
		if err == io.EOF { //nolint:errorlint
			err = io.ErrUnexpectedEOF
		}
		return nil, 0, err
	}

	if dentry.StreamCount > 0 {
		var streams []*Stream
		for i := uint16(0); i < dentry.StreamCount; i++ {
			s, n, err := img.readNextStream(r)
			length += n
			if err != nil {
				return nil, 0, err
			}
			// The first unnamed stream should be treated as the file stream.
			if i == 0 && s.Name == "" {
				f.Hash = s.Hash
				f.Size = s.Size
				f.offset = s.offset
				f.partNumber = s.partNumber
			} else if s.Name != "" {
				streams = append(streams, s)
			}
		}
		f.Streams = streams
	}

	if dentry.Attributes&FILE_ATTRIBUTE_REPARSE_POINT != 0 && f.Size == 0 {
		return nil, 0, &ParseError{
			Oper: "directory entry",
			Path: name,
			Err:  errors.New("reparse point is missing reparse stream"),
		}
	}

	return f, length, nil
}

func (img *Image) readNextStream(r io.Reader) (*Stream, int64, error) {
	var length int64
	err := binary.Read(r, binary.LittleEndian, &length)
	if err != nil {
		if err == io.EOF { //nolint:errorlint
			err = io.ErrUnexpectedEOF
		}
		return nil, 0, &ParseError{Oper: "stream length check", Err: err}
	}

	left := length
	if left < streamentrySize {
		return nil, 0, &ParseError{Oper: "stream entry", Err: errors.New("size too short")}
	}

	var sentry streamentry
	err = binary.Read(r, binary.LittleEndian, &sentry)
	if err != nil {
		return nil, 0, &ParseError{Oper: "stream entry", Err: err}
	}

	left -= streamentrySize

	if left < int64(sentry.NameLength) {
		return nil, 0, &ParseError{Oper: "stream entry", Err: errors.New("size too short for name")}
	}

	names := make([]uint16, sentry.NameLength/2)
	err = binary.Read(r, binary.LittleEndian, names)
	if err != nil {
		return nil, 0, &ParseError{Oper: "file name", Err: err}
	}

	left -= int64(sentry.NameLength)
	name := string(utf16.Decode(names))

	var sd streamDescriptor
	if sentry.Hash != (SHA1Hash{}) {
		var ok bool
		sd, ok = img.wim.fileData[sentry.Hash]
		if !ok {
			return nil, 0, &ParseError{
				Oper: "stream entry",
				Path: name,
				Err:  fmt.Errorf("could not find file data matching hash %v", sentry.Hash),
			}
		}
	}

	s := &Stream{
		StreamHeader: StreamHeader{
			Hash: sentry.Hash,
			Size: sd.OriginalSize,
			Name: name,
		},
		wim:        img.wim,
		offset:     sd.resourceDescriptor,
		partNumber: sd.PartNumber,
	}

	_, err = io.CopyN(io.Discard, r, left)
	if err != nil {
		if err == io.EOF { //nolint:errorlint
			err = io.ErrUnexpectedEOF
		}
		return nil, 0, err
	}

	return s, length, nil
}

// Open returns an io.ReadCloser that can be used to read the stream's contents.
func (s *Stream) Open() (io.ReadCloser, error) {
	return s.wim.resourceReaderAt(s.partNumber, &s.offset, 0)
}

// Open returns an io.ReadCloser that can be used to read the file's contents.
func (f *File) Open() (io.ReadCloser, error) {
	return f.img.wim.resourceReaderAt(f.partNumber, &f.offset, 0)
}

// Readdir reads the directory entries.
func (f *File) Readdir() ([]*File, error) {
	if !f.IsDir() {
		return nil, errors.New("not a directory")
	}
	return f.img.readdir(f.subdirOffset)
}

// IsDir returns whether the given file is a directory. It returns false when it
// is a directory reparse point.
func (f *FileHeader) IsDir() bool {
	return f.Attributes&(FILE_ATTRIBUTE_DIRECTORY|FILE_ATTRIBUTE_REPARSE_POINT) == FILE_ATTRIBUTE_DIRECTORY
}
