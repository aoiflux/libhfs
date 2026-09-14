package libhfs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// Directory hard links.
//
// These were not resolved at all until 2026-09-13: hardLinkTargetCNID returned
// 0 for anything that was not LinkHardFile, so a Time Machine volume's
// directory links reported the zero-length stub where a directory should be.
//
// The fixture here is the smallest volume that has *both* private stores, so
// it also pins the rule that a link reference valid in one store means nothing
// in the other, and the rule that separates a directory hard link from an
// ordinary Finder alias carrying the same Finder type and creator.

const (
	dlBlockSize = uint32(4096)
	// One node per block, and two blocks of catalog: the leaf carries every
	// record this fixture needs and they do not fit in 2048 bytes.
	dlNodeSize   = uint16(4096)
	dlCatalogBk  = uint32(2)
	dlCatalogBks = uint32(2)
	dlChildBk    = uint32(4)
	dlFileInoBk  = uint32(5)

	dlFilePrivCNID = uint32(16) // the file store
	dlDirPrivCNID  = uint32(17) // the directory store
	dlStubCNID     = uint32(101)
	dlAliasCNID    = uint32(102)
	dlFileStubCNID = uint32(103)
	dlDirInodeCNID = uint32(300)
	dlChildCNID    = uint32(301)
	dlFileInoCNID  = uint32(400)

	// dlLinkRef is the link reference both stubs carry. The same number names
	// "dir_555" in the directory store and "iNode555" in the file store, and
	// the fixture puts a file inode at the second one, so resolving a
	// directory link through the wrong store visibly returns a file.
	dlLinkRef = uint32(555)

	dlStubName     = "photos"
	dlAliasName    = "shortcut"
	dlFileStubName = "filelink.txt"
	dlChildName    = "inside.txt"
	dlDirInodeName = "dir_555"
	dlFileInoName  = "iNode555"

	dlDirLinkCount = uint32(2)

	// dlOrphanRef names nothing on the volume. It is what the file inode
	// points at in the adversarial case, so that following one hop further
	// would fail loudly rather than quietly land somewhere plausible.
	dlOrphanRef = uint32(999)
)

// dlOpts varies the fixture without changing its shape, so each test can state
// the one difference it depends on.
type dlOpts struct {
	// stubFlags is the directory link stub's record flags. Without
	// hfsHasLinkChainMask the stub is an ordinary Finder alias.
	stubFlags uint16

	// omitFileStore drops the file-link private directory, leaving a volume
	// that has only the directory store. The file store must then be reported
	// as absent rather than resolving to the directory store, whose name
	// contains the same "HFS+ Private" substring.
	omitFileStore bool

	// fileInodeIsLink marks the file inode as a hard link in its own right,
	// which no volume macOS writes ever does. Resolution must stop at it.
	fileInodeIsLink bool
}

// dlDirPrivName is HFSPLUS_DIR_METADATA_FOLDER. Unlike the file store it has
// no NUL prefix — a leading dot and a trailing carriage return are what keep
// it out of reach — so a fixture that reused the file store's name would not
// exercise the branch that tells the two apart. The terminator is spelled out
// rather than escaped so that it survives being copied between files.
var dlDirPrivName = ".HFS+ Private Directory Data" + string(rune(0x0D))

func dlChildPayload() []byte {
	return bytes.Repeat([]byte("a file inside the linked directory; "), 30)
}

func dlFileInodePayload() []byte {
	return bytes.Repeat([]byte("WRONG STORE: this is iNode555, a file; "), 30)
}

// withRecordFlags stamps the catalog record's flags field, which is what
// separates a directory hard link from an ordinary Finder alias.
func withRecordFlags(rec []byte, flags uint16) []byte {
	binary.BigEndian.PutUint16(rec[catFlags:catFlags+2], flags)
	return rec
}

