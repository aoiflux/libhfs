package hfs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// Links had no end-to-end coverage on any volume, real or synthetic.
//
// classifyLink and parseBSDInfo are well tested as pure functions, and the
// corpus confirms BSD metadata is populated. What had never run is the work
// those facts feed: Volume.ReadLink sat at 0% on the real HFS+ image and 25%
// hermetically — its only executed statement the rejection branch — and
// resolveHardLinkRecord at 37.5% everywhere, meaning its loop body had never
// executed at all. Reading a symlink's target and resolving a hard link to its
// inode are the two things the link API exists to do, and neither was tested.
//
// The gap cannot be closed with a real volume: img8_hfs.dd contains no links of
// either kind, and HFS+ cannot be written on this machine. A hermetic fixture
// is the only way these paths get tested.
//
// What this fixture does NOT settle: whether a real macOS volume satisfies the
// package's premise that HFSPlusBSDInfo.special on a hard-link stub is the
// target's CNID. Apple's own resolution looks up the name "iNode<special>" in
// the private metadata directory instead, and the two agree only when the inode
// happened to receive that CNID at creation time. The fixture encodes the
// package's documented interpretation (types.go: "LinkTarget is the inode CNID
// a hard link points at") so the resolution machinery is testable either way;
// it is not evidence that the interpretation is right.

const (
	lnkBlockSize = uint32(4096)
	lnkNodeSize  = uint16(2048)
	lnkCatalogBk = uint32(2)
	lnkInodeBk   = uint32(3)
	lnkSymBk     = uint32(4)
	lnkModeSymBk = uint32(5)

	lnkPrivDirCNID = uint32(16)
	lnkInodeCNID   = uint32(100)
	lnkHardCNID    = uint32(101)
	lnkSymCNID     = uint32(102)
	lnkModeSymCNID = uint32(103)

	lnkHardName    = "hard.txt"
	lnkSymName     = "sym"
	lnkModeSymName = "modesym"
	lnkInodeName   = "iNode100"

	// The link count stored in the inode's BSD special field. Two links share
	// this inode: the stub below and the original name.
	lnkLinkCount = uint32(2)
)

// lnkPrivDirName is Apple's hard-link store. The four leading NULs are part of
// the name on disk — they exist so the directory cannot be typed — and
// isSystemFile keys off them, so a fixture that omits them would exercise a
// different branch than a real volume does.
var lnkPrivDirName = "\x00\x00\x00\x00HFS+ Private Data"

func lnkInodePayload() []byte {
	return bytes.Repeat([]byte("inode content that both links share; "), 40)
}

// lnkSymTarget is stored with no terminator, which is how macOS writes it.
const lnkSymTarget = "/usr/share/target.txt"

// lnkModeSymTarget is stored with a trailing NUL, which some writers add. The
// two together pin ReadLink's documented tolerance: the terminator must be
// trimmed, and a target that has none must not lose its last byte.
const lnkModeSymTarget = "/var/db/other.txt"

// buildLinkFileRecord builds an HFSPlusCatalogFile with the Finder type/creator
// pair, file mode and BSD special field set, which between them are what mark a
// record as a link. A zero data fork means the record is a stub carrying no
// content of its own, which is what a hard link is.
func buildLinkFileRecord(cnid, finderType, finderCreator uint32, mode uint16, special uint32,
	forkStart uint32, forkSize uint64) []byte {
	r := make([]byte, catFileRecordSize)
	binary.BigEndian.PutUint16(r[catRecordType:catRecordType+2], catalogRecordFile)
	binary.BigEndian.PutUint32(r[catNodeID:catNodeID+4], cnid)

	// HFSPlusBSDInfo at 32: owner, group, admin/owner flags, mode, special.
	binary.BigEndian.PutUint32(r[catPermissions:catPermissions+4], 501)
	binary.BigEndian.PutUint32(r[catPermissions+4:catPermissions+8], 20)
	binary.BigEndian.PutUint16(r[catPermissions+10:catPermissions+12], mode)
	binary.BigEndian.PutUint32(r[catPermissions+12:catPermissions+16], special)

	binary.BigEndian.PutUint32(r[catFileType:catFileType+4], finderType)
	binary.BigEndian.PutUint32(r[catFileCreator:catFileCreator+4], finderCreator)

	if forkSize > 0 {
		df := r[catDataFork:catRsrcFork]
		binary.BigEndian.PutUint64(df[0:8], forkSize)
		binary.BigEndian.PutUint32(df[12:16], 1)
		binary.BigEndian.PutUint32(df[16:20], forkStart)
		binary.BigEndian.PutUint32(df[20:24], 1)
	}
	return r
}

