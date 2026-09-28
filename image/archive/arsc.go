package archive

import (
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

// DecodeARSC renders a binary Android resource table (resources.arsc) as text:
// one line per resource and configuration, with the id, type and name a
// resource is addressed by, the value as `aapt dump` shows it, and the items of
// style-like "bag" entries beneath them. References are resolved against the
// table itself, so `@0x7f050000` reads as `@color/primary`.
//
// The table is what `udf cat` prints for resources.arsc inside an APK, next to
// the AXML decoding that already turns the manifest and layouts into XML.
//
// Layout: a RES_TABLE chunk holding a global string pool (the values) and one
// chunk per package; each package holds the type and key string pools plus one
// RES_TABLE_TYPE chunk per type and configuration.
func DecodeARSC(data []byte) (string, error) {
	t, err := parseARSC(data)
	if err != nil {
		return "", err
	}
	return t.render(), nil
}

// isARSC reports whether the data looks like a resource table, checked before
// the (deeper) parse so unrelated files are never touched.
func isARSC(data []byte) bool {
	if len(data) < 12 {
		return false
	}
	u16 := func(off int) uint16 { return binary.LittleEndian.Uint16(data[off:]) }
	u32 := func(off int) uint32 { return binary.LittleEndian.Uint32(data[off:]) }
	return u16(0) == arscTableType && u16(2) >= 12 && int64(u32(4)) <= int64(len(data))
}

const (
	arscTableType   = 0x0002
	arscStringPool  = 0x0001
	arscPackageType = 0x0200
	arscTypeType    = 0x0201
	arscTypeSpec    = 0x0202

	arscEntryComplex  = 0x0001
	arscEntryOffset16 = 0x0002
	arscTypeSparse    = 0x0001

	// arscMaxText bounds the rendered output: a table's text can be many times
	// its binary size, and this is a listing, not a dump for machine use.
	arscMaxText = 16 << 20
)

// arscResource is one entry of the table before rendering.
type arscResource struct {
	id     uint32
	typeID uint8
	typ    string
	name   string
	config string
	// value for a simple entry, or the bag's items
	value    arscValue
	parent   uint32
	items    []arscItem
	isBag    bool
	hasValue bool
}

type arscItem struct {
	nameID uint32
	value  arscValue
}

// arscValue is a raw Res_value: the rendering (which may need the table's id
// map) happens once every resource is known.
type arscValue struct {
	dataType uint8
	data     uint32
}

type arscTable struct {
	data      []byte
	global    *resStringPool
	packages  []arscPackage
	resources []arscResource
	byID      map[uint32]string // 0x7f010000 -> "string/app_name"
}

type arscPackage struct {
	id    uint32
	name  string
	types *resStringPool
	keys  *resStringPool
}

func (t *arscTable) render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# resources.arsc: %d package(s), %d resources, decoded from %d bytes\n",
		len(t.packages), len(t.resources), len(t.data))
	for _, p := range t.packages {
		fmt.Fprintf(&b, "# package 0x%02x %s\n", p.id, p.name)
	}
	for _, r := range t.resources {
		if b.Len() > arscMaxText {
			b.WriteString("# ... output truncated\n")
			break
		}
		label := r.typ
		if r.name != "" {
			label += "/" + r.name
		}
		switch {
		case r.isBag:
			fmt.Fprintf(&b, "0x%08X  %-40s [%s]  bag(%d items)", r.id, label, r.config, len(r.items))
			if r.parent != 0 {
				fmt.Fprintf(&b, " parent=%s", t.describeRef(r.parent))
			}
			b.WriteByte('\n')
			for _, item := range r.items {
				fmt.Fprintf(&b, "     %-38s %s\n", t.describeRef(item.nameID), t.renderValue(item.value))
			}
		case r.hasValue:
			fmt.Fprintf(&b, "0x%08X  %-40s [%s]  %s\n", r.id, label, r.config, t.renderValue(r.value))
		default:
			fmt.Fprintf(&b, "0x%08X  %-40s [%s]\n", r.id, label, r.config)
		}
	}
	return b.String()
}