// buildDirLinkFolderRecord builds a folder record with its BSD special field
// set. On a directory inode that field is the link count: Apple's getbsdattr
// reads bsd->special.linkCount for S_IFDIR exactly as it does for S_IFREG.
func buildDirLinkFolderRecord(cnid, valence uint32, mode uint16, special uint32) []byte {
	r := buildFolderRecord(cnid, valence)
	binary.BigEndian.PutUint32(r[catPermissions:catPermissions+4], 501)
	binary.BigEndian.PutUint32(r[catPermissions+4:catPermissions+8], 20)
	binary.BigEndian.PutUint16(r[catPermissions+10:catPermissions+12], mode)
	binary.BigEndian.PutUint32(r[catPermissions+12:catPermissions+16], special)
	return r
}

// buildDirLinkImage lays out a volume with both private stores, a directory
// hard link, a Finder alias that differs from it only by the link-chain flag,
// and a file inode whose reference number collides with the directory inode's.
//
// stubFlags is supplied by the caller so the same layout also produces the
// case where the link-chain bit is absent.
func buildDirLinkImage(tb testing.TB, opts dlOpts) []byte {
	tb.Helper()

	child := dlChildPayload()
	fileIno := dlFileInodePayload()
	totalBlocks := dlFileInoBk + 1
	img := make([]byte, int(totalBlocks)*int(dlBlockSize))

	vh := img[volumeHeaderOffset : volumeHeaderOffset+volumeHeaderSize]
	binary.BigEndian.PutUint16(vh[0:2], signatureHFSP)
	binary.BigEndian.PutUint16(vh[2:4], versionHFSPlus)
	binary.BigEndian.PutUint32(vh[40:44], dlBlockSize)
	binary.BigEndian.PutUint32(vh[44:48], totalBlocks)
	binary.BigEndian.PutUint32(vh[48:52], 1)
	binary.BigEndian.PutUint64(vh[272:280], uint64(dlBlockSize*dlCatalogBks))
	binary.BigEndian.PutUint32(vh[272+12:272+16], dlCatalogBks)
	binary.BigEndian.PutUint32(vh[272+16:272+20], dlCatalogBk)
	binary.BigEndian.PutUint32(vh[272+20:272+24], dlCatalogBks)

	// The stub: a file record carrying the alias Finder pair. S_IFREG is not a
	// fixture simplification — createindirectlink sets exactly that mode for a
	// link to a directory.
	stub := withRecordFlags(
		buildLinkFileRecord(dlStubCNID, finderTypeDirLink, finderCreatorMacS,
			sIFREG|0644, dlLinkRef, 0, 0),
		opts.stubFlags)

	// A file hard link carrying the same reference number as the directory
	// link, so that both kinds are live on one volume and each has a
	// same-numbered object waiting in the other one's store.
	fileStub := withRecordFlags(
		buildLinkFileRecord(dlFileStubCNID, finderTypeHardLink, finderCreatorHFSPlus,
			sIFREG|0644, dlLinkRef, 0, 0),
		hfsHasLinkChainMask)

	// The same Finder pair with no link-chain bit: a plain alias document.
	alias := buildLinkFileRecord(dlAliasCNID, finderTypeDirLink, finderCreatorMacS,
		sIFREG|0644, dlLinkRef, 0, 0)

	dirInode := buildDirLinkFolderRecord(dlDirInodeCNID, 1, sIFDIR|0755, dlDirLinkCount)
	childRec := buildLinkFileRecord(dlChildCNID, 0, 0,
		sIFREG|0644, 0, dlChildBk, uint64(len(child)))
	fileInoType, fileInoCreator := uint32(0), uint32(0)
	fileInoSpecial := lnkLinkCount
	if opts.fileInodeIsLink {
		fileInoType, fileInoCreator = finderTypeHardLink, finderCreatorHFSPlus
		fileInoSpecial = dlOrphanRef
	}
	fileInoRec := buildLinkFileRecord(dlFileInoCNID, fileInoType, fileInoCreator,
		sIFREG|0644, fileInoSpecial, dlFileInoBk, uint64(len(fileIno)))

	rootValence := uint32(5)
	if opts.omitFileStore {
		rootValence = 4
	}

	// Records go in catalog key order — parent CNID ascending, then name —
	// because that is what the B-tree's ordered descent expects. Out of order
	// they are still found, but only through the linear fallback, which would
	// mean these tests quietly stopped exercising the real search path. The
	// file store sorts first among the root's children: its name begins with
	// four NULs.
	type catRec struct {
		key  []byte
		body []byte
		skip bool
	}
	entries := []catRec{
		{buildCatalogKey(1, "dirlinkvol"), buildFolderRecord(rootFolderCNID, rootValence), false},

		{buildCatalogKey(rootFolderCNID, ""),
			buildThreadRecord(catalogRecordFolderThread, 1, "dirlinkvol"), false},
		{buildCatalogKey(rootFolderCNID, lnkPrivDirName),
			buildFolderRecord(dlFilePrivCNID, 1), opts.omitFileStore},
		{buildCatalogKey(rootFolderCNID, dlDirPrivName),
			buildFolderRecord(dlDirPrivCNID, 1), false},
		{buildCatalogKey(rootFolderCNID, dlFileStubName), fileStub, false},
		{buildCatalogKey(rootFolderCNID, dlStubName), stub, false},
		{buildCatalogKey(rootFolderCNID, dlAliasName), alias, false},

		{buildCatalogKey(dlFilePrivCNID, ""),
			buildThreadRecord(catalogRecordFolderThread, rootFolderCNID, lnkPrivDirName),
			opts.omitFileStore},
		{buildCatalogKey(dlFilePrivCNID, dlFileInoName), fileInoRec, opts.omitFileStore},

		{buildCatalogKey(dlDirPrivCNID, ""),
			buildThreadRecord(catalogRecordFolderThread, rootFolderCNID, dlDirPrivName), false},
		{buildCatalogKey(dlDirPrivCNID, dlDirInodeName), dirInode, false},

		{buildCatalogKey(dlStubCNID, ""),
			buildThreadRecord(catalogRecordFileThread, rootFolderCNID, dlStubName), false},
		{buildCatalogKey(dlAliasCNID, ""),
			buildThreadRecord(catalogRecordFileThread, rootFolderCNID, dlAliasName), false},
		{buildCatalogKey(dlFileStubCNID, ""),
			buildThreadRecord(catalogRecordFileThread, rootFolderCNID, dlFileStubName), false},

		{buildCatalogKey(dlDirInodeCNID, ""),
			buildThreadRecord(catalogRecordFolderThread, dlDirPrivCNID, dlDirInodeName), false},
		{buildCatalogKey(dlDirInodeCNID, dlChildName), childRec, false},

		{buildCatalogKey(dlChildCNID, ""),
			buildThreadRecord(catalogRecordFileThread, dlDirInodeCNID, dlChildName), false},
		{buildCatalogKey(dlFileInoCNID, ""),
			buildThreadRecord(catalogRecordFileThread, dlFilePrivCNID, dlFileInoName),
			opts.omitFileStore},
	}

	catRecords := make([][]byte, 0, len(entries))
	for _, e := range entries {
		if e.skip {
			continue
		}
		catRecords = append(catRecords, append(e.key, e.body...))
	}

	writeNode := func(num uint32, node []byte) {
		off := int(dlCatalogBk)*int(dlBlockSize) + int(num)*int(dlNodeSize)
		copy(img[off:off+int(dlNodeSize)], node)
	}
	writeNode(0, makeNode(dlNodeSize, btreeNodeTypeHead,
		[][]byte{buildBTreeHeaderRecordBytesAt(dlNodeSize, 2, 1, 1)}))
	writeNode(1, makeNode(dlNodeSize, btreeNodeTypeLeaf, catRecords))

	copy(img[int(dlChildBk)*int(dlBlockSize):], child)
	copy(img[int(dlFileInoBk)*int(dlBlockSize):], fileIno)
	return img
}