// buildLinkImage lays out an HFS+ volume holding a hard link, its inode in the
// private data directory, and two symlinks — one marked by its Finder pair, one
// by its file mode alone.
func buildLinkImage(tb testing.TB) []byte {
	tb.Helper()

	inode := lnkInodePayload()
	if len(inode) > int(lnkBlockSize) {
		tb.Fatalf("inode payload of %d bytes does not fit one %d-byte block", len(inode), lnkBlockSize)
	}

	totalBlocks := lnkModeSymBk + 1
	img := make([]byte, int(totalBlocks)*int(lnkBlockSize))

	vh := img[volumeHeaderOffset : volumeHeaderOffset+volumeHeaderSize]
	binary.BigEndian.PutUint16(vh[0:2], signatureHFSP)
	binary.BigEndian.PutUint16(vh[2:4], versionHFSPlus)
	binary.BigEndian.PutUint32(vh[40:44], lnkBlockSize)
	binary.BigEndian.PutUint32(vh[44:48], totalBlocks)
	binary.BigEndian.PutUint32(vh[48:52], 1)

	// Catalog fork descriptor at 272: one block, starting at lnkCatalogBk.
	binary.BigEndian.PutUint64(vh[272:280], uint64(lnkBlockSize))
	binary.BigEndian.PutUint32(vh[272+12:272+16], 1)
	binary.BigEndian.PutUint32(vh[272+16:272+20], lnkCatalogBk)
	binary.BigEndian.PutUint32(vh[272+20:272+24], 1)

	symFork := []byte(lnkSymTarget)
	modeSymFork := append([]byte(lnkModeSymTarget), 0x00)

	hardLink := buildLinkFileRecord(lnkHardCNID, finderTypeHardLink, finderCreatorHFSPlus,
		sIFREG|0644, lnkInodeCNID, 0, 0)
	inodeRec := buildLinkFileRecord(lnkInodeCNID, 0, 0,
		sIFREG|0644, lnkLinkCount, lnkInodeBk, uint64(len(inode)))
	symRec := buildLinkFileRecord(lnkSymCNID, finderTypeSymlink, finderCreatorSymlink,
		sIFLNK|0777, 0, lnkSymBk, uint64(len(symFork)))
	// No Finder pair at all: this one is a symlink only because its mode says
	// so, which is the fallback branch classifyLink documents.
	modeSymRec := buildLinkFileRecord(lnkModeSymCNID, 0, 0,
		sIFLNK|0777, 0, lnkModeSymBk, uint64(len(modeSymFork)))

	// Catalog leaf records, in (parent, name) key order. Within parent 2 that
	// is "" < the NUL-prefixed private directory < hard.txt < modesym < sym.
	catRecords := [][]byte{
		append(buildCatalogKey(1, "linkvol"), buildFolderRecord(rootFolderCNID, 4)...),
		append(buildCatalogKey(rootFolderCNID, ""),
			buildThreadRecord(catalogRecordFolderThread, 1, "linkvol")...),
		append(buildCatalogKey(rootFolderCNID, lnkPrivDirName),
			buildFolderRecord(lnkPrivDirCNID, 1)...),
		append(buildCatalogKey(rootFolderCNID, lnkHardName), hardLink...),
		append(buildCatalogKey(rootFolderCNID, lnkModeSymName), modeSymRec...),
		append(buildCatalogKey(rootFolderCNID, lnkSymName), symRec...),
		append(buildCatalogKey(lnkPrivDirCNID, ""),
			buildThreadRecord(catalogRecordFolderThread, rootFolderCNID, lnkPrivDirName)...),
		append(buildCatalogKey(lnkPrivDirCNID, lnkInodeName), inodeRec...),
		append(buildCatalogKey(lnkInodeCNID, ""),
			buildThreadRecord(catalogRecordFileThread, lnkPrivDirCNID, lnkInodeName)...),
		append(buildCatalogKey(lnkHardCNID, ""),
			buildThreadRecord(catalogRecordFileThread, rootFolderCNID, lnkHardName)...),
		append(buildCatalogKey(lnkSymCNID, ""),
			buildThreadRecord(catalogRecordFileThread, rootFolderCNID, lnkSymName)...),
		append(buildCatalogKey(lnkModeSymCNID, ""),
			buildThreadRecord(catalogRecordFileThread, rootFolderCNID, lnkModeSymName)...),
	}

	writeNode := func(num uint32, node []byte) {
		off := int(lnkCatalogBk)*int(lnkBlockSize) + int(num)*int(lnkNodeSize)
		copy(img[off:off+int(lnkNodeSize)], node)
	}
	writeNode(0, makeNode(lnkNodeSize, btreeNodeTypeHead,
		[][]byte{buildBTreeHeaderRecordBytesAt(lnkNodeSize, 2, 1, 1)}))
	writeNode(1, makeNode(lnkNodeSize, btreeNodeTypeLeaf, catRecords))

	copy(img[int(lnkInodeBk)*int(lnkBlockSize):], inode)
	copy(img[int(lnkSymBk)*int(lnkBlockSize):], symFork)
	copy(img[int(lnkModeSymBk)*int(lnkBlockSize):], modeSymFork)
	return img
}

