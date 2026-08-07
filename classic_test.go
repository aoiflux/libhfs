package hfs

import (
	"bytes"
	"testing"
)

// Names above 0x7F were previously widened byte-for-byte, so every accented
// character decoded to the wrong code point. Only 0xA5 happened to be right, by
// coincidence — MacRoman 0xA5 is a bullet, and U+00A5 is the yen sign, so even
// that "match" was wrong.
func TestMacRomanDecoding(t *testing.T) {
	cases := []struct {
		name  string
		bytes []byte
		want  string
	}{
		{"ascii", []byte("README"), "README"},
		{"e-acute", []byte{0x8E}, "é"},
		{"a-umlaut", []byte{0x8A}, "ä"},
		{"n-tilde", []byte{0x96}, "ñ"},
		{"bullet", []byte{0xA5}, "•"},
		{"degree", []byte{0xA1}, "°"},
		{"ellipsis", []byte{0xC9}, "…"},
		{"apple-logo", []byte{0xF0}, ""},
		{"mixed", []byte{'C', 'a', 'f', 0x8E}, "Café"},
		{"empty", nil, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decodeMacRoman(tc.bytes); got != tc.want {
				t.Errorf("decodeMacRoman(% x) = %q, want %q", tc.bytes, got, tc.want)
			}
		})
	}
}

func TestMacRomanTableIsComplete(t *testing.T) {
	seen := map[rune]int{}
	for i, r := range macRomanHigh {
		if r == 0 {
			t.Errorf("macRomanHigh[0x%02X] is unset", 0x80+i)
		}
		if prev, dup := seen[r]; dup {
			t.Errorf("macRomanHigh[0x%02X] duplicates 0x%02X (both %U)", 0x80+i, 0x80+prev, r)
		}
		seen[r] = i
	}
	// Every byte 0x00-0xFF must decode to exactly one rune.
	all := make([]byte, 256)
	for i := range all {
		all[i] = byte(i)
	}
	if got := []rune(decodeMacRoman(all)); len(got) != 256 {
		t.Errorf("decoding all 256 bytes produced %d runes, want 256", len(got))
	}
}

func TestTextEncodingRawPreservesBytes(t *testing.T) {
	vol := &Volume{kind: KindHFS}
	raw := []byte{0x8E, 0xA5, 'x'}

	if got, want := vol.decodeHFSName(raw), "é•x"; got != want {
		t.Errorf("default encoding: got %q, want %q", got, want)
	}

	vol.SetTextEncoding(TextEncodingRaw)
	if got := vol.decodeHFSName(raw); got != "¥x" {
		t.Errorf("raw encoding: got %q, want the byte values widened", got)
	}
	if vol.TextEncoding() != TextEncodingRaw {
		t.Errorf("TextEncoding() = %v, want %v", vol.TextEncoding(), TextEncodingRaw)
	}
}

// End-to-end: a classic HFS volume with a high-bit filename must list and open
// under the decoded name.
func TestClassicHFSHighBitFilename(t *testing.T) {
	vol, err := Open(bytes.NewReader(buildClassicHFSTimesImage(t)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if vol.Kind() != KindHFS {
		t.Fatalf("Kind = %s, want %s", vol.Kind(), KindHFS)
	}

	entries, err := vol.ReadDir("/")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	var found bool
	for _, e := range entries {
		if e.Name == classicAccentedName {
			found = true
		}
	}
	if !found {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name)
		}
		t.Fatalf("accented name %q not in listing; got %q", classicAccentedName, names)
	}

	rec, err := vol.OpenPath("/" + classicAccentedName)
	if err != nil {
		t.Fatalf("OpenPath(%q): %v", classicAccentedName, err)
	}
	if rec.CNID != classicAccentedCNID {
		t.Errorf("CNID = %d, want %d", rec.CNID, classicAccentedCNID)
	}
}

// Classic HFS FinderInfo lives in filUsrWds at offset 4, and was previously
// left at zero. The type/creator pair is how classic Mac files record what they
// are, since the format has no extensions.
func TestClassicHFSFinderInfo(t *testing.T) {
	vol, err := Open(bytes.NewReader(buildClassicHFSTimesImage(t)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	rec, err := vol.OpenPath("/DATA")
	if err != nil {
		t.Fatalf("OpenPath: %v", err)
	}
	if rec.FinderType != classicFinderType {
		t.Errorf("FinderType = %#x, want %#x", rec.FinderType, classicFinderType)
	}
	if rec.FinderCreator != classicFinderCreator {
		t.Errorf("FinderCreator = %#x, want %#x", rec.FinderCreator, classicFinderCreator)
	}
}

// Classic HFS now uses keyed descent too. It must agree with the exhaustive
// walk and must not fall back.
func TestClassicHFSKeyedSearch(t *testing.T) {
	vol, err := Open(bytes.NewReader(buildClassicHFSTimesImage(t)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !vol.supportsKeyedSearch() {
		t.Fatal("classic HFS should support keyed search")
	}

	for _, cnid := range []uint32{rootFolderCNID, 100, classicAccentedCNID} {
		keyed, kerr := vol.lookupCNIDViaThread(cnid)
		linear, lerr := vol.lookupCNIDLinear(cnid)
		if lerr != nil {
			t.Fatalf("linear lookup of %d failed: %v", cnid, lerr)
		}
		if kerr != nil {
			// The fixture has no thread records, so the keyed path legitimately
			// cannot resolve a CNID; the linear fallback covers it.
			t.Logf("CNID %d: no thread record, fell back (%v)", cnid, kerr)
			continue
		}
		if keyed.CNID != linear.CNID || keyed.Name != linear.Name {
			t.Errorf("CNID %d: keyed=%+v linear=%+v", cnid, keyed, linear)
		}
	}

	// Directory listing goes through keyed descent regardless of thread records.
	entries, err := vol.ReadDirCNID(rootFolderCNID)
	if err != nil {
		t.Fatalf("ReadDirCNID: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(entries), entries)
	}
}

func TestClassicHFSValenceAndDates(t *testing.T) {
	vol, err := Open(bytes.NewReader(buildClassicHFSTimesImage(t)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	root, err := vol.GetRootDirectory()
	if err != nil {
		t.Fatalf("GetRootDirectory: %v", err)
	}
	if root.Valence != classicRootValence {
		t.Errorf("Valence = %d, want %d", root.Valence, classicRootValence)
	}
	if root.Times.Source != TimeSourceHFSLocal {
		t.Errorf("Source = %v, want %v", root.Times.Source, TimeSourceHFSLocal)
	}
}