func openDirLinkVolume(tb testing.TB, stubFlags uint16) *Volume {
	tb.Helper()
	return openDirLinkVolumeOpts(tb, dlOpts{stubFlags: stubFlags})
}

func openDirLinkVolumeOpts(tb testing.TB, opts dlOpts) *Volume {
	tb.Helper()
	vol, err := Open(bytes.NewReader(buildDirLinkImage(tb, opts)))
	if err != nil {
		tb.Fatalf("Open: %v", err)
	}
	return vol
}

// TestDirectoryHardLinkClassified pins both halves of the rule: the stub is a
// directory link, and the record differing only by the link-chain flag is not
// a link at all.
func TestDirectoryHardLinkClassified(t *testing.T) {
	vol := openDirLinkVolume(t, hfsHasLinkChainMask)

	stub, err := vol.OpenCNIDRaw(dlStubCNID)
	if err != nil {
		t.Fatalf("OpenCNIDRaw(stub): %v", err)
	}
	if stub.Link != LinkHardDir {
		t.Errorf("stub Link = %v, want %v", stub.Link, LinkHardDir)
	}
	if stub.Type != CatalogRecordFile {
		t.Errorf("stub Type = %v, want %v — a directory link stub is a file record",
			stub.Type, CatalogRecordFile)
	}
	if stub.LinkTarget != dlLinkRef {
		t.Errorf("stub LinkTarget = %d, want the link reference %d",
			stub.LinkTarget, dlLinkRef)
	}
	if !stub.IsHardLink() {
		t.Error("IsHardLink() = false on a directory hard link")
	}

	alias, err := vol.OpenCNIDRaw(dlAliasCNID)
	if err != nil {
		t.Fatalf("OpenCNIDRaw(alias): %v", err)
	}
	if alias.Link != LinkNone {
		t.Errorf("alias Link = %v, want %v — a Finder alias carries the same "+
			"type/creator pair but no link chain", alias.Link, LinkNone)
	}
	if alias.LinkTarget != 0 {
		t.Errorf("alias LinkTarget = %d, want 0", alias.LinkTarget)
	}
}