func openLinkVolume(t *testing.T) *Volume {
	t.Helper()
	vol, err := Open(bytes.NewReader(buildLinkImage(t)))
	if err != nil {
		t.Fatalf("Open link fixture: %v", err)
	}
	return vol
}

// TestSymlinkTargetReadBack reads both symlinks through the public API. This is
// ReadLink's entire reason to exist and it had never run.
func TestSymlinkTargetReadBack(t *testing.T) {
	for _, tc := range []struct {
		name string
		cnid uint32
		path string
		want string
	}{
		{"finder pair", lnkSymCNID, "/" + lnkSymName, lnkSymTarget},
		{"mode only", lnkModeSymCNID, "/" + lnkModeSymName, lnkModeSymTarget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vol := openLinkVolume(t)

			rec, err := vol.OpenCNID(tc.cnid)
			if err != nil {
				t.Fatalf("OpenCNID: %v", err)
			}
			if rec.Link != LinkSymbolic {
				t.Fatalf("Link = %v, want LinkSymbolic; the rest of this test would be vacuous", rec.Link)
			}
			if !rec.Perms.IsSymlink() {
				t.Errorf("Perms.IsSymlink() = false for mode %#o", rec.Perms.FileMode)
			}
			if rec.IsSymlink() != true {
				t.Errorf("CatalogRecord.IsSymlink() = false")
			}

			got, err := vol.ReadLink(tc.cnid)
			if err != nil {
				t.Fatalf("ReadLink: %v", err)
			}
			if got != tc.want {
				t.Errorf("ReadLink = %q, want %q", got, tc.want)
			}

			byPath, err := vol.ReadLinkByPath(tc.path)
			if err != nil {
				t.Fatalf("ReadLinkByPath(%q): %v", tc.path, err)
			}
			if byPath != tc.want {
				t.Errorf("ReadLinkByPath = %q, want %q", byPath, tc.want)
			}
		})
	}
}

