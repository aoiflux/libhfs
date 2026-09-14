package libhfs

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestReportVolumeSummary(t *testing.T) {
	vol, _ := openValidTree(t)

	rep, err := vol.Report(nil)
	if err != nil {
		t.Fatalf("Report failed: %v", err)
	}

	if rep.Version != ReportVersion {
		t.Fatalf("Version = %d, want %d", rep.Version, ReportVersion)
	}
	if rep.Generated.IsZero() {
		t.Fatal("Generated was not stamped")
	}

	hdr := vol.Header()
	if rep.Volume.Kind != string(vol.Kind()) {
		t.Fatalf("Kind = %q, want %q", rep.Volume.Kind, vol.Kind())
	}
	if rep.Volume.BlockSize != hdr.BlockSize || rep.Volume.TotalBlocks != hdr.TotalBlocks {
		t.Fatalf("geometry does not match the header: %#v", rep.Volume)
	}
	if rep.Volume.BaseOffset != vol.BaseOffset() {
		t.Fatalf("BaseOffset = %d, want %d", rep.Volume.BaseOffset, vol.BaseOffset())
	}
	if want := uint64(hdr.TotalBlocks) * uint64(hdr.BlockSize); rep.Volume.TotalBytes != want {
		t.Fatalf("TotalBytes = %d, want %d", rep.Volume.TotalBytes, want)
	}
	if rep.Capabilities != vol.Capabilities() {
		t.Fatalf("Capabilities = %#v, want %#v", rep.Capabilities, vol.Capabilities())
	}
}

// TestReportOmitsFilesByDefault pins the default that keeps a summary cheap.
func TestReportOmitsFilesByDefault(t *testing.T) {
	vol, _ := openValidTree(t)

	rep, err := vol.Report(nil)
	if err != nil {
		t.Fatalf("Report failed: %v", err)
	}
	if len(rep.Files) != 0 {
		t.Fatalf("default report listed %d files, want none", len(rep.Files))
	}
	if rep.FilesTruncated {
		t.Fatal("FilesTruncated set on a report with no listing")
	}

	withFiles, err := vol.Report(&ReportOptions{IncludeFiles: true, MaxFiles: -1})
	if err != nil {
		t.Fatalf("Report failed: %v", err)
	}
	if len(withFiles.Files) == 0 {
		t.Fatal("IncludeFiles produced no listing")
	}
	if withFiles.FilesTruncated {
		t.Fatal("an unbounded listing reported itself truncated")
	}
}

func TestReportTruncatesFileList(t *testing.T) {
	vol, _ := openValidTree(t)

	rep, err := vol.Report(&ReportOptions{IncludeFiles: true, MaxFiles: 2})
	if err != nil {
		t.Fatalf("Report failed: %v", err)
	}
	if len(rep.Files) != 2 {
		t.Fatalf("listed %d files, want 2", len(rep.Files))
	}
	if !rep.FilesTruncated {
		t.Fatal("a bounded listing that hit the bound did not report itself truncated")
	}
}

