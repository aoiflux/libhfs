package hfs

import "sort"

// Attribute B-tree record types. Every record begins with one of these as a
// big-endian uint32.
const (
	attrRecordTypeForkData  = uint32(0x20)
	attrRecordTypeExtension = uint32(0x30)
)

// Offsets within attribute records.
const (
	attrInlineHeaderSize = 16 // recordType, 2 reserved words, attrSize
	attrForkDataOffset   = 8  // recordType + reserved, then an HFSPlusForkData
	attrForkDataSize     = 80
	attrExtentsOffset    = 8 // recordType + reserved, then an extent record
	attrExtentsSize      = 64
)

// XAttrStorage says where an extended attribute's value lives.
type XAttrStorage uint8

const (
	// XAttrInline means the value is held in the attributes B-tree record
	// itself. Small attributes — the common case — are stored this way.
	XAttrInline XAttrStorage = iota

	// XAttrFork means the value is held in allocation blocks described by
	// Extents, exactly like file data.
	XAttrFork
)

func (s XAttrStorage) String() string {
	if s == XAttrFork {
		return "fork"
	}
	return "inline"
}

// XAttr describes one extended attribute without reading its value.
type XAttr struct {
	// CNID identifies the catalog node the attribute belongs to.
	CNID uint32

	// Name is the attribute name, such as "com.apple.quarantine".
	Name string

	// Size is the value's length in bytes.
	Size uint64

	// Storage says whether the value is inline or fork-backed.
	Storage XAttrStorage

	// Extents lists the on-disk fragments holding the value, for fork-backed
	// attributes. It is nil for inline attributes, whose bytes are in the
	// B-tree rather than in allocation blocks.
	Extents []ExtentDescriptor
}

// hasAttributes reports whether this volume has an attributes B-tree at all.
// Classic HFS never does, and an HFS+ volume that has never had an extended
// attribute written to it may have an empty one.
func (v *Volume) hasAttributes() bool {
	return v != nil && v.kind != KindHFS && v.header.AttributesFile.TotalBlocks > 0
}

