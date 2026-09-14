package libhfs

import "testing"

// The constant extraction replaced ~120 literal offsets. A wrong value would
// still compile and, in many cases, still pass higher-level tests by reading a
// neighbouring field. These pin the values against the specification.
func TestOffsetConstantsMatchSpec(t *testing.T) {
	cases := []struct {
		name string
		got  int
		want int
	}{
		// HFSPlusForkData
		{"forkDataLogicalSize", forkDataLogicalSize, 0},
		{"forkDataClumpSize", forkDataClumpSize, 8},
		{"forkDataTotalBlocks", forkDataTotalBlocks, 12},
		{"forkDataExtents", forkDataExtents, 16},
		{"forkDataSize", forkDataSize, 80},
		{"extentRecordSize", extentRecordSize, 64},
		{"hfsExtentRecordSize", hfsExtentRecordSize, 12},
		// HFSPlusVolumeHeader
		{"volHdrBlockSize", volHdrBlockSize, 40},
		{"volHdrTotalBlocks", volHdrTotalBlocks, 44},
		{"volHdrNextCatalogID", volHdrNextCatalogID, 64},
		{"volHdrFinderInfo", volHdrFinderInfo, 80},
		{"volHdrAllocationFile", volHdrAllocationFile, 112},
		{"volHdrExtentsFile", volHdrExtentsFile, 192},
		{"volHdrCatalogFile", volHdrCatalogFile, 272},
		{"volHdrAttributesFile", volHdrAttributesFile, 352},
		{"volHdrStartupFile", volHdrStartupFile, 432},
		// Catalog records: dates share offsets between folder and file.
		{"catValence", catValence, 4},
		{"catNodeID", catNodeID, 8},
		{"catCreateDate", catCreateDate, 12},
		{"catContentModDte", catContentModDte, 16},
		{"catAttrModDate", catAttrModDate, 20},
		{"catAccessDate", catAccessDate, 24},
		{"catBackupDate", catBackupDate, 28},
		{"catPermissions", catPermissions, 32},
		{"catUserInfo", catUserInfo, 48},
		{"catFileType", catFileType, 48},
		{"catFileCreator", catFileCreator, 52},
		{"catFinderInfo", catFinderInfo, 64},
		{"catFolderRecordSize", catFolderRecordSize, 88},
		{"catDataFork", catDataFork, 88},
		{"catRsrcFork", catRsrcFork, 168},
		{"catFileRecordSize", catFileRecordSize, 248},
		// BSDInfo, relative to catPermissions.
		{"bsdInfoSize", bsdInfoSize, 16},
		{"catPermissions+bsdSpecial", catPermissions + bsdSpecial, 46},
		// Classic HFS
		{"hfsDirValence", hfsDirValence, 4},
		{"hfsDirDirID", hfsDirDirID, 6},
		{"hfsDirCreateDate", hfsDirCreateDate, 10},
		{"hfsFilFdType", hfsFilFdType, 4},
		{"hfsFilFdCreator", hfsFilFdCreator, 8},
		{"hfsFilFileNumber", hfsFilFileNumber, 20},
		{"hfsFilLogicalSize", hfsFilLogicalSize, 26},
		{"hfsFilPhysSize", hfsFilPhysSize, 30},
		{"hfsFilRLogicalLen", hfsFilRLogicalLen, 36},
		{"hfsFilRPhysLen", hfsFilRPhysLen, 40},
		{"hfsFilCreateDate", hfsFilCreateDate, 44},
		{"hfsFilExtentRec", hfsFilExtentRec, 74},
		{"hfsFilRExtentRec", hfsFilRExtentRec, 86},
		{"hfsFilMinSize", hfsFilMinSize, 98},
		// B-tree
		{"nodeNumRecords", nodeNumRecords, 10},
		{"btHdrNodeSize", btHdrNodeSize, 18},
		{"btHdrTotalNodes", btHdrTotalNodes, 22},
		{"btHdrKeyCompareTyp", btHdrKeyCompareTyp, 37},
		// Keys
		{"catKeyNameLength", catKeyNameLength, 4},
		{"catKeyName", catKeyName, 6},
		{"extKeyFileID", extKeyFileID, 4},
		{"extKeyStartBlock", extKeyStartBlock, 8},
		{"attrKeyFileID", attrKeyFileID, 2},
		{"attrKeyName", attrKeyName, 12},
	}

	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
}

// The BSDInfo parser reads at catPermissions+field; the field offsets are
// relative to the structure, not the record.
func TestBSDInfoOffsetsAreStructureRelative(t *testing.T) {
	if bsdOwnerID != 0 {
		t.Errorf("bsdOwnerID = %d, want 0 (relative to catPermissions)", bsdOwnerID)
	}
	if catPermissions+bsdFileMode != 42 {
		t.Errorf("file mode lands at %d, want 42", catPermissions+bsdFileMode)
	}
}