func TestReportFileSummaryContents(t *testing.T) {
	vol, _ := openValidTree(t)

	rep, err := vol.Report(&ReportOptions{IncludeFiles: true, MaxFiles: -1})
	if err != nil {
		t.Fatalf("Report failed: %v", err)
	}

	var found *FileSummary
	want := validTreeFilePath(0, 0)
	for i := range rep.Files {
		if rep.Files[i].Path == want {
			found = &rep.Files[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("listing has no entry for %q", want)
	}

	rec, err := vol.OpenPath(want)
	if err != nil {
		t.Fatalf("OpenPath failed: %v", err)
	}
	if found.CNID != rec.CNID {
		t.Fatalf("CNID = %d, want %d", found.CNID, rec.CNID)
	}
	if found.Type != "file" {
		t.Fatalf("Type = %q, want %q", found.Type, "file")
	}
	if found.Identity != rec.Identity().String() {
		t.Fatalf("Identity = %q, want %q", found.Identity, rec.Identity().String())
	}

	// Every path in the listing must be one WalkPaths would produce.
	for _, f := range rep.Files {
		if f.Path == "" {
			continue // orphans legitimately carry no path
		}
		if !strings.HasPrefix(f.Path, "/") {
			t.Fatalf("path %q is not absolute", f.Path)
		}
	}
}

func TestReportRoundTripsThroughJSON(t *testing.T) {
	vol, _ := openValidTree(t)

	rep, err := vol.Report(&ReportOptions{IncludeFiles: true, MaxFiles: 5})
	if err != nil {
		t.Fatalf("Report failed: %v", err)
	}

	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var back Report
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if back.Version != rep.Version || back.Volume.Kind != rep.Volume.Kind {
		t.Fatalf("round trip lost volume fields: %#v", back.Volume)
	}
	if len(back.Files) != len(rep.Files) {
		t.Fatalf("round trip lost files: %d vs %d", len(back.Files), len(rep.Files))
	}
	if back.Capabilities != rep.Capabilities {
		t.Fatalf("round trip lost capabilities: %#v", back.Capabilities)
	}

	// The tags must actually be in force, so that consumers see lowerCamelCase
	// keys rather than Go field names.
	for _, key := range []string{`"version"`, `"volume"`, `"blockSize"`, `"baseOffset"`, `"capabilities"`, `"extendedAttributes"`} {
		if !bytes.Contains(raw, []byte(key)) {
			t.Fatalf("marshalled report has no %s key: %s", key, raw)
		}
	}
}

// TestReportAbsentDatesMarshalAsNull covers the one place the report must
// contradict the underlying type: an unset header date arrives as the Unix
// epoch, not as the zero time, and reporting 1970 would invent a date.
func TestReportAbsentDatesMarshalAsNull(t *testing.T) {
	vol, _ := openValidTree(t)

	rep, err := vol.Report(&ReportOptions{IncludeFiles: true, MaxFiles: 5})
	if err != nil {
		t.Fatalf("Report failed: %v", err)
	}

	hdr := vol.Header()
	if !hdr.CreateTime.Equal(time.Unix(0, 0)) {
		t.Skipf("fixture volume has a real create date (%v); nothing to assert", hdr.CreateTime)
	}
	if rep.Volume.Created != nil {
		t.Fatalf("an unset header date was reported as %v, want null", *rep.Volume.Created)
	}

	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	if !bytes.Contains(raw, []byte(`"created":null`)) {
		t.Fatalf("absent dates did not marshal as null: %s", raw)
	}
	if bytes.Contains(raw, []byte(`"1970-01-01T00:00:00Z"`)) {
		t.Fatalf("an absent date was reported as the Unix epoch: %s", raw)
	}
	if bytes.Contains(raw, []byte(`"0001-01-01T00:00:00Z"`)) {
		t.Fatalf("an absent date was reported as the zero time: %s", raw)
	}
}

func TestReportIncludesVolumeIdentifier(t *testing.T) {
	const (
		high = uint32(0xDEADBEEF)
		low  = uint32(0x0BADF00D)
	)

	img := buildValidCatalogImage(t)
	binary.BigEndian.PutUint32(img[volHdrFinderInfoWord(6):volHdrFinderInfoWord(6)+4], high)
	binary.BigEndian.PutUint32(img[volHdrFinderInfoWord(7):volHdrFinderInfoWord(7)+4], low)

	vol, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	rep, err := vol.Report(nil)
	if err != nil {
		t.Fatalf("Report failed: %v", err)
	}
	if got, want := rep.Volume.Identifier, "DEADBEEF0BADF00D"; got != want {
		t.Fatalf("Identifier = %q, want %q", got, want)
	}
	if rep.Volume.UUID == "" {
		t.Fatal("UUID was not derived")
	}
	if rep.Volume.UUID == rep.Volume.Identifier {
		t.Fatal("the report conflates the raw identifier with the derived UUID")
	}
}

func TestReportContextCancel(t *testing.T) {
	vol, _ := openValidTree(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := vol.ReportContext(ctx, &ReportOptions{IncludeFiles: true, MaxFiles: -1}); err == nil {
		t.Fatal("expected a cancelled report to fail")
	}

	// A summary without a listing does no walking, so cancellation cannot
	// reach it; it must still succeed rather than fail spuriously.
	if _, err := vol.ReportContext(ctx, nil); err != nil {
		t.Fatalf("a summary-only report failed on a cancelled context: %v", err)
	}
}

// TestReportDescribesCompressedFiles covers the one place the listing could
// quietly contradict the rest of the package.
//
// WalkPaths yields decoded records, and decmpfs compression is recorded in an
// extended attribute rather than in the catalog, so a listing built straight
// from the walk calls every compressed file uncompressed and zero-length.
func TestReportDescribesCompressedFiles(t *testing.T) {
	vol := openDecmpfsFixture(t)

	rep, err := vol.Report(&ReportOptions{IncludeFiles: true, MaxFiles: -1})
	if err != nil {
		t.Fatalf("Report failed: %v", err)
	}

	var found *FileSummary
	for i := range rep.Files {
		if rep.Files[i].CNID == dcInlineCNID {
			found = &rep.Files[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("listing has no entry for CNID %d: %+v", dcInlineCNID, rep.Files)
	}

	// The record the rest of the API hands out for the same file.
	rec, err := vol.OpenCNID(dcInlineCNID)
	if err != nil {
		t.Fatalf("OpenCNID failed: %v", err)
	}

	if !found.Compressed {
		t.Error("a decmpfs-compressed file was reported as uncompressed")
	}
	if found.CompressionType != rec.CompressionType {
		t.Errorf("CompressionType = %d, want %d", found.CompressionType, rec.CompressionType)
	}
	if found.Size != rec.DataFork.LogicalSize {
		t.Errorf("Size = %d, want %d (the uncompressed size the package reports elsewhere)",
			found.Size, rec.DataFork.LogicalSize)
	}
	if found.Size == 0 {
		t.Error("a compressed file was reported as zero-length")
	}
}