// TestDirectoryHardLinkResolvesToInode is the gap this closes. Before it, the
// stub resolved to nothing and OpenCNID handed back the zero-length file
// record itself.
func TestDirectoryHardLinkResolvesToInode(t *testing.T) {
	vol := openDirLinkVolume(t, hfsHasLinkChainMask)

	rec, err := vol.OpenCNID(dlStubCNID)
	if err != nil {
		t.Fatalf("OpenCNID(stub): %v", err)
	}
	if !rec.IsDirectory() {
		t.Fatalf("resolved record Type = %v, want a folder", rec.Type)
	}
	if rec.CNID != dlDirInodeCNID {
		t.Errorf("resolved CNID = %d, want the directory inode %d",
			rec.CNID, dlDirInodeCNID)
	}
	if rec.Name != dlStubName {
		t.Errorf("resolved Name = %q, want the link's own name %q",
			rec.Name, dlStubName)
	}
	if rec.ParentCNID != rootFolderCNID {
		t.Errorf("resolved ParentCNID = %d, want the link's parent %d",
			rec.ParentCNID, rootFolderCNID)
	}
	if rec.Link != LinkHardDir {
		t.Errorf("resolved Link = %v, want %v — the link fact must survive "+
			"resolution", rec.Link, LinkHardDir)
	}
	if rec.LinkTarget != dlDirInodeCNID {
		t.Errorf("resolved LinkTarget = %d, want %d", rec.LinkTarget, dlDirInodeCNID)
	}
	if rec.LinkCount != dlDirLinkCount {
		t.Errorf("resolved LinkCount = %d, want %d", rec.LinkCount, dlDirLinkCount)
	}
	if rec.Valence != 1 {
		t.Errorf("resolved Valence = %d, want the inode's 1", rec.Valence)
	}
}

