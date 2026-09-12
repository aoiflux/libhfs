package hfs

import "testing"

func TestClassifyLink(t *testing.T) {
	cases := []struct {
		name         string
		ftype, fcrea uint32
		mode         uint16
		isDir        bool
		want         LinkKind
	}{
		{"plain file", 0x54455854, 0x21526368, sIFREG | 0o644, false, LinkNone},
		{"plain folder", 0, 0, sIFDIR | 0o755, true, LinkNone},
		{"hard file link", finderTypeHardLink, finderCreatorHFSPlus, sIFREG, false, LinkHardFile},
		{"dir hard link", finderTypeDirLink, finderCreatorMacS, sIFDIR, true, LinkHardDir},
		{"symlink by finder", finderTypeSymlink, finderCreatorSymlink, sIFLNK, false, LinkSymbolic},
		{"symlink by mode only", 0, 0, sIFLNK | 0o777, false, LinkSymbolic},
		// A folder whose mode happens to carry the symlink bits is not a
		// symlink; only the Finder pair can make a directory a link.
		{"folder with link mode", 0, 0, sIFLNK, true, LinkNone},
		// Half a Finder pair is not a link.
		{"type without creator", finderTypeHardLink, 0, sIFREG, false, LinkNone},
		{"creator without type", 0, finderCreatorHFSPlus, sIFREG, false, LinkNone},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyLink(tc.ftype, tc.fcrea, tc.mode, tc.isDir)
			if got != tc.want {
				t.Errorf("classifyLink(%#x, %#x, %#o, dir=%v) = %v, want %v",
					tc.ftype, tc.fcrea, tc.mode, tc.isDir, got, tc.want)
			}
		})
	}
}

func TestBSDInfoAccessors(t *testing.T) {
	cases := []struct {
		mode                 uint16
		link, regular, isDir bool
		perm                 uint16
	}{
		{sIFREG | 0o644, false, true, false, 0o644},
		{sIFDIR | 0o755, false, false, true, 0o755},
		{sIFLNK | 0o777, true, false, false, 0o777},
	}
	for _, tc := range cases {
		b := BSDInfo{FileMode: tc.mode}
		if b.IsSymlink() != tc.link || b.IsRegular() != tc.regular || b.IsDir() != tc.isDir {
			t.Errorf("mode %#o: symlink=%v regular=%v dir=%v; want %v/%v/%v",
				tc.mode, b.IsSymlink(), b.IsRegular(), b.IsDir(), tc.link, tc.regular, tc.isDir)
		}
		if b.Perm() != tc.perm {
			t.Errorf("mode %#o: Perm() = %#o, want %#o", tc.mode, b.Perm(), tc.perm)
		}
	}
}

func TestParseBSDInfoOffsets(t *testing.T) {
	payload := make([]byte, 88)
	// Distinct values so a decoder reading the wrong offset cannot pass.
	put32 := func(off int, v uint32) {
		payload[off] = byte(v >> 24)
		payload[off+1] = byte(v >> 16)
		payload[off+2] = byte(v >> 8)
		payload[off+3] = byte(v)
	}
	put32(32, 501)        // ownerID
	put32(36, 20)         // groupID
	payload[40] = 0x11    // adminFlags
	payload[41] = 0x22    // ownerFlags
	payload[42] = 0x81    // fileMode high
	payload[43] = 0xA4    // fileMode low -> 0x81A4 = S_IFREG|0644
	put32(44, 0xDEADBEEF) // special

	got := parseBSDInfo(payload)
	if got.OwnerID != 501 || got.GroupID != 20 {
		t.Errorf("owner/group = %d/%d, want 501/20", got.OwnerID, got.GroupID)
	}
	if got.AdminFlags != 0x11 || got.OwnerFlags != 0x22 {
		t.Errorf("flags = %#x/%#x, want 0x11/0x22", got.AdminFlags, got.OwnerFlags)
	}
	if got.FileMode != 0x81A4 {
		t.Errorf("FileMode = %#x, want 0x81A4", got.FileMode)
	}
	if !got.IsRegular() || got.Perm() != 0o644 {
		t.Errorf("mode %#x: regular=%v perm=%#o", got.FileMode, got.IsRegular(), got.Perm())
	}
	if got.Special != 0xDEADBEEF {
		t.Errorf("Special = %#x, want 0xDEADBEEF", got.Special)
	}
}