// ListXAttrs returns every extended attribute on a catalog node, in B-tree key
// order.
//
// Volumes with no attributes file, and nodes with no attributes, return an
// empty slice rather than an error — having none is not a failure.
//
// System attributes such as "com.apple.decmpfs" and "com.apple.ResourceFork"
// are returned like any other. Filtering them is a policy decision that belongs
// to the caller, not the parser.
func (v *Volume) ListXAttrs(cnid uint32) ([]XAttr, error) {
	// hasAttributes answers false for a nil volume, which would report the
	// node as having no attributes. That is a different claim from "there is
	// no volume here", and an examiner reading a report cannot tell them
	// apart after the fact.
	if v == nil {
		return nil, &ParseError{Op: "list_xattrs", Offset: int64(cnid), Err: ErrCorrupt}
	}
	if !v.hasAttributes() {
		return nil, nil
	}

	// Records for one file arrive grouped by name, with any 0x30 extension
	// records following the 0x20 fork-data record they extend.
	byName := make(map[string]*XAttr)
	order := make([]string, 0, 8)

	err := v.walkAttributesForFile(cnid, func(k attributesKey, payload []byte) error {
		if len(payload) < 4 {
			return nil
		}
		switch be32(payload[0:4]) {
		case attrRecordTypeInlineData:
			if len(payload) < attrInlineHeaderSize {
				return nil
			}
			size := be32(payload[12:16])
			if int(size) > len(payload)-attrInlineHeaderSize {
				// Truncated record: report the attribute's presence, since its
				// existence is itself evidence, but not a length we cannot back.
				v.noteAnomaly("xattr_inline", int64(cnid),
					"inline attribute declares more data than its record holds")
				size = uint32(len(payload) - attrInlineHeaderSize)
			}
			if _, seen := byName[k.Name]; !seen {
				order = append(order, k.Name)
			}
			byName[k.Name] = &XAttr{
				CNID: cnid, Name: k.Name, Size: uint64(size), Storage: XAttrInline,
			}

		case attrRecordTypeForkData:
			if len(payload) < attrForkDataOffset+attrForkDataSize {
				return nil
			}
			fd := parseForkData(payload[attrForkDataOffset : attrForkDataOffset+attrForkDataSize])
			if _, seen := byName[k.Name]; !seen {
				order = append(order, k.Name)
			}
			byName[k.Name] = &XAttr{
				CNID:    cnid,
				Name:    k.Name,
				Size:    fd.LogicalSize,
				Storage: XAttrFork,
				Extents: compactExtents(fd.Extents[:]),
			}

		case attrRecordTypeExtension:
			// Additional extents for a fork-backed attribute whose value did
			// not fit in the eight the fork-data record holds.
			existing, ok := byName[k.Name]
			if !ok || existing.Storage != XAttrFork {
				v.noteAnomaly("xattr_extension", int64(cnid),
					"extension record without a preceding fork-data record")
				return nil
			}
			if len(payload) < attrExtentsOffset+attrExtentsSize {
				return nil
			}
			more, err := parseExtentsRecord(payload[attrExtentsOffset : attrExtentsOffset+attrExtentsSize])
			if err != nil {
				return nil
			}
			existing.Extents = append(existing.Extents, compactExtents(more)...)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	out := make([]XAttr, 0, len(order))
	for _, name := range order {
		out = append(out, *byName[name])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ListXAttrsByPath is ListXAttrs addressed by path.
func (v *Volume) ListXAttrsByPath(path string) ([]XAttr, error) {
	rec, err := v.OpenPath(path)
	if err != nil {
		return nil, err
	}
	return v.ListXAttrs(rec.CNID)
}

// GetXAttr returns the metadata for one named attribute without reading its
// value. It reports ErrNotFound when the node has no such attribute.
func (v *Volume) GetXAttr(cnid uint32, name string) (XAttr, error) {
	attrs, err := v.ListXAttrs(cnid)
	if err != nil {
		return XAttr{}, err
	}
	for _, a := range attrs {
		if a.Name == name {
			return a, nil
		}
	}
	return XAttr{}, ErrNotFound
}

// ReadXAttr returns the value of one named attribute.
//
// The buffer is sized from the attribute's recorded length, so an implausible
// size yields [ErrSizeLimit] rather than an allocation attempt — see
// [Volume.SetMaxAlloc]. Use [Volume.OpenXAttr] to stream a large value.
func (v *Volume) ReadXAttr(cnid uint32, name string) ([]byte, error) {
	if !v.hasAttributes() {
		return nil, ErrNotFound
	}

	// Inline values are read straight out of the B-tree record, so they never
	// go through the extent path.
	var inline []byte
	var found bool
	err := v.walkAttributesForFile(cnid, func(k attributesKey, payload []byte) error {
		if k.Name != name || k.StartBlock != 0 {
			return nil
		}
		if len(payload) < 4 || be32(payload[0:4]) != attrRecordTypeInlineData {
			return nil
		}
		if len(payload) < attrInlineHeaderSize {
			return nil
		}
		size := int(be32(payload[12:16]))
		if size > len(payload)-attrInlineHeaderSize {
			size = len(payload) - attrInlineHeaderSize
		}
		if err := v.checkAlloc("read_xattr", int64(size)); err != nil {
			return err
		}
		inline = append([]byte(nil), payload[attrInlineHeaderSize:attrInlineHeaderSize+size]...)
		found = true
		return errStopWalk
	})
	if err != nil && !isStopWalk(err) {
		return nil, err
	}
	if found {
		return inline, nil
	}

	// Otherwise it is fork-backed.
	fh, err := v.OpenXAttr(cnid, name)
	if err != nil {
		return nil, err
	}
	return fh.ReadAll()
}

// ReadXAttrByPath is ReadXAttr addressed by path.
func (v *Volume) ReadXAttrByPath(path, name string) ([]byte, error) {
	rec, err := v.OpenPath(path)
	if err != nil {
		return nil, err
	}
	return v.ReadXAttr(rec.CNID, name)
}

// OpenXAttr returns a reader over a fork-backed attribute's value, for values
// too large to buffer.
//
// Inline attributes are served from memory, so the returned File reads the
// value already held in the B-tree record.
func (v *Volume) OpenXAttr(cnid uint32, name string) (*File, error) {
	attr, err := v.GetXAttr(cnid, name)
	if err != nil {
		return nil, err
	}

	if attr.Storage == XAttrInline {
		data, err := v.ReadXAttr(cnid, name)
		if err != nil {
			return nil, err
		}
		return &File{vol: v, inline: data, size: int64(len(data))}, nil
	}

	if len(attr.Extents) == 0 {
		return nil, &ParseError{Op: "open_xattr", Offset: int64(cnid), Err: ErrMissingExtent}
	}
	return &File{
		vol:     v,
		extents: attr.Extents,
		size:    int64(attr.Size),
	}, nil
}

// WalkXAttrs visits every extended attribute on the volume, in B-tree key
// order. Returning a non-nil error from cb stops the walk.
//
// This is the bulk-collection path: quarantine flags, provenance records and
// security ACLs across a whole image, without a lookup per file.
func (v *Volume) WalkXAttrs(cb func(XAttr) error) error {
	if v == nil {
		return &ParseError{Op: "walk_xattrs", Offset: 0, Err: ErrCorrupt}
	}
	if cb == nil || !v.hasAttributes() {
		return nil
	}

	var current *XAttr
	flush := func() error {
		if current == nil {
			return nil
		}
		a := *current
		current = nil
		return cb(a)
	}

	err := v.walkAttributesLeafChain(func(k attributesKey, payload []byte) error {
		if len(payload) < 4 {
			return nil
		}
		switch be32(payload[0:4]) {
		case attrRecordTypeInlineData:
			if err := flush(); err != nil {
				return err
			}
			if len(payload) < attrInlineHeaderSize {
				return nil
			}
			size := be32(payload[12:16])
			if int(size) > len(payload)-attrInlineHeaderSize {
				size = uint32(len(payload) - attrInlineHeaderSize)
			}
			return cb(XAttr{CNID: k.FileID, Name: k.Name, Size: uint64(size), Storage: XAttrInline})

		case attrRecordTypeForkData:
			if err := flush(); err != nil {
				return err
			}
			if len(payload) < attrForkDataOffset+attrForkDataSize {
				return nil
			}
			fd := parseForkData(payload[attrForkDataOffset : attrForkDataOffset+attrForkDataSize])
			current = &XAttr{
				CNID:    k.FileID,
				Name:    k.Name,
				Size:    fd.LogicalSize,
				Storage: XAttrFork,
				Extents: compactExtents(fd.Extents[:]),
			}

		case attrRecordTypeExtension:
			if current == nil || current.CNID != k.FileID || current.Name != k.Name {
				return nil
			}
			if len(payload) < attrExtentsOffset+attrExtentsSize {
				return nil
			}
			more, err := parseExtentsRecord(payload[attrExtentsOffset : attrExtentsOffset+attrExtentsSize])
			if err != nil {
				return nil
			}
			current.Extents = append(current.Extents, compactExtents(more)...)
		}
		return nil
	})
	if err != nil {
		if isStopWalk(err) {
			return nil
		}
		return err
	}
	return flush()
}

func isStopWalk(err error) bool {
	return err == errStopWalk
}