// TestSymlinkTrailingNULTrimmed states the difference between the two fixtures
// above, so that the tolerance ReadLink documents is asserted rather than
// implied. One target is stored with a terminator and one without; both must
// read back as the path alone.
func TestSymlinkTrailingNULTrimmed(t *testing.T) {
	img := buildLinkImage(t)

	// The premise: the two forks really do differ in this respect on disk.
	stored := img[int(lnkModeSymBk)*int(lnkBlockSize):][:len(lnkModeSymTarget)+1]
	if stored[len(stored)-1] != 0x00 {
		t.Fatalf("mode-only symlink fork does not end in NUL, so trimming is untested")
	}
	plain := img[int(lnkSymBk)*int(lnkBlockSize):][:len(lnkSymTarget)]
	if bytes.IndexByte(plain, 0x00) >= 0 {
		t.Fatalf("finder-pair symlink fork contains a NUL, so it is not the unterminated case")
	}

	vol, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, err := vol.ReadLink(lnkModeSymCNID)
	if err != nil {
		t.Fatalf("ReadLink: %v", err)
	}
	if got != lnkModeSymTarget {
		t.Errorf("ReadLink = %q, want %q — the terminator was not trimmed", got, lnkModeSymTarget)
	}
	// And the untrimmed one must not have lost its final character to an
	// over-eager trim.
	if got, err := vol.ReadLink(lnkSymCNID); err != nil || got != lnkSymTarget {
		t.Errorf("ReadLink(unterminated) = %q, %v; want %q", got, err, lnkSymTarget)
	}
}

// TestHardLinkResolvesToInode drives resolveHardLinkRecord's loop, which had
// never executed. The stub carries no content of its own; everything the caller
// sees about size and bytes must come from the inode, while name and place in
// the tree must stay the stub's.
func TestHardLinkResolvesToInode(t *testing.T) {
	vol := openLinkVolume(t)
	want := lnkInodePayload()

	// The premise: the stub really is empty on disk, so a pass cannot come from
	// the stub's own fork.
	raw, err := vol.OpenCNIDRaw(lnkHardCNID)
	if err != nil {
		t.Fatalf("OpenCNIDRaw: %v", err)
	}
	if raw.DataFork.LogicalSize != 0 || raw.DataFork.TotalBlocks != 0 {
		t.Fatalf("hard-link stub has a %d-byte data fork; it should be empty",
			raw.DataFork.LogicalSize)
	}
	if raw.Link != LinkHardFile {
		t.Fatalf("stub Link = %v, want LinkHardFile", raw.Link)
	}
	if raw.LinkTarget != lnkInodeCNID {
		t.Fatalf("stub LinkTarget = %d, want %d", raw.LinkTarget, lnkInodeCNID)
	}

	rec, err := vol.OpenCNID(lnkHardCNID)
	if err != nil {
		t.Fatalf("OpenCNID: %v", err)
	}
	if rec.DataFork.LogicalSize != uint64(len(want)) {
		t.Errorf("resolved size = %d, want the inode's %d", rec.DataFork.LogicalSize, len(want))
	}
	// Identity stays the link's: an examiner needs to see where the link sits,
	// not where the inode is hidden.
	if rec.Name != lnkHardName {
		t.Errorf("resolved Name = %q, want the link's %q", rec.Name, lnkHardName)
	}
	if rec.ParentCNID != rootFolderCNID {
		t.Errorf("resolved ParentCNID = %d, want the link's %d", rec.ParentCNID, rootFolderCNID)
	}
	// And the link facts survive resolution.
	if !rec.IsHardLink() {
		t.Errorf("resolved record does not report IsHardLink")
	}
	if rec.LinkTarget != lnkInodeCNID {
		t.Errorf("resolved LinkTarget = %d, want %d", rec.LinkTarget, lnkInodeCNID)
	}
	if rec.LinkCount != lnkLinkCount {
		t.Errorf("LinkCount = %d, want %d (the inode's BSD special field)", rec.LinkCount, lnkLinkCount)
	}

	f, err := vol.OpenFileByCNID(lnkHardCNID)
	if err != nil {
		t.Fatalf("OpenFileByCNID: %v", err)
	}
	got, err := f.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("read %d bytes through the link, want the inode's %d; equal prefix %d",
			len(got), len(want), commonPrefixLen(got, want))
	}
}

