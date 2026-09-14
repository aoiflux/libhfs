package libhfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
)

// walkStopCase adapts one exported Walk method to a shape the table below can
// drive: run the walk with a callback that returns whatever body hands back.
//
// The adapters discard the values each callback receives on purpose. What is
// under test is the contract every Walk method shares — a callback returning
// [ErrStopWalk] ends the walk successfully, and any other error ends it and
// comes back — and that contract says nothing about what a record contains.
type walkStopCase struct {
	name string
	open func(t *testing.T) *Volume
	walk func(v *Volume, body func() error) error
}

func openStopVolume(t *testing.T, img []byte) *Volume {
	t.Helper()
	vol, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	return vol
}

// Every exported Walk method belongs here. A method missing from this table is
// a method whose stop behaviour nothing checks, which is how WalkXAttrs came to
// test the sentinel with == while everything else used errors.Is.
//
// WalkDeleted is the one exception, covered by TestCorpusWalkDeletedStops: no
// hermetic fixture reliably yields more than one recovered record, and a stop
// after the first record proves nothing on a walk that emits exactly one.
func walkStopCases() []walkStopCase {
	tree := func(t *testing.T) *Volume { v, _ := openValidTree(t); return v }
	xattrs := func(t *testing.T) *Volume { return openStopVolume(t, buildXAttrImage(t)) }
	bitmap := func(t *testing.T) *Volume { return openStopVolume(t, buildSplitFreeRunImage(t)) }

	return []walkStopCase{
		{"WalkCatalog", tree, func(v *Volume, body func() error) error {
			return v.WalkCatalog(func(CatalogRecord) error { return body() })
		}},
		{"WalkCatalogContext", tree, func(v *Volume, body func() error) error {
			return v.WalkCatalogContext(context.Background(), func(CatalogRecord) error { return body() })
		}},
		{"WalkDir", tree, func(v *Volume, body func() error) error {
			return v.WalkDir("/", func(DirEntry) error { return body() })
		}},
		{"WalkDirCNID", tree, func(v *Volume, body func() error) error {
			return v.WalkDirCNID(rootFolderCNID, func(DirEntry) error { return body() })
		}},
		{"WalkPaths", tree, func(v *Volume, body func() error) error {
			return v.WalkPaths(func(string, CatalogRecord) error { return body() })
		}},
		{"WalkPathsContext", tree, func(v *Volume, body func() error) error {
			return v.WalkPathsContext(context.Background(), func(string, CatalogRecord) error { return body() })
		}},
		{"WalkXAttrs", xattrs, func(v *Volume, body func() error) error {
			return v.WalkXAttrs(func(XAttr) error { return body() })
		}},
		{"WalkUnallocated", bitmap, func(v *Volume, body func() error) error {
			return v.WalkUnallocated(func(uint32, uint32) error { return body() })
		}},
	}
}

// buildSplitFreeRunImage is buildClassicHFSBitmapImage with one extra block
// marked in use partway through the free area, so the bitmap yields two runs
// rather than one.
//
// The shared fixture cannot be changed to do this: TestClassicHFSWalkUnallocatedRuns
// asserts its exact single run, and that assertion is the regression test for
// the bitmap's address. A walk that emits one record cannot demonstrate
// stopping after the first, so this fixture splits the run here instead.
func buildSplitFreeRunImage(t *testing.T) []byte {
	t.Helper()

	img := buildClassicHFSBitmapImage(t)
	bitmap := int(classicVBMStart) * hfsSectorSize

	// Blocks 0-8 are already in use and everything after them free. Byte 2 of
	// the bitmap covers blocks 16-23, MSB first, so this claims block 20 and
	// leaves free runs at 9-19 and 21 onwards.
	img[bitmap+2] = 0x08
	return img
}