// TestDirectoryHardLinkListsTarget checks the listing follows through, by CNID
// and by path. Reading children from the stub's own CNID reports an empty
// directory rather than an error, which is why the walker resolves first.
func TestDirectoryHardLinkListsTarget(t *testing.T) {
	vol := openDirLinkVolume(t, hfsHasLinkChainMask)

	for _, tc := range []struct {
		name string
		list func() ([]DirEntry, error)
	}{
		{"by CNID", func() ([]DirEntry, error) { return vol.ReadDirCNID(dlStubCNID) }},
		{"by path", func() ([]DirEntry, error) { return vol.ReadDir("/" + dlStubName) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ents, err := tc.list()
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(ents) != 1 {
				t.Fatalf("got %d entries %v, want just %q", len(ents), ents, dlChildName)
			}
			if ents[0].Name != dlChildName || ents[0].CNID != dlChildCNID {
				t.Errorf("entry = %+v, want %q/%d", ents[0], dlChildName, dlChildCNID)
			}
		})
	}

	// And the child is readable through the link, which is the only end-to-end
	// proof that the resolved CNID addresses real content.
	f, err := vol.OpenFileByPath("/" + dlStubName + "/" + dlChildName)
	if err != nil {
		t.Fatalf("OpenFileByPath through the link: %v", err)
	}
	got, err := f.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if want := dlChildPayload(); !bytes.Equal(got, want) {
		t.Errorf("read %d bytes, want %d", len(got), len(want))
	}
}

// TestDirectoryHardLinkUsesDirectoryStore is the counterpart of
// TestHardLinkResolvesByNameNotCNID for directories: the same reference number
// names an object in both private stores, and picking the wrong one returns a
// file where a directory belongs.
func TestDirectoryHardLinkUsesDirectoryStore(t *testing.T) {
	vol := openDirLinkVolume(t, hfsHasLinkChainMask)

	// The premise: both stores exist, they are different directories, and each
	// really does hold an object named for dlLinkRef.
	fileStore, ok := vol.privateDirCNID(storeFileLinks)
	if !ok {
		t.Fatal("no file-link store found; the fixture is not testing what it claims")
	}
	dirStore, ok := vol.privateDirCNID(storeDirLinks)
	if !ok {
		t.Fatal("no directory-link store found; the fixture is not testing what it claims")
	}
	if fileStore == dirStore {
		t.Fatalf("both stores resolved to CNID %d; the two names were not told apart",
			fileStore)
	}
	if fileStore != dlFilePrivCNID || dirStore != dlDirPrivCNID {
		t.Fatalf("stores = file %d, dir %d; want %d and %d",
			fileStore, dirStore, dlFilePrivCNID, dlDirPrivCNID)
	}
	if _, ok := vol.findInodeByName(dlLinkRef, LinkHardFile); !ok {
		t.Fatalf("no iNode%d in the file store; the wrong-store case cannot fire",
			dlLinkRef)
	}

	// The decisive part: resolution lands on the folder, not on iNode555.
	rec, err := vol.OpenCNID(dlStubCNID)
	if err != nil {
		t.Fatalf("OpenCNID: %v", err)
	}
	if rec.CNID == dlFileInoCNID {
		t.Fatalf("the link resolved to iNode%d — the directory link was looked "+
			"up in the file store", dlLinkRef)
	}
	if rec.CNID != dlDirInodeCNID {
		t.Fatalf("resolved CNID = %d, want %d", rec.CNID, dlDirInodeCNID)
	}
}

// TestFinderAliasIsNotResolved keeps an ordinary alias document readable as
// itself. Resolving it would send a reader to a private directory for an inode
// that was never created.
func TestFinderAliasIsNotResolved(t *testing.T) {
	vol := openDirLinkVolume(t, hfsHasLinkChainMask)

	rec, err := vol.OpenCNID(dlAliasCNID)
	if err != nil {
		t.Fatalf("OpenCNID(alias): %v", err)
	}
	if rec.CNID != dlAliasCNID {
		t.Errorf("alias resolved to CNID %d; it must resolve to itself (%d)",
			rec.CNID, dlAliasCNID)
	}
	if rec.Type != CatalogRecordFile {
		t.Errorf("alias Type = %v, want a file", rec.Type)
	}
	if _, err := vol.ReadDirCNID(dlAliasCNID); !errors.Is(err, ErrNotDir) {
		t.Errorf("ReadDirCNID(alias) error = %v, want ErrNotDir", err)
	}
}

