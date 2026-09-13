package hfs

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// The blkid oracle: the volume UUID and label, from an implementation that
// shares no code with this one.
//
// oracle_test.go checks the catalog against hfsutils, which understands only
// classic HFS. The UUID had no check at all. It is derived rather than stored,
// so a corpus test can only confirm that libhfs agrees with itself, and the
// reference implementation — diskutil — needs a Mac. util-linux's libblkid
// derives it in C from the same published Apple source, and is available in
// WSL, which makes it the nearest independent authority available here.
//
// Generate the sidecars with testdata/gen_blkid_oracle.sh.

type blkidOracle struct {
	path      string
	fsType    string
	uuid      string // "" when blkid reported none
	label     string // "" when blkid reported none
	haveUUID  bool
	haveLabel bool
}

// loadBlkidOracle reads the sidecar written beside the corpus image.
func loadBlkidOracle(tb testing.TB) *blkidOracle {
	tb.Helper()

	img := os.Getenv(corpusEnvVar)
	if img == "" {
		tb.Skipf("set %s to a raw HFS image to run corpus tests", corpusEnvVar)
	}
	path := img + ".blkid.tsv"
	data, err := os.ReadFile(path)
	if err != nil {
		tb.Skipf("no blkid oracle beside %s; generate one with testdata/gen_blkid_oracle.sh", img)
	}

	fields := strings.Split(strings.TrimRight(string(data), "\r\n"), "\t")
	if len(fields) < 4 || fields[0] != "U" {
		tb.Fatalf("%s: malformed blkid oracle %q", path, string(data))
	}
	o := &blkidOracle{path: path, fsType: fields[1]}
	if fields[2] != "-" {
		o.uuid, o.haveUUID = fields[2], true
	}
	if fields[3] != "-" {
		o.label, o.haveLabel = fields[3], true
	}
	return o
}

// TestCorpusUUIDMatchesBlkid is the only check the UUID derivation has against
// another implementation.
//
// The comparison is deliberately case-insensitive: blkid prints lower case and
// this package prints upper, matching diskutil. That is a formatting choice on
// both sides, and pinning it here would make the test fail for the wrong
// reason if either tool changed its mind.
func TestCorpusUUIDMatchesBlkid(t *testing.T) {
	vol, cleanup := corpusVolume(t)
	defer cleanup()

	o := loadBlkidOracle(t)
	got, err := vol.UUID()

	if !o.haveUUID {
		// These classic HFS images leave the identifier words zero and blkid
		// reports no UUID for them. libhfs must agree that there is nothing
		// there rather than derive one from zeros.
		if err == nil {
			t.Errorf("UUID() = %q, but blkid found none in %s", got, o.path)
		} else if !errors.Is(err, ErrNotFound) {
			t.Errorf("UUID() error = %v, want ErrNotFound", err)
		}
		return
	}

	if err != nil {
		t.Fatalf("UUID() = %v, but blkid read %s from this image", err, o.uuid)
	}
	if !strings.EqualFold(got, o.uuid) {
		t.Errorf("UUID mismatch: libhfs %s, blkid %s (%s)", got, o.uuid, o.path)
	}

	// The derivation must also be reachable from the raw identifier, or the
	// two accessors have drifted apart.
	id, err := vol.VolumeIdentifier()
	if err != nil {
		t.Fatalf("VolumeIdentifier: %v", err)
	}
	if id.IsZero() {
		t.Fatal("VolumeIdentifier is zero but a UUID was derived from it")
	}
	if !strings.EqualFold(id.UUID(), o.uuid) {
		t.Errorf("VolumeIdentifier.UUID() = %s, want %s", id.UUID(), o.uuid)
	}
}

// TestCorpusVolumeNameMatchesBlkid cross-checks VolumeName, which until now
// was only ever compared against this package's own reading of the same bytes.
//
// It covers both paths at once across the corpus: blkid reads drVN from the
// MDB for classic HFS and the root catalog record for HFS+, which are exactly
// the two branches VolumeName takes.
func TestCorpusVolumeNameMatchesBlkid(t *testing.T) {
	vol, cleanup := corpusVolume(t)
	defer cleanup()

	o := loadBlkidOracle(t)

	// The kind is checked first, because a mismatch below could otherwise be
	// explained away by the two tools having parsed different volumes — which
	// is a real hazard on a wrapped image, where one may see the HFS wrapper
	// and the other the embedded HFS+ volume.
	switch o.fsType {
	case "hfs":
		if vol.Kind() != KindHFS {
			t.Fatalf("blkid says hfs, libhfs says %s — not the same volume", vol.Kind())
		}
	case "hfsplus":
		if vol.Kind() != KindHFSP && vol.Kind() != KindHFSX {
			t.Fatalf("blkid says hfsplus, libhfs says %s — not the same volume", vol.Kind())
		}
	default:
		t.Fatalf("%s: unexpected filesystem type %q", o.path, o.fsType)
	}

	if !o.haveLabel {
		// blkid declines to read a label it cannot reach — it does exactly
		// that on the 64 KiB-block image — and that is not evidence the volume
		// has none.
		t.Skipf("blkid read no label from %s; nothing to compare", o.path)
	}

	got, err := vol.VolumeName()
	if err != nil {
		t.Fatalf("VolumeName() = %v, but blkid read %q from this image", err, o.label)
	}
	if got != o.label {
		t.Errorf("volume name mismatch: libhfs %q, blkid %q (%s)", got, o.label, o.path)
	}
}
