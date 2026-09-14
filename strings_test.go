package libhfs

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// The rendered forms of the package's enums, and the two internal helpers that
// nothing in the suite reached.
//
// These are not coverage for its own sake. Every String method here ends up in
// a forensic report, where the difference between "free node" and "unallocated"
// is the difference between two provenance claims an examiner would defend
// differently; and unwrapSearchErr decides whether a caller's own error is
// returned or is mistaken for a degraded search, which is the difference
// between an honest failure and a silent full-catalog rescan.

func TestExtentsKeyString(t *testing.T) {
	// The fork discriminator is the part that can be wrong: the resource fork
	// is 0xFF on disk, not 1, so a "!= 0" or "== 1" test would mislabel every
	// key. Anything that is not the resource marker reads as the data fork,
	// which is what the on-disk format means by it.
	cases := []struct {
		name string
		key  ExtentsKey
		want string
	}{
		{"data fork", ExtentsKey{FileID: 42, ForkType: extentKeyTypeData, StartBlock: 7},
			"cnid=42 fork=data start=7"},
		{"resource fork", ExtentsKey{FileID: 42, ForkType: extentKeyTypeRsrc, StartBlock: 0},
			"cnid=42 fork=resource start=0"},
		{"unknown fork type reads as data", ExtentsKey{FileID: 1, ForkType: 0x01, StartBlock: 3},
			"cnid=1 fork=data start=3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.key.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRecoverySourceString(t *testing.T) {
	cases := []struct {
		src  RecoverySource
		want string
	}{
		{RecoveredFromNodeSlack, "node slack"},
		{RecoveredFromFreeNode, "free node"},
		{RecoveredFromUnallocated, "unallocated"},
		// An unrecognised value is a damaged or future record, not a new kind
		// of provenance, so it must not render as a number that reads like one.
		{RecoverySource(200), "unknown"},
	}
	seen := map[string]RecoverySource{}
	for _, tc := range cases {
		got := tc.src.String()
		if got != tc.want {
			t.Errorf("RecoverySource(%d).String() = %q, want %q", tc.src, got, tc.want)
		}
		if prev, dup := seen[got]; dup && prev != tc.src {
			t.Errorf("RecoverySource(%d) and RecoverySource(%d) both render as %q; "+
				"two provenances a report cannot tell apart", prev, tc.src, got)
		}
		seen[got] = tc.src
	}
}

func TestTextEncodingString(t *testing.T) {
	cases := []struct {
		enc  TextEncoding
		want string
	}{
		{TextEncodingMacRoman, "MacRoman"},
		{TextEncodingRaw, "raw"},
		{TextEncoding(9), "unknown"},
	}
	for _, tc := range cases {
		if got := tc.enc.String(); got != tc.want {
			t.Errorf("TextEncoding(%d).String() = %q, want %q", tc.enc, got, tc.want)
		}
	}

	// The zero value is the documented default, so the string for an encoding
	// nobody set must be the one the decoder actually uses.
	var unset TextEncoding
	if unset.String() != "MacRoman" {
		t.Errorf("zero TextEncoding renders as %q, want MacRoman", unset.String())
	}
}

func TestXAttrStorageString(t *testing.T) {
	if got := XAttrInline.String(); got != "inline" {
		t.Errorf("XAttrInline.String() = %q, want inline", got)
	}
	if got := XAttrFork.String(); got != "fork" {
		t.Errorf("XAttrFork.String() = %q, want fork", got)
	}
	// Only XAttrFork means fork storage; anything else is inline, because an
	// attribute record that is not a fork-data record holds its value in the
	// B-tree node.
	if got := XAttrStorage(7).String(); got != "inline" {
		t.Errorf("XAttrStorage(7).String() = %q, want inline", got)
	}
}

// TestCallbackErrorIsNotMistakenForDegradation pins the contract the keyed
// B-tree search depends on.
//
// A callback error must reach the caller unchanged. Anything else means the
// descent failed and the linear fallback should run. Confusing the two is
// silent in both directions: a caller's error treated as degradation provokes
// a full-catalog rescan and is then discarded, and a genuine descent failure
// treated as a callback error skips the fallback and reports a short listing
// as complete.
func TestCallbackErrorIsNotMistakenForDegradation(t *testing.T) {
	sentinel := errors.New("the caller stopped the walk")
	ce := &callbackError{err: sentinel}

	if got := ce.Error(); got != sentinel.Error() {
		t.Errorf("Error() = %q, want the wrapped message %q", got, sentinel.Error())
	}
	// errors.Is has to reach the wrapped error, which is what Unwrap is for.
	if !errors.Is(ce, sentinel) {
		t.Error("errors.Is could not see through the callbackError")
	}

	t.Run("callback error is returned", func(t *testing.T) {
		got, degraded := unwrapSearchErr(ce)
		if degraded {
			t.Error("a callback error was reported as a degraded search")
		}
		if got != sentinel {
			t.Errorf("unwrapSearchErr returned %v, want the caller's own error", got)
		}
	})

	t.Run("wrapped callback error is still found", func(t *testing.T) {
		// errors.As is what finds it, so a callbackError buried under a
		// fmt.Errorf must not read as degradation.
		wrapped := fmt.Errorf("descent failed: %w", &callbackError{err: sentinel})
		got, degraded := unwrapSearchErr(wrapped)
		if degraded {
			t.Error("a wrapped callback error was reported as a degraded search")
		}
		if got != sentinel {
			t.Errorf("unwrapSearchErr returned %v, want the caller's own error", got)
		}
	})

	t.Run("search failure is degradation", func(t *testing.T) {
		if _, degraded := unwrapSearchErr(errSearchDegraded); !degraded {
			t.Error("errSearchDegraded was not reported as a degraded search")
		}
		if _, degraded := unwrapSearchErr(ErrCorrupt); !degraded {
			t.Error("an ordinary error was not reported as a degraded search")
		}
	})

	t.Run("no error is neither", func(t *testing.T) {
		got, degraded := unwrapSearchErr(nil)
		if got != nil || degraded {
			t.Errorf("unwrapSearchErr(nil) = (%v, %v), want (nil, false)", got, degraded)
		}
	})
}

// TestOpenDeletedClampsSizeToExtents covers the fallback a recovered record
// needs: its stored size is as likely to be damaged as the rest of it, so the
// reader is bounded by what the extents can physically hold.
//
// Without the clamp a record claiming a gigabyte would hand back a reader that
// walks off the end of its own extents.
func TestOpenDeletedClampsSizeToExtents(t *testing.T) {
	const blockSize = 4096
	vol := &Volume{header: VolumeHeader{BlockSize: blockSize}}

	rec := func(size uint64, counts ...uint32) DeletedRecord {
		var d DeletedRecord
		d.Record.CNID = 101
		d.Record.DataFork.LogicalSize = size
		for i, c := range counts {
			d.Record.DataFork.Extents[i] = ExtentDescriptor{
				StartBlock: uint32(10 + i),
				BlockCount: c,
			}
		}
		return d
	}

	cases := []struct {
		name string
		rec  DeletedRecord
		want int64
	}{
		// The honest case: a size the extents can hold is kept as it is, so
		// the clamp cannot be hiding a bug by rounding everything up.
		{"plausible size kept", rec(5000, 2), 5000},
		{"exact fit kept", rec(2*blockSize, 2), 2 * blockSize},
		// A size larger than the extents hold, a zero size and a size whose
		// signed conversion is negative are all corruption, and all fall back
		// to the capacity.
		{"oversized size clamped", rec(1<<30, 2), 2 * blockSize},
		{"zero size replaced", rec(0, 3), 3 * blockSize},
		{"negative when signed", rec(1<<63, 1), blockSize},
		// Capacity sums every extent, not just the first.
		{"capacity spans all extents", rec(1<<30, 1, 2, 4), 7 * blockSize},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := vol.OpenDeleted(tc.rec)
			if err != nil {
				t.Fatalf("OpenDeleted: %v", err)
			}
			if got := f.Size(); got != tc.want {
				t.Errorf("Size() = %d, want %d", got, tc.want)
			}
		})
	}

	t.Run("no extents is an error", func(t *testing.T) {
		_, err := vol.OpenDeleted(rec(1234))
		if !errors.Is(err, ErrMissingExtent) {
			t.Errorf("OpenDeleted with no extents = %v, want ErrMissingExtent", err)
		}
	})
}

// TestReportCarriesVolumeName pins the field the report gained alongside
// Volume.VolumeName, including that a volume without a readable name still
// produces a report rather than failing over a cosmetic field.
func TestReportCarriesVolumeName(t *testing.T) {
	vol := openLinkVolume(t)

	rep, err := vol.Report(nil)
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	want, err := vol.VolumeName()
	if err != nil {
		t.Fatalf("VolumeName: %v", err)
	}
	if want == "" {
		t.Fatal("the fixture volume has no name; this test would prove nothing")
	}
	if rep.Volume.Name != want {
		t.Errorf("Report volume name = %q, want %q", rep.Volume.Name, want)
	}
	if strings.TrimSpace(rep.Volume.Name) == "" {
		t.Error("report carried a blank volume name")
	}
}
