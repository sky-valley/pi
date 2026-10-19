package coding

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// Where pi trims it trims with String.prototype.trim, which strips U+FEFF and
// keeps U+0085 — the reverse of strings.TrimSpace — and where pi hands text to
// the yaml parser, the ignore matcher or JSON.parse, each accepts only its own
// whitespace. Every expectation here is pi's own, captured under node at
// 7140838fd by testdata/jstrim/capture-jstrim.mts, which also records each
// fixture this test replays.

const jstrimCaptureFile = "testdata/jstrim/jstrim-7140838fd.json"

type jstrimCapture struct {
	Frontmatter map[string]struct {
		Doc         string            `json:"doc"`
		Frontmatter map[string]string `json:"frontmatter"`
		Body        string            `json:"body"`
		Error       string            `json:"error"`
	} `json:"frontmatter"`
	Skills map[string]struct {
		Tree         map[string]string `json:"tree"`
		Names        []string          `json:"names"`
		Descriptions map[string]string `json:"descriptions"`
		Diagnostics  []struct {
			Message string `json:"message"`
			Path    string `json:"path"`
		} `json:"diagnostics"`
	} `json:"skills"`
	GitPaths map[string]struct {
		Tree   map[string]string `json:"tree"`
		Result *struct {
			RepoDir      string `json:"repoDir"`
			CommonGitDir string `json:"commonGitDir"`
		} `json:"result"`
	} `json:"gitPaths"`
	ModelReferences []struct {
		Reference string  `json:"reference"`
		Match     *string `json:"match"`
	} `json:"modelReferences"`
	PNGBase64 string `json:"pngBase64"`
	Images    []struct {
		MimeType       string   `json:"mimeType"`
		OK             bool     `json:"ok"`
		ResultMimeType string   `json:"resultMimeType"`
		Hints          []string `json:"hints"`
	} `json:"images"`
	Sessions struct {
		Cwd           string             `json:"cwd"`
		Files         map[string]string  `json:"files"`
		Entries       []string           `json:"entries"`
		MessageCounts map[string]int     `json:"messageCounts"`
		Found         map[string]*string `json:"found"`
	} `json:"sessions"`
}

func loadJSTrimCapture(t *testing.T) jstrimCapture {
	t.Helper()
	data, err := os.ReadFile(jstrimCaptureFile)
	if err != nil {
		t.Fatal(err)
	}
	var c jstrimCapture
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatalf("%s: %v; rerun capture-jstrim.mts", jstrimCaptureFile, err)
	}
	return c
}

// writeJSTrimTree writes a recorded fixture tree under a fresh root, returned
// with symlinks resolved so path comparisons see what the loaders see.
func writeJSTrimTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for rel, content := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// The yaml parser's own whitespace is space and tab: a key or a plain scalar
// keeps any other padding, save the U+FEFF its lexer drops from the start of a
// line read before the document begins. The body after the closing fence is
// trimmed as JavaScript trims.
func TestJSTrimFrontmatter(t *testing.T) {
	for name, want := range loadJSTrimCapture(t).Frontmatter {
		t.Run(name, func(t *testing.T) {
			if want.Error != "" {
				t.Fatalf("capture recorded a yaml error (%s); this test replays parsed documents only", want.Error)
			}
			fm, body := parseFrontmatter(want.Doc)
			got := map[string]string{}
			for key, value := range fm {
				got[key] = value.value
			}
			if !reflect.DeepEqual(got, want.Frontmatter) {
				t.Errorf("frontmatter = %+q, pi = %+q", got, want.Frontmatter)
			}
			if body != want.Body {
				t.Errorf("body = %+q, pi = %+q", body, want.Body)
			}
		})
	}
}

// A description counts only when non-blank as JavaScript trims, and an ignore
// file line is blank or a comment by that trim too, while the rule it leaves is
// the ignore matcher's (7.0.8, what pi's lock pins): a leading BOM dropped, a
// trailing CR run and then a trailing run of spaces dropped unless escaped,
// every other whitespace kept, and a body left empty matching every path.
func TestJSTrimSkills(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture trees have file names Windows cannot create")
	}
	for name, want := range loadJSTrimCapture(t).Skills {
		t.Run(name, func(t *testing.T) {
			root := writeJSTrimTree(t, want.Tree)
			skills, diags := loadSkillsFromDir(root)

			var names []string
			descriptions := map[string]string{}
			for _, s := range skills {
				names = append(names, s.Name)
				descriptions[s.Name] = s.Description
			}
			sort.Strings(names)
			if !slices.Equal(names, want.Names) {
				t.Errorf("skills = %+q, pi = %+q", names, want.Names)
			}
			if !reflect.DeepEqual(descriptions, want.Descriptions) {
				t.Errorf("descriptions = %+q, pi = %+q", descriptions, want.Descriptions)
			}
			var gotDiags []string
			for _, d := range diags {
				rel, _ := filepath.Rel(root, d.Path)
				gotDiags = append(gotDiags, filepath.ToSlash(rel)+": "+d.Message)
			}
			var wantDiags []string
			for _, d := range want.Diagnostics {
				wantDiags = append(wantDiags, d.Path+": "+d.Message)
			}
			sort.Strings(gotDiags)
			if !slices.Equal(gotDiags, wantDiags) {
				t.Errorf("diagnostics = %q, pi = %q", gotDiags, wantDiags)
			}
		})
	}
}