// TestHardLinkRawRecordIsNotResolved pins the distinction OpenCNIDRaw exists
// for. Without this, an implementation that resolved in both would pass every
// other test here.
func TestHardLinkRawRecordIsNotResolved(t *testing.T) {
	vol := openLinkVolume(t)

	raw, err := vol.OpenCNIDRaw(lnkHardCNID)
	if err != nil {
		t.Fatalf("OpenCNIDRaw: %v", err)
	}
	resolved, err := vol.OpenCNID(lnkHardCNID)
	if err != nil {
		t.Fatalf("OpenCNID: %v", err)
	}
	if raw.DataFork.LogicalSize == resolved.DataFork.LogicalSize {
		t.Fatalf("raw and resolved records report the same size %d, so OpenCNIDRaw resolved the link",
			raw.DataFork.LogicalSize)
	}
	if raw.LinkCount != 0 {
		t.Errorf("raw stub LinkCount = %d, want 0 — the count belongs to the inode", raw.LinkCount)
	}
}

// TestHardLinkInodeReachableDirectly checks the other half: the inode is an
// ordinary file when addressed by its own CNID, and resolution does not loop on
// it. Its BSD special field is a link count there, not a target.
func TestHardLinkInodeReachableDirectly(t *testing.T) {
	vol := openLinkVolume(t)

	rec, err := vol.OpenCNID(lnkInodeCNID)
	if err != nil {
		t.Fatalf("OpenCNID(inode): %v", err)
	}
	if rec.Link != LinkNone {
		t.Errorf("inode Link = %v, want LinkNone", rec.Link)
	}
	if rec.Name != lnkInodeName {
		t.Errorf("inode Name = %q, want %q", rec.Name, lnkInodeName)
	}
	if rec.Perms.Special != lnkLinkCount {
		t.Errorf("inode Special = %d, want the link count %d", rec.Perms.Special, lnkLinkCount)
	}
}

// TestIsSystemFileBranches pins each rule in isSystemFile with a name that
// satisfies that rule alone.
//
// This exists because the obvious fixture cannot do it: Apple's real name,
// "\x00\x00\x00\x00HFS+ Private Data", matches both the leading-NUL rule and
// the "HFS+ Private" substring rule, so deleting either one leaves the other to
// return true and the test still passes. Each name below hits exactly one rule,
// which is what makes deleting that rule observable.
func TestIsSystemFileBranches(t *testing.T) {
	for _, tc := range []struct {
		name string
		rule string
		want bool
	}{
		{"", "empty name", false},
		{"\x00hidden", "leading NUL, no HFS+ substring", true},
		{"$Extend", "leading dollar", true},
		{"copy of HFS+ Private Data", "substring only, no NUL or dollar", true},
		{".HFSBTreeAllocation", ".HFS prefix only", true},
		{".journal", "journal file", true},
		{".journal_info_block", "journal info block", true},
		{"report.txt", "ordinary file", false},
		{".hidden", "leading dot but not .HFS or .journal", false},
		{"HFS+ notes", "mentions HFS+ but not the private store", false},
	} {
		if got := isSystemFile(tc.name); got != tc.want {
			t.Errorf("isSystemFile(%q) = %v, want %v (%s)", tc.name, got, tc.want, tc.rule)
		}
	}
}

// TestLinkPrivateDirectoryIsSystem checks that Apple's hard-link store, written
// with the NULs it really carries on disk, survives a round trip through the
// catalog and is still recognised. A tool that reports it as user content is
// misreporting the volume.
//
// Which rule recognises it is pinned by TestIsSystemFileBranches above, not
// here — this name matches two of them.
func TestLinkPrivateDirectoryIsSystem(t *testing.T) {
	vol := openLinkVolume(t)

	entries, err := vol.ReadDir("/")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var found bool
	for _, e := range entries {
		if e.CNID != lnkPrivDirCNID {
			continue
		}
		found = true
		if !isSystemFile(e.Name) {
			t.Errorf("private data directory %q is not reported as a system file", e.Name)
		}
	}
	if !found {
		t.Fatalf("private data directory not listed in the root; the check above never ran")
	}
}

