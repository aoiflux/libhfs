package hfs

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func TestFileIdentityEqual(t *testing.T) {
	created := time.Date(2020, 3, 4, 5, 6, 7, 0, time.UTC)

	base := FileIdentity{CNID: 42, Created: created, Source: TimeSourceHFSPlusGMT}

	tests := []struct {
		name  string
		other FileIdentity
		want  bool
	}{
		{
			name:  "identical",
			other: FileIdentity{CNID: 42, Created: created, Source: TimeSourceHFSPlusGMT},
			want:  true,
		},
		{
			name:  "same instant in a different location",
			other: FileIdentity{CNID: 42, Created: created.In(time.FixedZone("X", 3600)), Source: TimeSourceHFSPlusGMT},
			want:  true,
		},
		{
			name:  "different CNID",
			other: FileIdentity{CNID: 43, Created: created, Source: TimeSourceHFSPlusGMT},
			want:  false,
		},
		{
			name:  "different creation date",
			other: FileIdentity{CNID: 42, Created: created.Add(time.Second), Source: TimeSourceHFSPlusGMT},
			want:  false,
		},
		{
			// A classic HFS reading is wall-clock with no recorded offset, so
			// it is not the same value even when the numbers coincide.
			name:  "same numbers from a different kind of clock",
			other: FileIdentity{CNID: 42, Created: created, Source: TimeSourceHFSLocal},
			want:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := base.Equal(tc.other); got != tc.want {
				t.Fatalf("Equal = %v, want %v", got, tc.want)
			}
			if got := tc.other.Equal(base); got != tc.want {
				t.Fatalf("Equal is not symmetric: reverse gave %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFileIdentityComparable(t *testing.T) {
	created := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		id   FileIdentity
		want bool
	}{
		{"cnid and date", FileIdentity{CNID: 7, Created: created}, true},
		{"no date", FileIdentity{CNID: 7}, false},
		{"no cnid", FileIdentity{Created: created}, false},
		{"neither", FileIdentity{}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.id.Comparable(); got != tc.want {
				t.Fatalf("Comparable = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFileIdentityString(t *testing.T) {
	created := time.Date(2020, 3, 4, 5, 6, 7, 0, time.UTC)

	id := FileIdentity{CNID: 42, Created: created}
	if got, want := id.String(), "42@2020-03-04T05:06:07Z"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}

	// The same instant recorded in another zone must render identically, so
	// the string is usable as a key for correlating two readings.
	shifted := FileIdentity{CNID: 42, Created: created.In(time.FixedZone("X", 7200))}
	if got := shifted.String(); got != id.String() {
		t.Fatalf("String() depends on the location: %q vs %q", got, id.String())
	}

	absent := FileIdentity{CNID: 42}
	if got, want := absent.String(), "42@unknown"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

func TestIdentityFromRecordAndVolume(t *testing.T) {
	vol := openTimesFixture(t)

	rec, err := vol.OpenPath("/file.txt")
	if err != nil {
		t.Fatalf("OpenPath failed: %v", err)
	}

	fromRecord := rec.Identity()
	if fromRecord.CNID != rec.CNID {
		t.Fatalf("Identity CNID = %d, want %d", fromRecord.CNID, rec.CNID)
	}
	if !fromRecord.Created.Equal(rec.Times.Created) {
		t.Fatalf("Identity Created = %v, want %v", fromRecord.Created, rec.Times.Created)
	}
	if fromRecord.Source != rec.Times.Source {
		t.Fatalf("Identity Source = %v, want %v", fromRecord.Source, rec.Times.Source)
	}

	byCNID, err := vol.IdentityByCNID(rec.CNID)
	if err != nil {
		t.Fatalf("IdentityByCNID failed: %v", err)
	}
	byPath, err := vol.IdentityByPath("/file.txt")
	if err != nil {
		t.Fatalf("IdentityByPath failed: %v", err)
	}
	if !byCNID.Equal(fromRecord) || !byPath.Equal(fromRecord) {
		t.Fatalf("identities disagree: record %v, cnid %v, path %v", fromRecord, byCNID, byPath)
	}
}

func TestIdentityByCNIDNotFound(t *testing.T) {
	vol, _ := openValidTree(t)
	if _, err := vol.IdentityByCNID(999999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// openTimesFixture opens the timestamp fixture, which is the smallest image
// carrying a file with a populated creation date.
func openTimesFixture(t *testing.T) *Volume {
	t.Helper()
	vol, err := Open(bytes.NewReader(buildTimesTestImage(t)))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	return vol
}