// describeRef names a resource id, falling back to the hex form.
func (t *arscTable) describeRef(id uint32) string {
	if name, ok := t.byID[id]; ok {
		return "@" + name
	}
	return fmt.Sprintf("@0x%08X", id)
}

func parseARSC(data []byte) (*arscTable, error) {
	t := &arscTable{data: data, byID: map[uint32]string{}}
	u16 := func(off int) uint16 { return binary.LittleEndian.Uint16(data[off:]) }
	u32 := func(off int) uint32 { return binary.LittleEndian.Uint32(data[off:]) }

	tableEnd := int(u32(4))
	if tableEnd > len(data) || tableEnd < 12 {
		tableEnd = len(data)
	}
	if u16(0) != arscTableType {
		return nil, fmt.Errorf("not a resource table")
	}

	pos := int(u16(2)) // past the table header
	for pos+8 <= tableEnd {
		kind := u16(pos)
		hdrSize := int(u16(pos + 2))
		size := int(u32(pos + 4))
		if size < 8 || pos+size > tableEnd || hdrSize < 8 || hdrSize > size {
			return nil, fmt.Errorf("resource chunk at %d is out of bounds", pos)
		}
		switch kind {
		case arscStringPool:
			if pool, err := parseResStringPool(data, pos, pos+hdrSize, pos+size); err == nil {
				t.global = pool
			}
		case arscPackageType:
			pkg, err := t.parsePackage(pos, hdrSize, pos+size)
			if err != nil {
				return nil, err
			}
			t.packages = append(t.packages, pkg)
		}
		pos += size
	}
	if len(t.packages) == 0 {
		return nil, fmt.Errorf("resource table holds no package")
	}

	// A second pass names every resource, so references (and bag items) can be
	// rendered with the type and name they point at.
	for i := range t.resources {
		r := &t.resources[i]
		if name := t.resourceName(*r); name != "" {
			t.byID[r.id] = name
		}
	}
	sort.SliceStable(t.resources, func(i, j int) bool { return t.resources[i].id < t.resources[j].id })
	return t, nil
}

func (t *arscTable) resourceName(r arscResource) string {
	if r.typ == "" {
		return ""
	}
	if r.name == "" {
		return r.typ
	}
	return r.typ + "/" + r.name
}

// parsePackage walks one package's chunks: its string pools, then a type chunk
// per type and configuration.
func (t *arscTable) parsePackage(start, hdrSize, end int) (arscPackage, error) {
	u16 := func(off int) uint16 { return binary.LittleEndian.Uint16(t.data[off:]) }
	u32 := func(off int) uint32 { return binary.LittleEndian.Uint32(t.data[off:]) }

	var pkg arscPackage
	pkg.id = u32(start + 8)
	// The package name is a 128-unit UTF-16 string in the header.
	nameUnits := make([]uint16, 0, 128)
	for i := 0; i < 128; i++ {
		u := u16(start + 12 + i*2)
		if u == 0 {
			break
		}
		nameUnits = append(nameUnits, u)
	}
	pkg.name = decodeUTF16(nameUnits)

	typeStrings := int(u32(start + 268))
	keyStrings := int(u32(start + 276))

	for pos := start + hdrSize; pos+8 <= end; {
		kind := u16(pos)
		hdr := int(u16(pos + 2))
		size := int(u32(pos + 4))
		if size < 8 || pos+size > end || hdr < 8 || hdr > size {
			break
		}
		switch kind {
		case arscStringPool:
			pool, err := parseResStringPool(t.data, pos, pos+hdr, pos+size)
			if err != nil {
				pos += size
				continue
			}
			switch {
			case typeStrings > 0 && pos == start+typeStrings:
				pkg.types = pool
			case keyStrings > 0 && pos == start+keyStrings:
				pkg.keys = pool
			default:
				if pkg.types == nil {
					pkg.types = pool
				} else if pkg.keys == nil {
					pkg.keys = pool
				}
			}
		case arscTypeType:
			if err := t.parseType(&pkg, pos, hdr, pos+size); err != nil {
				return pkg, err
			}
		case arscTypeSpec:
			// Entry flags (public, weak, ...): not part of a readable listing.
		}
		pos += size
	}
	return pkg, nil
}