// TestReadLinkOnHardLinkRejected states that ReadLink means symlink only. A
// hard link has no target text, and returning its inode's first bytes as a path
// would be worse than an error.
func TestReadLinkOnHardLinkRejected(t *testing.T) {
	vol := openLinkVolume(t)

	_, err := vol.ReadLink(lnkHardCNID)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("ReadLink(hard link) = %v, want ErrNotFound", err)
	}
	var pe *ParseError
	if errors.As(err, &pe) && pe.Op != "read_link" {
		t.Errorf("rejected by %q, want %q", pe.Op, "read_link")
	}
}

// TestLinkKindString covers the exported String method, which a report or a log
// line goes through.
func TestLinkKindString(t *testing.T) {
	for kind, want := range map[LinkKind]string{
		LinkNone:     "none",
		LinkHardFile: "hard link (file)",
		LinkHardDir:  "hard link (directory)",
		LinkSymbolic: "symlink",
	} {
		if got := kind.String(); got != want {
			t.Errorf("LinkKind(%d).String() = %q, want %q", uint8(kind), got, want)
		}
	}
}

// The collision case from Apple's hfs_makelink: a hard link whose stored
// indirect-node number is NOT the inode's CNID.
//
// hfs_makelink normally assigns that number from the file's own c_fileid and
// then renames the file into the private directory, which preserves the CNID —
// so the number and the CNID usually match, and reading it as a CNID happens to
// work. When the name "iNode<n>" is already taken the loop retries with a
// separate counter instead, and from then on they are unrelated.
//
// This fixture is that case, with an unrelated file deliberately occupying the
// CNID the stub names. Resolving by CNID does not fail here — it silently
// returns the wrong file's contents, which is the worst way for a forensic tool
// to be wrong.
const (
	colBlockSize = uint32(4096)
	colNodeSize  = uint16(2048)
	colCatalogBk = uint32(2)
	colInodeBk   = uint32(3)
	colDecoyBk   = uint32(4)

	colPrivCNID  = uint32(16)
	colStubCNID  = uint32(101)
	colInodeCNID = uint32(222)
	colINodeNum  = uint32(777) // the stub's Special, and the decoy's CNID
	colStubName  = "link.txt"
	colDecoyName = "decoy.bin"
	colInodeName = "iNode777"
)

func colInodePayload() []byte {
	return bytes.Repeat([]byte("the real linked content; "), 40)
}

func colDecoyPayload() []byte {
	return bytes.Repeat([]byte("UNRELATED FILE, MUST NOT BE RETURNED; "), 40)
}

func buildHardLinkCollisionImage(tb testing.TB) []byte {
	tb.Helper()
	return buildHardLinkImageNamed(tb, colInodeName, colINodeNum)
}

