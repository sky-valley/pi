package schema

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The official JSON-Schema-Test-Suite, drafts 2020-12 and 7, as upstream's
// test/conformance.test.ts runs it: every test, each also checking that
// errors agree with check. A test listed in knownFailures must fail; when one
// starts passing, remove it from the list.

// knownFailures is upstream's KNOWN_FAILURES at 6536f8012, verbatim.
var knownFailures = map[string]bool{
	// Vocabularies switched off by a custom meta-schema are not supported.
	"draft2020-12/vocabulary > schema that uses custom metaschema with with no validation vocabulary > no validation: invalid number, but it still validates": true,
	"draft2020-12/optional/format-assertion > schema that uses custom metaschema with format-assertion: false > format-assertion: false: invalid string":      true,
	"draft2020-12/optional/format-assertion > schema that uses custom metaschema with format-assertion: true > format-assertion: true: invalid string":        true,
	// A $ref to a document of another draft is evaluated with the compile's draft.
	"draft2020-12/optional/cross-draft > refs to historic drafts are processed as historic drafts > first item not a string is valid": true,
	"draft7/optional/cross-draft > refs to future drafts are processed as future drafts > missing bar is invalid":                     true,
	// contentMediaType and contentEncoding are annotations.
	"draft7/optional/content > validation of string-encoded content based on media type > an invalid JSON document":             true,
	"draft7/optional/content > validation of binary string-encoding > an invalid base64 string (% is not a valid character)":    true,
	"draft7/optional/content > validation of binary-encoded media type documents > a validly-encoded invalid JSON document":     true,
	"draft7/optional/content > validation of binary-encoded media type documents > an invalid base64 string that is valid JSON": true,
}

// re2Failures are the tests the port fails where upstream passes because RE2
// cannot express the ECMAScript pattern (each pattern then matches every
// string). Empty: no suite pattern needs a lookaround or a backreference.
var re2Failures = map[string]bool{}

// formatFile reports whether a suite file asserts formats. Upstream runs
// those files with its `formats` table; the port's format checks are the
// schema/formats package, which tests them there, so this test skips them.
func formatFile(name string) bool { return strings.Contains(name, "/optional/format/") }

type suiteFile struct {
	name   string
	draft  string
	groups []any
}

const suiteRoot = "testdata/json-schema-test-suite"

func suiteFiles(t *testing.T) []suiteFile {
	t.Helper()
	var files []suiteFile
	for _, d := range []struct{ dir, draft string }{{"draft2020-12", Draft2020}, {"draft7", Draft7}} {
		root := filepath.Join(suiteRoot, "tests")
		for _, path := range jsonFiles(t, filepath.Join(root, d.dir)) {
			rel, _ := filepath.Rel(root, path)
			groups, ok := readJSON(t, path).([]any)
			if !ok {
				t.Fatalf("%s: not a list of groups", path)
			}
			files = append(files, suiteFile{strings.TrimSuffix(filepath.ToSlash(rel), ".json"), d.draft, groups})
		}
	}
	if len(files) == 0 {
		t.Fatal("no suite files")
	}
	return files
}

// suiteSchemas are the remote documents and meta-schemas the tests
// reference, by URI.
func suiteSchemas(t *testing.T) map[string]any {
	t.Helper()
	schemas := map[string]any{}
	remotes := filepath.Join(suiteRoot, "remotes")
	for _, path := range jsonFiles(t, remotes) {
		rel, _ := filepath.Rel(remotes, path)
		schemas["http://localhost:1234/"+filepath.ToSlash(rel)] = readJSON(t, path)
	}
	meta := "testdata/meta-schemas"
	schemas["http://json-schema.org/draft-07/schema"] = readJSON(t, filepath.Join(meta, "draft-07-schema.json"))
	schemas["https://json-schema.org/draft/2020-12/schema"] = readJSON(t, filepath.Join(meta, "draft2020-12-schema.json"))
	for _, vocabulary := range []string{"core", "applicator", "unevaluated", "validation", "meta-data", "format-annotation", "content"} {
		schemas["https://json-schema.org/draft/2020-12/meta/"+vocabulary] = readJSON(t, filepath.Join(meta, "draft2020-12-meta-"+vocabulary+".json"))
	}
	return schemas
}

func readJSON(t *testing.T, path string) any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	v, err := Decode(data)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return v
}

func jsonFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".json") {
			out = append(out, path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

func TestConformance(t *testing.T) {
	schemas := suiteSchemas(t)
	var passed, failedAsKnown, skippedFormat, total int
	for _, file := range suiteFiles(t) {
		for _, g := range file.groups {
			group := g.(Object)
			description, _ := group.Get("description")
			schema, _ := group.Get("schema")
			tests, _ := group.Get("tests")
			for _, tc := range tests.([]any) {
				test := tc.(Object)
				testDescription, _ := test.Get("description")
				data, _ := test.Get("data")
				valid, _ := test.Get("valid")
				name := file.name + " > " + description.(string) + " > " + testDescription.(string)
				total++
				if formatFile(file.name) {
					skippedFormat++
					continue
				}
				v, err := Compile(schema, Options{Draft: file.draft, Schemas: schemas})
				if err != nil {
					t.Errorf("%s: compile: %v", name, err)
					continue
				}
				got := v.Check(data)
				if errs, err := v.Errors(data); err != nil || (len(errs) == 0) != got {
					t.Errorf("%s (errors agree with check): check %v, errors %v, %v", name, got, errs, err)
				}
				known := knownFailures[name] || re2Failures[name]
				switch {
				case known && got == valid:
					t.Errorf("%s: listed as a known failure but passes; remove it from the list", name)
				case known:
					failedAsKnown++
				case got != valid:
					t.Errorf("%s: check %v, want %v", name, got, valid)
				default:
					passed++
				}
			}
		}
	}
	t.Logf("suite: %d tests; %d pass, %d known failures, %d in format files skipped", total, passed, failedAsKnown, skippedFormat)
}

// TestConformanceCore pins upstream's headline: of the core tests (not
// optional/), all but the one vocabulary test pass.
func TestConformanceCore(t *testing.T) {
	schemas := suiteSchemas(t)
	var core, pass int
	for _, file := range suiteFiles(t) {
		if strings.Contains(file.name, "/optional/") {
			continue
		}
		for _, g := range file.groups {
			group := g.(Object)
			schema, _ := group.Get("schema")
			tests, _ := group.Get("tests")
			for _, tc := range tests.([]any) {
				test := tc.(Object)
				data, _ := test.Get("data")
				valid, _ := test.Get("valid")
				core++
				if v, err := Compile(schema, Options{Draft: file.draft, Schemas: schemas}); err == nil && v.Check(data) == valid {
					pass++
				}
			}
		}
	}
	if core != 2223 || pass != 2222 {
		t.Errorf("core tests: %d/%d pass, upstream 2222/2223", pass, core)
	}
}