// parseType reads one RES_TABLE_TYPE chunk: the configuration it applies to,
// the entry offsets (plain, 16-bit or sparse) and each entry's value or bag.
func (t *arscTable) parseType(pkg *arscPackage, start, hdrSize, end int) error {
	u16 := func(off int) uint16 { return binary.LittleEndian.Uint16(t.data[off:]) }
	u32 := func(off int) uint32 { return binary.LittleEndian.Uint32(t.data[off:]) }

	typeID := t.data[start+8]
	flags := t.data[start+9]
	entryCount := int(u32(start + 12))
	entriesStart := int(u32(start + 16))
	if hdrSize < 20 || entryCount < 0 {
		return fmt.Errorf("bad type chunk at %d", start)
	}
	if start+entriesStart > end {
		return fmt.Errorf("type chunk entries start out of range")
	}
	typeName := pkg.types.get(uint32(typeID - 1))

	// Entry offsets: 32-bit, 16-bit (offset/4) or sparse (id + offset/4).
	type slot struct {
		index  int
		offset int
	}
	var slots []slot
	offsetsBase := start + hdrSize
	switch {
	case flags&arscTypeSparse != 0:
		if offsetsBase+entryCount*4 > end {
			return fmt.Errorf("sparse type offsets out of range")
		}
		for i := 0; i < entryCount; i++ {
			idx := int(u16(offsetsBase + i*4))
			off := int(u16(offsetsBase+i*4+2)) * 4
			slots = append(slots, slot{index: idx, offset: off})
		}
	case flags&arscEntryOffset16 != 0:
		if offsetsBase+entryCount*2 > end {
			return fmt.Errorf("offset16 type offsets out of range")
		}
		for i := 0; i < entryCount; i++ {
			raw := u16(offsetsBase + i*2)
			if raw == 0xFFFF {
				continue
			}
			slots = append(slots, slot{index: i, offset: int(raw) * 4})
		}
	default:
		if offsetsBase+entryCount*4 > end {
			return fmt.Errorf("type offsets out of range")
		}
		for i := 0; i < entryCount; i++ {
			raw := u32(offsetsBase + i*4)
			if raw == 0xFFFFFFFF {
				continue
			}
			slots = append(slots, slot{index: i, offset: int(raw)})
		}
	}

	for _, s := range slots {
		off := start + entriesStart + s.offset
		if off+8 > end {
			continue
		}
		entrySize := int(u16(off))
		entryFlags := u16(off + 2)
		keyID := u32(off + 4)
		res := arscResource{
			id:     pkg.id<<24 | uint32(typeID)<<16 | uint32(s.index),
			typeID: typeID,
			typ:    typeName,
			name:   pkg.keys.get(keyID),
			config: arscConfigLabelAt(t.data, start, hdrSize, end),
		}
		if entryFlags&arscEntryComplex != 0 {
			res.isBag = true
			if off+16 <= end {
				res.parent = u32(off + 8)
				count := int(u32(off + 12))
				itemOff := off + 16
				if count > 4096 {
					count = 4096
				}
				for i := 0; i < count && itemOff+12 <= end; i++ {
					res.items = append(res.items, arscItem{
						nameID: u32(itemOff),
						value:  t.rawValue(itemOff + 4),
					})
					itemOff += 12
				}
			}
		} else if valueOff := off + entrySize; valueOff+8 <= end && entrySize >= 8 {
			res.value = t.rawValue(valueOff)
			res.hasValue = true
		}
		t.resources = append(t.resources, res)
	}
	return nil
}

