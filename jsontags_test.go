package libhfs

import (
	"encoding/json"
	"reflect"
	"regexp"
	"testing"
	"time"
)

// camelKey matches the spelling this package used before v0.4.0. The other five
// libraries in this family tag exclusively in snake_case, and a pipeline that
// deserialises reports from more than one of them had to special-case exactly
// this one.
var camelKey = regexp.MustCompile(`[a-z][A-Z]`)

// exportedTaggedValues returns one instance of every exported type in this
// package that carries JSON tags.
//
// A populated Report reaches most of them by nesting, but only the ones the
// fixture happens to produce: with no anomalies there is no Anomaly, and with
// no files no FileSummary or TimeSummary. Marshalling each type directly as
// well means a key cannot escape the check by being absent from one volume.
func exportedTaggedValues(rep Report) []any {
	return []any{
		rep,
		Report{},
		VolumeSummary{},
		FileSummary{},
		TimeSummary{},
		Capabilities{},
		Anomaly{},
	}
}

// TestJSONKeysAreSnakeCase is the acceptance test for the v0.4.0 tag
// conversion. It walks the decoded document rather than regexing the raw bytes,
// because a string *value* containing camelCase — a filename, a link target, a
// hex Finder info blob — is not a violation and must not fail this.
func TestJSONKeysAreSnakeCase(t *testing.T) {
	vol, _ := openValidTree(t)
	rep, err := vol.Report(&ReportOptions{IncludeFiles: true, MaxFiles: 50})
	if err != nil {
		t.Fatalf("Report failed: %v", err)
	}

	for _, v := range exportedTaggedValues(rep) {
		name := reflect.TypeOf(v).Name()
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("Marshal(%s) failed: %v", name, err)
		}
		var decoded any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("Unmarshal(%s) failed: %v", name, err)
		}
		walkJSONKeys(decoded, func(key, path string) {
			if camelKey.MatchString(key) {
				t.Errorf("%s: key %q at %s is camelCase; every tag in this package is snake_case", name, key, path)
			}
		})
	}
}

// TestJSONTagsCoverEveryExportedReportField pins that the conversion was a
// rename and not a deletion: a field whose tag was dropped would marshal under
// its Go name, which is CamelCase and would be caught above, but a field that
// gained `json:"-"` would vanish silently and satisfy every other assertion
// here.
func TestJSONTagsCoverEveryExportedReportField(t *testing.T) {
	types := []reflect.Type{
		reflect.TypeFor[Report](),
		reflect.TypeFor[VolumeSummary](),
		reflect.TypeFor[FileSummary](),
		reflect.TypeFor[TimeSummary](),
		reflect.TypeFor[Capabilities](),
		reflect.TypeFor[Anomaly](),
	}
	for _, typ := range types {
		for i := range typ.NumField() {
			f := typ.Field(i)
			if !f.IsExported() {
				continue
			}
			tag, ok := f.Tag.Lookup("json")
			if !ok {
				t.Errorf("%s.%s has no json tag", typ.Name(), f.Name)
				continue
			}
			if tag == "-" {
				t.Errorf("%s.%s is excluded from the document", typ.Name(), f.Name)
			}
		}
	}
}

// walkJSONKeys visits every object key in a decoded JSON document, reporting
// the path it was found at so a failure names the field rather than only the
// key.
func walkJSONKeys(v any, visit func(key, path string)) {
	var walk func(v any, path string)
	walk = func(v any, path string) {
		switch t := v.(type) {
		case map[string]any:
			for k, child := range t {
				visit(k, path)
				walk(child, path+"."+k)
			}
		case []any:
			for _, child := range t {
				walk(child, path+"[]")
			}
		}
	}
	walk(v, "$")
}

// TestReportSchemaVersionSurvivesRoundTrip is the C10 acceptance criterion:
// every root document carries a schema version, and it comes back non-zero.
func TestReportSchemaVersionSurvivesRoundTrip(t *testing.T) {
	vol, _ := openValidTree(t)
	rep, err := vol.Report(nil)
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

	if back.SchemaVersion == 0 {
		t.Fatalf("SchemaVersion is zero after a round trip: %s", raw)
	}
	if back.SchemaVersion != ReportVersion {
		t.Fatalf("SchemaVersion = %d, want %d", back.SchemaVersion, ReportVersion)
	}
	if back.LibraryVersion == "" {
		t.Fatalf("LibraryVersion is empty after a round trip: %s", raw)
	}
	if back.Generated.IsZero() || back.Generated.After(time.Now().Add(time.Minute)) {
		t.Fatalf("Generated is implausible: %v", back.Generated)
	}
}