func TestJSTrimGitPaths(t *testing.T) {
	for name, want := range loadJSTrimCapture(t).GitPaths {
		t.Run(name, func(t *testing.T) {
			root := writeJSTrimTree(t, want.Tree)
			got, ok := findGitPaths(filepath.Join(root, "wt"))
			if want.Result == nil {
				if ok {
					t.Fatalf("found %+v, pi found nothing", got)
				}
				return
			}
			if !ok {
				t.Fatalf("found nothing, pi found %+v", *want.Result)
			}
			repoDir, _ := filepath.Rel(root, got.repoDir)
			commonGitDir, _ := filepath.Rel(root, got.commonGitDir)
			if filepath.ToSlash(repoDir) != want.Result.RepoDir || filepath.ToSlash(commonGitDir) != want.Result.CommonGitDir {
				t.Fatalf("found {%s %s}, pi {%s %s}", repoDir, commonGitDir, want.Result.RepoDir, want.Result.CommonGitDir)
			}
		})
	}
}

// The capture ran pi's findExactModelReferenceMatch, which is
// resolveModelReference without thinking suffixes or fuzzy matching, every
// provider usable; its only callers are in interactive mode, which the port
// does not have, so the trim is checked on resolveModelReference itself.
func TestJSTrimModelReferences(t *testing.T) {
	models := []*ai.Model{
		{Provider: "anthropic", ID: "claude-sonnet-4-5", Name: "Claude Sonnet 4.5"},
		{Provider: "openai", ID: "gpt-4o", Name: "GPT-4o"},
	}
	for _, tc := range loadJSTrimCapture(t).ModelReferences {
		var got *ai.Model
		request := modelReferenceRequest{text: tc.Reference, thinkingSuffix: thinkingSuffixNone}
		if result := resolveModelReference(request, models, func(string) bool { return true }); result.status == referenceResolved {
			got = result.model
		}
		switch {
		case tc.Match == nil && got != nil:
			t.Errorf("%+q matched %s/%s, pi matched nothing", tc.Reference, got.Provider, got.ID)
		case tc.Match != nil && (got == nil || string(got.Provider)+"/"+got.ID != *tc.Match):
			t.Errorf("%+q matched %v, pi matched %s", tc.Reference, got, *tc.Match)
		}
	}
}

func TestJSTrimImageMimeTypes(t *testing.T) {
	c := loadJSTrimCapture(t)
	png, err := base64.StdEncoding.DecodeString(c.PNGBase64)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range c.Images {
		got := processImage(png, want.MimeType, false, nil)
		if got.Ok != want.OK || got.MimeType != want.ResultMimeType || !slices.Equal(got.Hints, want.Hints) {
			t.Errorf("processImage(%+q) = {ok %v %s %+q}, pi = {ok %v %s %+q}", want.MimeType,
				got.Ok, got.MimeType, got.Hints, want.OK, want.ResultMimeType, want.Hints)
		}
	}
}

// A session line is blank by JavaScript's trim, and otherwise one JSON value
// padded only by JSON's own whitespace.
func TestJSTrimSessionLines(t *testing.T) {
	want := loadJSTrimCapture(t).Sessions
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	quotedCwd, _ := json.Marshal(cwd)
	files := map[string]string{}
	for name, content := range want.Files {
		files[name] = strings.ReplaceAll(content, `"cwd":"`+want.Cwd+`"`, `"cwd":`+string(quotedCwd))
	}
	dir := writeJSTrimTree(t, files)

	data, err := os.ReadFile(filepath.Join(dir, "entries.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := readSessionEntries(data)
	var ids []string
	for _, entry := range entries {
		id, _ := entry["id"].(string)
		ids = append(ids, id)
	}
	if !slices.Equal(ids, want.Entries) {
		t.Errorf("entries = %q, pi = %q", ids, want.Entries)
	}

	counts := map[string]int{}
	for _, info := range ListSessions(cwd, dir) {
		counts[filepath.Base(info.Path)] = info.Messages
	}
	if !reflect.DeepEqual(counts, want.MessageCounts) {
		t.Errorf("listed message counts = %v, pi = %v", counts, want.MessageCounts)
	}

	for id, wantFile := range want.Found {
		path, ok := FindSessionByID(cwd, id, dir)
		switch {
		case wantFile == nil && ok:
			t.Errorf("FindSessionByID(%s) = %s, pi found nothing", id, path)
		case wantFile != nil && (!ok || filepath.Base(path) != *wantFile):
			t.Errorf("FindSessionByID(%s) = (%q, %v), pi found %s", id, path, ok, *wantFile)
		}
	}
}