// rawValue reads a Res_value without interpreting it.
func (t *arscTable) rawValue(off int) arscValue {
	if off+8 > len(t.data) {
		return arscValue{}
	}
	return arscValue{
		dataType: t.data[off+3],
		data:     binary.LittleEndian.Uint32(t.data[off+4:]),
	}
}

// renderValue formats a Res_value. References are named through the table's id
// map (which is complete by the time anything is rendered) and string values
// through its global string pool.
func (t *arscTable) renderValue(v arscValue) string {
	dataType, data := v.dataType, v.data
	switch dataType {
	case 0x00:
		return "@null"
	case 0x01:
		if data == 0 {
			return "@empty"
		}
		return t.describeRef(data)
	case 0x02:
		return fmt.Sprintf("?%s", strings.TrimPrefix(t.describeRef(data), "@"))
	case 0x03:
		return strconv.Quote(t.global.get(data))
	case 0x04:
		return strconv.FormatFloat(float64(math.Float32frombits(data)), 'g', -1, 32)
	case 0x05:
		return axmlDimension(data)
	case 0x06:
		return axmlFraction(data)
	case 0x07:
		return fmt.Sprintf("@dynamic/0x%08X", data)
	case 0x08:
		return fmt.Sprintf("?dynamic/0x%08X", data)
	case 0x10:
		return strconv.FormatInt(int64(int32(data)), 10)
	case 0x11:
		return fmt.Sprintf("0x%X", data)
	case 0x12:
		if data != 0 {
			return "true"
		}
		return "false"
	case 0x1c, 0x1d, 0x1e, 0x1f:
		return axmlColor(dataType, data)
	}
	return fmt.Sprintf("0x%X (type 0x%02x)", data, dataType)
}

// arscConfigLabelAt renders the configuration of a type chunk.
func arscConfigLabelAt(data []byte, start, hdrSize, end int) string {
	if hdrSize <= 20 {
		return "default"
	}
	limit := start + hdrSize
	if limit > end {
		limit = end
	}
	return arscConfig(data[start+20 : limit])
}