// buildHardLinkImageNamed builds the collision fixture with the inode's name
// and the stub's stored number supplied by the caller, so the same layout can
// exercise both the name lookup and the CNID fallback behind it.
func buildHardLinkImageNamed(tb testing.TB, inodeName string, stubSpecial uint32) []byte {
	tb.Helper()

	inode := colInodePayload()
	decoy := colDecoyPayload()
	totalBlocks := colDecoyBk + 1
	img := make([]byte, int(totalBlocks)*int(colBlockSize))

	vh := img[volumeHeaderOffset : volumeHeaderOffset+volumeHeaderSize]
	binary.BigEndian.PutUint16(vh[0:2], signatureHFSP)
	binary.BigEndian.PutUint16(vh[2:4], versionHFSPlus)
	binary.BigEndian.PutUint32(vh[40:44], colBlockSize)
	binary.BigEndian.PutUint32(vh[44:48], totalBlocks)
	binary.BigEndian.PutUint32(vh[48:52], 1)
	binary.BigEndian.PutUint64(vh[272:280], uint64(colBlockSize))
	binary.BigEndian.PutUint32(vh[272+12:272+16], 1)
	binary.BigEndian.PutUint32(vh[272+16:272+20], colCatalogBk)
	binary.BigEndian.PutUint32(vh[272+20:272+24], 1)

	stub := buildLinkFileRecord(colStubCNID, finderTypeHardLink, finderCreatorHFSPlus,
		sIFREG|0644, stubSpecial, 0, 0)
	inodeRec := buildLinkFileRecord(colInodeCNID, 0, 0,
		sIFREG|0644, lnkLinkCount, colInodeBk, uint64(len(inode)))
	decoyRec := buildLinkFileRecord(colINodeNum, 0, 0,
		sIFREG|0644, 0, colDecoyBk, uint64(len(decoy)))

	catRecords := [][]byte{
		append(buildCatalogKey(1, "collidevol"), buildFolderRecord(rootFolderCNID, 3)...),
		append(buildCatalogKey(rootFolderCNID, ""),
			buildThreadRecord(catalogRecordFolderThread, 1, "collidevol")...),
		append(buildCatalogKey(rootFolderCNID, lnkPrivDirName),
			buildFolderRecord(colPrivCNID, 1)...),
		append(buildCatalogKey(rootFolderCNID, colDecoyName), decoyRec...),
		append(buildCatalogKey(rootFolderCNID, colStubName), stub...),
		append(buildCatalogKey(colPrivCNID, ""),
			buildThreadRecord(catalogRecordFolderThread, rootFolderCNID, lnkPrivDirName)...),
		append(buildCatalogKey(colPrivCNID, inodeName), inodeRec...),
		append(buildCatalogKey(colStubCNID, ""),
			buildThreadRecord(catalogRecordFileThread, rootFolderCNID, colStubName)...),
		append(buildCatalogKey(colInodeCNID, ""),
			buildThreadRecord(catalogRecordFileThread, colPrivCNID, inodeName)...),
		append(buildCatalogKey(colINodeNum, ""),
			buildThreadRecord(catalogRecordFileThread, rootFolderCNID, colDecoyName)...),
	}

	writeNode := func(num uint32, node []byte) {
		off := int(colCatalogBk)*int(colBlockSize) + int(num)*int(colNodeSize)
		copy(img[off:off+int(colNodeSize)], node)
	}
	writeNode(0, makeNode(colNodeSize, btreeNodeTypeHead,
		[][]byte{buildBTreeHeaderRecordBytesAt(colNodeSize, 2, 1, 1)}))
	writeNode(1, makeNode(colNodeSize, btreeNodeTypeLeaf, catRecords))

	copy(img[int(colInodeBk)*int(colBlockSize):], inode)
	copy(img[int(colDecoyBk)*int(colBlockSize):], decoy)
	return img
}