// TestWalkStopSentinel is the contract [ErrStopWalk] exists to state.
//
// Before it was exported a caller could still leave a walk early, by returning
// an error of their own and filtering it back out at the call site. What they
// could not do was say "I am finished" without it arriving as a failure, and on
// a forensic tool that distinction is not cosmetic: a triage pass that stops at
// the first match and a triage pass that died partway through an image produce
// the same partial answer and must not be reported the same way.
func TestWalkStopSentinel(t *testing.T) {
	for _, tc := range walkStopCases() {
		t.Run(tc.name, func(t *testing.T) {
			// "stopped after one callback" says nothing about a walk that only
			// ever makes one, so establish the fixture makes several first.
			total := 0
			if err := tc.walk(tc.open(t), func() error { total++; return nil }); err != nil {
				t.Fatalf("%s over the whole fixture failed: %v", tc.name, err)
			}
			if total < 2 {
				t.Fatalf("the fixture yields %d callbacks; stopping after the first would prove nothing", total)
			}

			foreign := errors.New("caller gave up")
			cases := []struct {
				name string
				give error
				want error // nil means the walk must report success
			}{
				{"bare sentinel", ErrStopWalk, nil},
				// Wrapping is the interesting one: it is what a caller does to
				// carry a reason back out, and it is the case a == comparison
				// silently gets wrong.
				{"wrapped sentinel", fmt.Errorf("found it at record 1: %w", ErrStopWalk), nil},
				{"unrelated error", foreign, foreign},
			}

			for _, sc := range cases {
				t.Run(sc.name, func(t *testing.T) {
					calls := 0
					err := tc.walk(tc.open(t), func() error { calls++; return sc.give })

					switch {
					case sc.want == nil && err != nil:
						t.Errorf("%s returned %v, want nil", tc.name, err)
					case sc.want != nil && !errors.Is(err, sc.want):
						t.Errorf("%s returned %v, want it to wrap %v", tc.name, err, sc.want)
					}
					if calls != 1 {
						t.Errorf("callback ran %d times after asking to stop, want 1", calls)
					}
				})
			}
		})
	}
}

// TestCorpusWalkDeletedStops covers the one Walk method the hermetic table
// cannot, for both of its entry points.
//
// Stopping matters more here than anywhere else in the package: WalkDeleted
// runs three scans in sequence, and the last of them reads every unallocated
// block on the volume. A caller who has seen enough after the first record
// should not pay for that, so the stop has to skip the phases that follow
// rather than merely suppress their output.
func TestCorpusWalkDeletedStops(t *testing.T) {
	vol, cleanup := corpusVolume(t)
	defer cleanup()

	total := 0
	if err := vol.WalkDeleted(nil, func(DeletedRecord) error { total++; return nil }); err != nil {
		t.Fatalf("WalkDeleted failed: %v", err)
	}
	if total < 2 {
		t.Skipf("this image yields %d recovered records; stopping after the first would prove nothing", total)
	}
	t.Logf("%d recovered records available to stop after", total)

	run := func(name string, walk func(cb func(DeletedRecord) error) error, give, want error) {
		t.Run(name, func(t *testing.T) {
			calls := 0
			err := walk(func(DeletedRecord) error { calls++; return give })
			switch {
			case want == nil && err != nil:
				t.Errorf("returned %v, want nil", err)
			case want != nil && !errors.Is(err, want):
				t.Errorf("returned %v, want it to wrap %v", err, want)
			}
			if calls != 1 {
				t.Errorf("callback ran %d times after asking to stop, want 1", calls)
			}
		})
	}

	foreign := errors.New("caller gave up")
	plain := func(cb func(DeletedRecord) error) error { return vol.WalkDeleted(nil, cb) }
	withCtx := func(cb func(DeletedRecord) error) error {
		return vol.WalkDeletedContext(context.Background(), nil, cb)
	}

	run("WalkDeleted/bare sentinel", plain, ErrStopWalk, nil)
	run("WalkDeleted/wrapped sentinel", plain, fmt.Errorf("seen enough: %w", ErrStopWalk), nil)
	run("WalkDeleted/unrelated error", plain, foreign, foreign)
	run("WalkDeletedContext/bare sentinel", withCtx, ErrStopWalk, nil)
	run("WalkDeletedContext/unrelated error", withCtx, foreign, foreign)
}