// arscConfig renders a ResTable_config as the compact qualifier list Android
// uses, e.g. "en-rUS-sw600dp-v29"; an empty configuration is "default".
func arscConfig(cfg []byte) string {
	if len(cfg) < 4 {
		return "default"
	}
	var parts []string
	u16 := func(off int) uint16 {
		if off+2 > len(cfg) {
			return 0
		}
		return binary.LittleEndian.Uint16(cfg[off:])
	}
	u8 := func(off int) uint8 {
		if off >= len(cfg) {
			return 0
		}
		return cfg[off]
	}

	if mcc := u16(4); mcc != 0 {
		parts = append(parts, fmt.Sprintf("mcc%d", mcc))
		if mnc := u16(6); mnc != 0 {
			parts = append(parts, fmt.Sprintf("mnc%d", mnc))
		}
	}
	// Locale: language/country, plus script and variant in BCP47 form.
	lang := strings.TrimRight(string(cfg[min(8, len(cfg)):min(10, len(cfg))]), "\x00")
	country := ""
	if len(cfg) >= 12 {
		country = strings.TrimRight(string(cfg[10:12]), "\x00")
	}
	script := ""
	if len(cfg) >= 36 {
		script = strings.TrimRight(string(cfg[32:36]), "\x00")
	}
	variant := ""
	if len(cfg) >= 44 {
		variant = strings.TrimRight(string(cfg[36:44]), "\x00")
	}
	switch {
	case lang == "" && country == "" && script == "" && variant == "":
	case script != "" || variant != "":
		bcp := "b+" + lang
		if script != "" {
			bcp += "+" + script
		}
		if country != "" {
			bcp += "+" + country
		}
		if variant != "" {
			bcp += "+" + variant
		}
		parts = append(parts, bcp)
	default:
		loc := lang
		if country != "" {
			loc += "-r" + country
		}
		parts = append(parts, loc)
	}

	if orientation := u8(12); orientation != 0 {
		parts = append(parts, [...]string{"", "port", "land", "square"}[min(int(orientation), 3)])
	}
	if touch := u8(13); touch != 0 {
		parts = append(parts, [...]string{"", "notouch", "stylus", "finger"}[min(int(touch), 3)])
	}
	switch density := u16(14); density {
	case 0:
	case 0xfffe:
		parts = append(parts, "anydpi")
	case 0xffff:
		parts = append(parts, "nodpi")
	default:
		parts = append(parts, fmt.Sprintf("%ddpi", density))
	}
	if keyboard := u8(16); keyboard != 0 {
		parts = append(parts, [...]string{"", "nokeys", "qwerty", "12key"}[min(int(keyboard), 3)])
	}
	if nav := u8(17); nav != 0 {
		parts = append(parts, [...]string{"", "nonav", "dpad", "trackball", "wheel"}[min(int(nav), 4)])
	}
	if in := u8(18); in != 0 {
		if in&0x03 != 0 {
			parts = append(parts, [...]string{"", "keysexposed", "keyshidden", "keyssoft"}[min(int(in&0x03), 3)])
		}
		if in&0x0c != 0 {
			parts = append(parts, [...]string{"", "navexposed", "navhidden"}[(int(in&0x0c)>>2)])
		}
	}
	if w, h := u16(20), u16(22); w != 0 || h != 0 {
		if w != 0 {
			parts = append(parts, fmt.Sprintf("w%ddp", w))
		}
		if h != 0 {
			parts = append(parts, fmt.Sprintf("h%ddp", h))
		}
	}
	if sdk := u16(24); sdk != 0 {
		parts = append(parts, fmt.Sprintf("v%d", sdk))
	}
	if minor := u16(26); minor != 0 {
		parts = append(parts, fmt.Sprintf(".%d", minor))
	}
	if layout := u8(28); layout != 0 {
		if size := layout & 0x0f; size != 0 {
			parts = append(parts, [...]string{"", "small", "normal", "large", "xlarge"}[min(int(size), 4)])
		}
		if long := layout & 0x30; long != 0 {
			parts = append(parts, [...]string{"", "notlong", "long"}[min(int(long>>4), 2)])
		}
		if dir := layout & 0xc0; dir != 0 {
			parts = append(parts, [...]string{"", "ldltr", "ldrtl"}[min(int(dir>>6), 2)])
		}
	}
	if ui := u8(29); ui != 0 {
		switch ui & 0x0f {
		case 1:
			parts = append(parts, "desk")
		case 2:
			parts = append(parts, "car")
		case 3:
			parts = append(parts, "television")
		case 4:
			parts = append(parts, "appliance")
		case 5:
			parts = append(parts, "watch")
		case 6:
			parts = append(parts, "vrheadset")
		}
		if ui&0x20 != 0 {
			parts = append(parts, "night")
		} else if ui&0x10 != 0 {
			parts = append(parts, "notnight")
		}
	}
	if sw := u16(30); sw != 0 {
		parts = append(parts, fmt.Sprintf("sw%ddp", sw))
	}
	if len(cfg) >= 52 {
		if layout2 := u8(48); layout2 != 0 {
			if round := layout2 & 0x03; round == 1 {
				parts = append(parts, "notround")
			} else if round == 2 {
				parts = append(parts, "round")
			}
		}
		if mode := u8(49); mode != 0 {
			if mode&0x03 == 1 {
				parts = append(parts, "widecg")
			} else if mode&0x03 == 2 {
				parts = append(parts, "nowidecg")
			}
			if mode&0x0c == 0x04 {
				parts = append(parts, "highdr")
			} else if mode&0x0c == 0x08 {
				parts = append(parts, "lowdr")
			}
		}
	}
	if len(parts) == 0 {
		return "default"
	}
	return strings.Join(parts, "-")
}

// decodeUTF16 turns UTF-16 units into a string.
func decodeUTF16(units []uint16) string {
	return string(utf16.Decode(units))
}