// TestDirLinkStubWithoutChainBitIsInert is the flag's own regression test: the
// identical layout with the bit cleared must behave like the alias case
// throughout, not merely classify differently.
func TestDirLinkStubWithoutChainBitIsInert(t *testing.T) {
	vol := openDirLinkVolume(t, 0)

	rec, err := vol.OpenCNID(dlStubCNID)
	if err != nil {
		t.Fatalf("OpenCNID: %v", err)
	}
	if rec.CNID != dlStubCNID {
		t.Errorf("resolved CNID = %d, want the stub itself (%d) — without the "+
			"link-chain bit this is an alias document", rec.CNID, dlStubCNID)
	}
	if rec.Link != LinkNone {
		t.Errorf("Link = %v, want %v", rec.Link, LinkNone)
	}
}

// TestPrivateStoresAreSystemFiles keeps both stores out of user listings. The
// directory store has no NUL prefix, so it reaches isSystemFile through a
// different branch than the file store does.
func TestPrivateStoresAreSystemFiles(t *testing.T) {
	vol := openDirLinkVolume(t, hfsHasLinkChainMask)

	ents, err := vol.ReadDirCNID(rootFolderCNID)
	if err != nil {
		t.Fatalf("ReadDirCNID(root): %v", err)
	}

	var sawFileStore, sawDirStore bool
	for _, e := range ents {
		switch e.CNID {
		case dlFilePrivCNID:
			sawFileStore = true
			if !e.IsSystem {
				t.Errorf("file store %q reported as user content", e.Name)
			}
		case dlDirPrivCNID:
			sawDirStore = true
			if !e.IsSystem {
				t.Errorf("directory store %q reported as user content", e.Name)
			}
		default:
			if e.IsSystem {
				t.Errorf("%q (CNID %d) reported as a system file", e.Name, e.CNID)
			}
		}
	}
	if !sawFileStore || !sawDirStore {
		t.Fatalf("root listing missed a store: file %v, dir %v — nothing was checked",
			sawFileStore, sawDirStore)
	}
}

// TestFileLinkStoreAbsentIsNotTheDirectoryStore covers a volume that has
// carried directory links but never a file link, so only one private store
// exists.
//
// Both names contain "HFS+ Private", so a substring test that does not exclude
// the directory store returns it for a file-link lookup. Nothing about that
// fails loudly: the search simply looks for "iNode<n>" in a directory that
// holds "dir_<n>" entries, finds nothing, and silently falls back to reading
// the reference as a CNID.
func TestFileLinkStoreAbsentIsNotTheDirectoryStore(t *testing.T) {
	vol := openDirLinkVolumeOpts(t, dlOpts{
		stubFlags:     hfsHasLinkChainMask,
		omitFileStore: true,
	})

	// The premise: the directory store is present, so there is something for
	// the file-store search to wrongly latch onto.
	dirStore, ok := vol.privateDirCNID(storeDirLinks)
	if !ok || dirStore != dlDirPrivCNID {
		t.Fatalf("directory store = %d (found %v), want %d — the fixture is not "+
			"testing what it claims", dirStore, ok, dlDirPrivCNID)
	}

	if cnid, ok := vol.privateDirCNID(storeFileLinks); ok {
		t.Errorf("file store reported at CNID %d on a volume that has none; "+
			"the directory store is at %d", cnid, dirStore)
	}

	// The directory link must still resolve — removing one store does not
	// disturb the other.
	rec, err := vol.OpenCNID(dlStubCNID)
	if err != nil {
		t.Fatalf("OpenCNID(dir link): %v", err)
	}
	if rec.CNID != dlDirInodeCNID {
		t.Errorf("resolved CNID = %d, want %d", rec.CNID, dlDirInodeCNID)
	}
}