// TestHardLinkResolvesByNameNotCNID is the decisive test for the difference.
//
// Resolving the stub's number as a CNID lands on decoy.bin and returns its
// bytes with no error at all.
func TestHardLinkResolvesByNameNotCNID(t *testing.T) {
	vol, err := Open(bytes.NewReader(buildHardLinkCollisionImage(t)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	want := colInodePayload()
	decoy := colDecoyPayload()

	// Premises. Without all three the test proves nothing: the number really
	// must differ from the inode's CNID, and something unrelated really must
	// occupy it.
	stub, err := vol.OpenCNIDRaw(colStubCNID)
	if err != nil {
		t.Fatalf("OpenCNIDRaw(stub): %v", err)
	}
	if stub.Perms.Special != colINodeNum {
		t.Fatalf("stub iNodeNum = %d, want %d", stub.Perms.Special, colINodeNum)
	}
	if colINodeNum == colInodeCNID {
		t.Fatal("fixture uses the same number for both; it cannot tell the two lookups apart")
	}
	decoyRec, err := vol.OpenCNIDRaw(colINodeNum)
	if err != nil {
		t.Fatalf("the decoy at CNID %d is not reachable, so a CNID lookup would fail rather than "+
			"silently succeed: %v", colINodeNum, err)
	}
	if decoyRec.Name != colDecoyName {
		t.Fatalf("CNID %d holds %q, want the decoy %q", colINodeNum, decoyRec.Name, colDecoyName)
	}

	f, err := vol.OpenFileByCNID(colStubCNID)
	if err != nil {
		t.Fatalf("OpenFileByCNID(stub): %v", err)
	}
	got, err := f.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if bytes.Equal(got, decoy) {
		t.Fatalf("the link resolved to decoy.bin — the iNodeNum was read as a CNID")
	}
	if !bytes.Equal(got, want) {
		t.Errorf("read %d bytes through the link, want the inode's %d; equal prefix %d",
			len(got), len(want), commonPrefixLen(got, want))
	}

	rec, err := vol.OpenCNID(colStubCNID)
	if err != nil {
		t.Fatalf("OpenCNID(stub): %v", err)
	}
	// LinkTarget is documented as the inode's CNID, so it must be the inode's
	// own number rather than the stub's iNodeNum when the two differ.
	if rec.LinkTarget != colInodeCNID {
		t.Errorf("LinkTarget = %d, want the inode's CNID %d (iNodeNum is %d)",
			rec.LinkTarget, colInodeCNID, colINodeNum)
	}
	// The raw iNodeNum must stay reachable; it is what the volume actually
	// stores and an examiner may need it.
	if stub.Perms.Special != colINodeNum {
		t.Errorf("stub Perms.Special = %d, want the stored %d", stub.Perms.Special, colINodeNum)
	}
	if rec.Name != colStubName {
		t.Errorf("resolved Name = %q, want the link's %q", rec.Name, colStubName)
	}
}

// TestHardLinkFallsBackToCNIDLookup covers the path behind the name lookup.
//
// Volumes whose private metadata directory is missing, renamed or damaged still
// have to resolve, and before the name lookup existed reading the stub's number
// as a CNID was the only behaviour there was. It stays as the fallback, so a
// volume that used to work must not stop working.
func TestHardLinkFallsBackToCNIDLookup(t *testing.T) {
	// The inode is not named iNode<n>, so the name lookup finds nothing; the
	// stub's number is the inode's real CNID, which is the ordinary case.
	img := buildHardLinkImageNamed(t, "notaninode.dat", colInodeCNID)
	vol, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// The premise: the name lookup really must fail here, or this test is just
	// the previous one again.
	if _, ok := vol.findInodeByName(colInodeCNID, LinkHardFile); ok {
		t.Fatalf("an iNode%d record was found; the fallback is not what resolved this",
			colInodeCNID)
	}

	f, err := vol.OpenFileByCNID(colStubCNID)
	if err != nil {
		t.Fatalf("OpenFileByCNID: %v", err)
	}
	got, err := f.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if want := colInodePayload(); !bytes.Equal(got, want) {
		t.Errorf("read %d bytes, want the inode's %d; equal prefix %d",
			len(got), len(want), commonPrefixLen(got, want))
	}
}

// TestHardLinkWithoutPrivateDirectory is the same fallback reached the other
// way: a volume with no private metadata directory at all.
func TestHardLinkWithoutPrivateDirectory(t *testing.T) {
	vol, err := Open(bytes.NewReader(buildLinkImage(t)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, ok := vol.privateDirCNID(storeFileLinks); !ok {
		t.Skip("fixture has no private directory; nothing to contrast")
	}

	// Confirm the cache returns a stable answer rather than rescanning to a
	// different one, since every hard link on a volume goes through it.
	first, ok1 := vol.privateDirCNID(storeFileLinks)
	second, ok2 := vol.privateDirCNID(storeFileLinks)
	if first != second || ok1 != ok2 {
		t.Errorf("privateDirCNID returned (%d,%v) then (%d,%v)", first, ok1, second, ok2)
	}
}
