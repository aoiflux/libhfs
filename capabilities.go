package hfs

// Volume attribute bits from the HFS+ volume header.
const (
	volAttrJournaled = uint32(1 << 13) // kHFSVolumeJournaledBit
)

// Capabilities reports which filesystem features a volume can carry.
//
// These are properties of the format and the volume, not of this package. A
// false value means the volume cannot hold that kind of data at all — classic
// HFS has no extended attributes, so asking for them is meaningless rather than
// merely unsupported.
//
// The point of this type is to let a caller branch on a value instead of
// comparing Kind() against a string, and to distinguish "this volume records no
// access times" from "every access time happened to be zero".
type Capabilities struct {
	// ExtendedAttributes reports whether the volume has an attributes B-tree.
	ExtendedAttributes bool

	// HardLinks reports whether the format supports hard links. Classic HFS
	// does not.
	HardLinks bool

	// Compression reports whether the format supports decmpfs compression,
	// which requires extended attributes.
	Compression bool

	// AccessTimes reports whether catalog records carry an access date.
	// Classic HFS has no such field, so CatalogTimes.Accessed is always zero
	// there — a fact about the format, not about the files.
	AccessTimes bool

	// AttrModTimes reports whether catalog records carry an
	// attribute-modification date. Classic HFS does not.
	AttrModTimes bool

	// POSIXPermissions reports whether catalog records carry BSD ownership and
	// mode. Classic HFS predates it.
	POSIXPermissions bool

	// CaseSensitive reports whether name comparison is case-sensitive. Only
	// HFSX volumes formatted that way are.
	CaseSensitive bool

	// Journaled reports whether the volume has a journal. This package does not
	// read the journal, but its presence tells an examiner that a source of
	// recent pre-commit metadata exists on the volume.
	Journaled bool

	// UnicodeNames reports whether names are stored as Unicode. Classic HFS
	// stores bytes in a Mac script encoding instead — see Volume.SetTextEncoding.
	UnicodeNames bool
}

// Capabilities returns what this volume's format supports.
func (v *Volume) Capabilities() Capabilities {
	if v == nil {
		return Capabilities{}
	}

	if v.kind == KindHFS {
		// Classic HFS: no attributes tree, no POSIX metadata, no links, and
		// only three dates per record.
		return Capabilities{
			CaseSensitive: false,
			UnicodeNames:  false,
		}
	}

	caps := Capabilities{
		ExtendedAttributes: v.header.AttributesFile.TotalBlocks > 0,
		HardLinks:          true,
		AccessTimes:        true,
		AttrModTimes:       true,
		POSIXPermissions:   true,
		UnicodeNames:       true,
		Journaled:          v.header.Attributes&volAttrJournaled != 0,
	}
	// decmpfs stores its metadata in an extended attribute, so compression is
	// only possible where attributes are.
	caps.Compression = caps.ExtendedAttributes

	if v.kind == KindHFSX {
		if hdr, err := v.CatalogBTreeHeader(); err == nil {
			caps.CaseSensitive = hdr.CompType == btreeCompTypeSensitive
		}
	}
	return caps
}