// TestHardLinkInodeClaimingToBeALink is the volume no version of macOS writes:
// the inode a link points at is itself marked as a hard link, pointing at a
// reference that names nothing.
//
// Two things are pinned. Resolution stops at the inode rather than taking a
// second hop — that is what makes the old visited-set loop unnecessary rather
// than merely unexercised, since there is no chain to guard against whatever
// the record claims. And the malformed claim surfaces as ErrNotFound rather
// than as some other file's bytes: reading a fork re-enters the catalog by
// CNID, so the inode is asked to resolve on its own account and fails there.
// A regression in the first would show up as a hang; in the second, as silent
// wrong content.
func TestHardLinkInodeClaimingToBeALink(t *testing.T) {
	vol := openDirLinkVolumeOpts(t, dlOpts{
		stubFlags:       hfsHasLinkChainMask,
		fileInodeIsLink: true,
	})

	// The premise: the inode really does look like a link, and what it points
	// at really is absent.
	ino, err := vol.OpenCNIDRaw(dlFileInoCNID)
	if err != nil {
		t.Fatalf("OpenCNIDRaw(inode): %v", err)
	}
	if ino.Link != LinkHardFile {
		t.Fatalf("inode Link = %v, want %v — the adversarial case is not set up",
			ino.Link, LinkHardFile)
	}
	if ino.DataFork.LogicalSize == 0 {
		t.Fatal("inode has no data fork; a wrong answer could not be distinguished")
	}
	if _, ok := vol.findInodeByName(dlOrphanRef, LinkHardFile); ok {
		t.Fatalf("iNode%d exists; the second hop would succeed and this test "+
			"would prove nothing", dlOrphanRef)
	}

	// One hop, and it lands on the inode.
	rec, err := vol.OpenCNID(dlFileStubCNID)
	if err != nil {
		t.Fatalf("OpenCNID(file link): %v", err)
	}
	if rec.CNID != dlFileInoCNID {
		t.Fatalf("resolved CNID = %d, want the inode %d — resolution took a "+
			"second hop", rec.CNID, dlFileInoCNID)
	}
	if rec.LinkTarget != dlFileInoCNID {
		t.Errorf("LinkTarget = %d, want %d: the resolved record must point at "+
			"itself, which is what ends the resolution", rec.LinkTarget, dlFileInoCNID)
	}

	// And the broken claim is reported rather than worked around.
	if _, err := vol.OpenCNID(dlFileInoCNID); !errors.Is(err, ErrNotFound) {
		t.Errorf("OpenCNID(inode) error = %v, want ErrNotFound — the inode "+
			"points at iNode%d, which does not exist", err, dlOrphanRef)
	}
	if _, err := vol.OpenFileByCNID(dlFileStubCNID); !errors.Is(err, ErrNotFound) {
		t.Errorf("OpenFileByCNID through the link error = %v, want ErrNotFound", err)
	}
}

// TestFileHardLinkUsesFileStore is the mirror of
// TestDirectoryHardLinkUsesDirectoryStore: on the same volume, the same
// reference number resolved as a file link must reach iNode555, not dir_555.
func TestFileHardLinkUsesFileStore(t *testing.T) {
	vol := openDirLinkVolume(t, hfsHasLinkChainMask)

	rec, err := vol.OpenCNID(dlFileStubCNID)
	if err != nil {
		t.Fatalf("OpenCNID(file link): %v", err)
	}
	if rec.CNID == dlDirInodeCNID {
		t.Fatalf("the file link resolved to dir_%d — it was looked up in the "+
			"directory store", dlLinkRef)
	}
	if rec.CNID != dlFileInoCNID {
		t.Fatalf("resolved CNID = %d, want %d", rec.CNID, dlFileInoCNID)
	}
	if rec.Type != CatalogRecordFile {
		t.Errorf("resolved Type = %v, want a file", rec.Type)
	}

	f, err := vol.OpenFileByCNID(dlFileStubCNID)
	if err != nil {
		t.Fatalf("OpenFileByCNID: %v", err)
	}
	got, err := f.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if want := dlFileInodePayload(); !bytes.Equal(got, want) {
		t.Errorf("read %d bytes, want %d", len(got), len(want))
	}
}