// ---------------------------------------------------------------------------
// Corpus
// ---------------------------------------------------------------------------

// POSIX metadata must actually come through on a real volume. All-zero modes
// would mean the BSDInfo offset is wrong.
func TestCorpusBSDInfoPopulated(t *testing.T) {
	vol, cleanup := corpusVolume(t)
	defer cleanup()

	// HFSCatalogFile and HFSCatalogFolder carry no permissions field, so a
	// classic HFS volume has no POSIX metadata to find and all-zero modes are
	// the correct answer rather than a symptom.
	if vol.Kind() == KindHFS {
		t.Skip("classic HFS records carry no BSD info")
	}

	var total, zeroMode, regular, dirs, symlinks, hardLinks int
	owners := map[uint32]int{}

	err := vol.WalkCatalog(func(r CatalogRecord) error {
		if r.Type != CatalogRecordFile && r.Type != CatalogRecordFolder {
			return nil
		}
		total++
		if r.Perms.FileMode == 0 {
			zeroMode++
		}
		owners[r.Perms.OwnerID]++
		switch {
		case r.Perms.IsRegular():
			regular++
		case r.Perms.IsDir():
			dirs++
		}
		if r.IsSymlink() {
			symlinks++
		}
		if r.IsHardLink() {
			hardLinks++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkCatalog: %v", err)
	}

	t.Logf("records=%d zeroMode=%d regular=%d dirs=%d symlinks=%d hardLinks=%d owners=%d",
		total, zeroMode, regular, dirs, symlinks, hardLinks, len(owners))

	if total == 0 {
		t.Fatal("no records")
	}
	// A volume that has never been mounted carries no POSIX metadata at all:
	// mkfs.hfsplus leaves the root folder's mode, owner and group zero, and
	// the mounting OS fills them in later. On such an image every assertion
	// below is the correct answer rather than a symptom.
	skipEmptyCorpus(t, vol, "POSIX metadata")
	if zeroMode == total {
		t.Error("every record has a zero file mode; the BSDInfo offset is likely wrong")
	}
	if regular == 0 && dirs == 0 {
		t.Error("no record has a recognisable file type in its mode")
	}
}

// The file type recorded in the POSIX mode must agree with the catalog record
// type. Disagreement would mean one of the two is being read wrongly.
func TestCorpusModeAgreesWithRecordType(t *testing.T) {
	vol, cleanup := corpusVolume(t)
	defer cleanup()

	var mismatches, checked int
	err := vol.WalkCatalog(func(r CatalogRecord) error {
		if r.Perms.FileMode == 0 {
			return nil // no POSIX metadata recorded for this node
		}
		switch r.Type {
		case CatalogRecordFolder:
			checked++
			if !r.Perms.IsDir() {
				mismatches++
				if mismatches <= 5 {
					t.Errorf("folder %q (cnid %d) has non-directory mode %#o",
						r.Name, r.CNID, r.Perms.FileMode)
				}
			}
		case CatalogRecordFile:
			checked++
			// Regular, symlink and device nodes are all legitimate here; only a
			// directory mode would be contradictory.
			if r.Perms.IsDir() {
				mismatches++
				if mismatches <= 5 {
					t.Errorf("file %q (cnid %d) has directory mode %#o",
						r.Name, r.CNID, r.Perms.FileMode)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkCatalog: %v", err)
	}
	t.Logf("checked %d records, %d mismatches", checked, mismatches)
	if checked == 0 {
		t.Skip("no records carry POSIX metadata")
	}
}

// ReadLink must refuse records that are not symlinks rather than returning the
// data fork's contents as if they were a path.
func TestReadLinkRejectsNonSymlink(t *testing.T) {
	vol, cleanup := corpusVolume(t)
	defer cleanup()

	var target uint32
	_ = vol.WalkCatalog(func(r CatalogRecord) error {
		if target == 0 && r.Type == CatalogRecordFile && !r.IsSymlink() {
			target = r.CNID
		}
		return nil
	})
	if target == 0 {
		t.Skip("no non-symlink file found")
	}
	if _, err := vol.ReadLink(target); err == nil {
		t.Error("ReadLink on a regular file returned nil error")
	}
}
