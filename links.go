package hfs

import "strings"

// Finder type/creator pairs that mark a catalog record as a link rather than
// ordinary content.
const (
	finderTypeHardLink    = uint32(0x686C6E6B) // "hlnk"
	finderCreatorHFSPlus  = uint32(0x6866732B) // "hfs+"
	finderTypeDirLink     = uint32(0x66647270) // "fdrp"
	finderCreatorMacS     = uint32(0x4D414353) // "MACS"
	finderTypeSymlink     = uint32(0x736C6E6B) // "slnk"
	finderCreatorSymlink  = uint32(0x72686170) // "rhap"
	privateDataDirNameSub = "HFS+ Private"
)

// File mode bits from the BSD stat(2) layout, as stored in HFSPlusBSDInfo.
const (
	sIFMT  = uint16(0xF000)
	sIFLNK = uint16(0xA000)
	sIFREG = uint16(0x8000)
	sIFDIR = uint16(0x4000)
)

// LinkKind classifies a catalog record's link status.
type LinkKind uint8

const (
	// LinkNone marks an ordinary file or folder.
	LinkNone LinkKind = iota

	// LinkHardFile marks a hard link to a file. The record is a stub; the
	// content lives in an indirect node inside the volume's private data
	// directory, identified by LinkTarget.
	LinkHardFile

	// LinkHardDir marks a hard link to a directory, as created by Time Machine.
	LinkHardDir

	// LinkSymbolic marks a symbolic link. Its target is the text held in the
	// data fork — see Volume.ReadLink.
	LinkSymbolic
)

func (k LinkKind) String() string {
	switch k {
	case LinkHardFile:
		return "hard link (file)"
	case LinkHardDir:
		return "hard link (directory)"
	case LinkSymbolic:
		return "symlink"
	default:
		return "none"
	}
}

// BSDInfo holds the POSIX ownership and permission metadata HFS+ records for
// each catalog node. It is absent on classic HFS, which predates it, and is
// zero there.
type BSDInfo struct {
	OwnerID    uint32
	GroupID    uint32
	AdminFlags uint8
	OwnerFlags uint8 // UF_HIDDEN, UF_IMMUTABLE, UF_COMPRESSED, …
	FileMode   uint16

	// Special is a union on disk. It holds the link count on a hard-link
	// inode, the inode number on a hard-link stub, and the device number on a
	// device node — which of those it is depends on the record, so it is
	// exposed raw rather than guessed at. CatalogRecord.LinkTarget and
	// LinkCount give the interpreted values where they are known.
	Special uint32
}

// IsSymlink reports whether the file mode marks this node as a symbolic link.
func (b BSDInfo) IsSymlink() bool { return b.FileMode&sIFMT == sIFLNK }

// IsRegular reports whether the file mode marks this node as a regular file.
func (b BSDInfo) IsRegular() bool { return b.FileMode&sIFMT == sIFREG }

// IsDir reports whether the file mode marks this node as a directory.
func (b BSDInfo) IsDir() bool { return b.FileMode&sIFMT == sIFDIR }

// Perm returns the permission bits, without the file-type bits.
func (b BSDInfo) Perm() uint16 { return b.FileMode & 0x0FFF }

// parseBSDInfo decodes the 16-byte HFSPlusBSDInfo at offset 32 of a catalog
// file or folder record. The caller must have length-checked to at least 48.
func parseBSDInfo(payload []byte) BSDInfo {
	return BSDInfo{
		OwnerID:    be32(payload[32:36]),
		GroupID:    be32(payload[36:40]),
		AdminFlags: payload[40],
		OwnerFlags: payload[41],
		FileMode:   be16(payload[42:44]),
		Special:    be32(payload[44:48]),
	}
}

// classifyLink determines a record's link kind from its Finder type/creator
// pair and file mode.
//
// The Finder pair is the authoritative marker: macOS sets it when creating the
// link, and it survives even when the mode bits do not. The mode check is a
// fallback for symlinks written by tools that set only the POSIX metadata.
func classifyLink(finderType, finderCreator uint32, mode uint16, isDir bool) LinkKind {
	switch {
	case finderType == finderTypeHardLink && finderCreator == finderCreatorHFSPlus:
		return LinkHardFile
	case finderType == finderTypeDirLink && finderCreator == finderCreatorMacS:
		return LinkHardDir
	case finderType == finderTypeSymlink && finderCreator == finderCreatorSymlink:
		return LinkSymbolic
	case !isDir && mode&sIFMT == sIFLNK:
		return LinkSymbolic
	}
	return LinkNone
}

// OpenCNIDRaw returns a catalog record exactly as it appears on disk, without
// resolving hard links.
//
// [Volume.OpenCNID] follows a hard link to its target and reports the target's
// metadata, which is what a filesystem consumer wants. An examiner often wants
// the other thing: the link record itself, with its own timestamps and its own
// place in the directory tree. This returns that.
func (v *Volume) OpenCNIDRaw(cnid uint32) (CatalogRecord, error) {
	return v.lookupCNIDRaw(cnid)
}

// ReadLink returns the target path of a symbolic link.
//
// HFS+ stores the target as the plain text contents of the link's data fork.
// A record that is not a symlink returns ErrNotFound; use CatalogRecord.Link
// to test first.
func (v *Volume) ReadLink(cnid uint32) (string, error) {
	rec, err := v.OpenCNIDRaw(cnid)
	if err != nil {
		return "", err
	}
	if rec.Link != LinkSymbolic {
		return "", &ParseError{Op: "read_link", Offset: int64(cnid), Err: ErrNotFound}
	}

	fh, err := v.openFileFromRecord(rec, false)
	if err != nil {
		return "", err
	}
	data, err := fh.ReadAll()
	if err != nil {
		return "", err
	}
	// Targets are stored without a terminator, but tolerate one.
	return strings.TrimRight(string(data), "\x00"), nil
}

// ReadLinkByPath is ReadLink addressed by path.
func (v *Volume) ReadLinkByPath(path string) (string, error) {
	rec, err := v.OpenPath(path)
	if err != nil {
		return "", err
	}
	return v.ReadLink(rec.CNID)
}
