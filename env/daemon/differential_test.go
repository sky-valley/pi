package daemon

import (
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// TestListFilesAgainstRust lists random trees under random ignore files with
// both daemons and compares the listings. It needs the Rust daemon:
//
//	PI_ENV_RUST_DAEMON=/path/to/rust/pi-env go test ./daemon -run TestListFilesAgainstRust
func TestListFilesAgainstRust(t *testing.T) {
	rust := os.Getenv("PI_ENV_RUST_DAEMON")
	if rust == "" {
		t.Skip("set PI_ENV_RUST_DAEMON to the Rust pi-env to compare listings with it")
	}
	if runtime.GOOS == "windows" {
		t.Skip("Unix trees")
	}
	seeds, _ := strconv.Atoi(os.Getenv("PI_ENV_SEEDS"))
	if seeds == 0 {
		seeds = 200
	}
	root := t.TempDir()
	env := append(os.Environ(), "HOME="+filepath.Join(root, "home"), "GIT_CONFIG_GLOBAL="+filepath.Join(root, "gitconfig"), "GIT_CONFIG_SYSTEM="+os.DevNull)
	daemons := []*daemonSession{startDaemon(t, rust, env...), startDaemon(t, goDaemon, env...)}
	filtered := 0
	for seed := 1; seed <= seeds; seed++ {
		dir := filepath.Join(root, "t"+strconv.Itoa(seed))
		files := randomIgnoreTree(rand.New(rand.NewSource(int64(seed))))
		for path, content := range files {
			full := filepath.Join(dir, path)
			if strings.HasSuffix(path, "/") {
				os.MkdirAll(full, 0o755)
				continue
			}
			os.MkdirAll(filepath.Dir(full), 0o755)
			if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		unfiltered := daemons[1].request(`{"op":"listFiles","path":`+strconv.Quote(dir)+`,"gitignore":false}`, nil)
		if listed := daemons[1].request(`{"op":"listFiles","path":`+strconv.Quote(dir)+`}`, nil); string(listed[0].Payload) != string(unfiltered[0].Payload) {
			filtered++
		}
		for _, options := range []string{``, `,"hidden":false`, `,"exclude":["n","m.x"]`} {
			for _, sub := range []string{"", "/d"} {
				request := `{"op":"listFiles","path":` + strconv.Quote(dir+sub) + options + `}`
				var replies [2]string
				for i, d := range daemons {
					for _, f := range d.request(request, nil) {
						replies[i] += strconv.Itoa(int(f.Kind)) + " " + string(f.JSON) + " " + strings.ReplaceAll(string(f.Payload), "\x00", " | ")
					}
				}
				if replies[0] != replies[1] {
					t.Fatalf("seed %d %s\nignore files: %q\nrust: %s\ngo:   %s", seed, request, ignoreFiles(files), replies[0], replies[1])
				}
			}
		}
	}
	// The trees are only a test when their rules ignore something.
	if filtered < seeds/4 {
		t.Fatalf("ignore rules filtered only %d of %d trees", filtered, seeds)
	}
	t.Logf("%d trees, %d with ignored entries", seeds, filtered)
}

func ignoreFiles(files map[string]string) map[string]string {
	out := map[string]string{}
	for path, content := range files {
		if strings.HasSuffix(path, ".gitignore") || strings.HasSuffix(path, "exclude") {
			out[path] = content
		}
	}
	return out
}

// randomIgnoreTree is a small work tree whose names and ignore rules are
// drawn from the same alphabet, so rules often match.
func randomIgnoreTree(r *rand.Rand) map[string]string {
	names := []string{"a", "b", "ab", "a.x", "b.x", "m.x", ".h", "n", "d", "é", "[a]", "a b", "#c", "!d", "x*", "a?", "{a,b}", "a\\b", "aé"}
	pick := func() string { return names[r.Intn(len(names))] }
	pieces := []string{"a", "b", "*", "?", "**", "/", "[ab]", "[!a]", "[a-c]", "[]a]", "[é]", ".x", "{a,b}", "\\*", "\\", "!", "#", " ", "é", "[", "]", "-", "d", "n", ",", "}"}
	rule := func() string {
		var b strings.Builder
		for n := 1 + r.Intn(4); n > 0; n-- {
			b.WriteString(pieces[r.Intn(len(pieces))])
		}
		return b.String()
	}
	rules := func() string {
		var b strings.Builder
		for n := 1 + r.Intn(5); n > 0; n-- {
			if r.Intn(4) == 0 {
				b.WriteString("!")
			}
			b.WriteString(rule())
			if r.Intn(5) == 0 {
				b.WriteString("/")
			}
			b.WriteString("\n")
		}
		return b.String()
	}
	files := map[string]string{".git/HEAD": "ref: refs/heads/main\n", ".gitignore": rules()}
	if r.Intn(2) == 0 {
		files["d/.gitignore"] = rules()
	}
	if r.Intn(3) == 0 {
		files[".git/info/exclude"] = rules()
	}
	for n := 5 + r.Intn(20); n > 0; n-- {
		path := pick()
		for r.Intn(2) == 0 {
			path += "/" + pick()
		}
		if r.Intn(3) == 0 {
			path = "d/" + path
		}
		if r.Intn(5) == 0 {
			path += "/"
		}
		files[path] = ""
	}
	// Keep files from also being directories.
	for path := range files {
		for dir := filepath.Dir(path); dir != "."; dir = filepath.Dir(dir) {
			delete(files, dir)
		}
	}
	return files
}

// TestRequestParsingAgainstRust sends JSON that serde_json and encoding/json
// read differently, and compares the replies of both daemons. It needs the
// Rust daemon (PI_ENV_RUST_DAEMON).
func TestRequestParsingAgainstRust(t *testing.T) {
	rust := os.Getenv("PI_ENV_RUST_DAEMON")
	if rust == "" {
		t.Skip("set PI_ENV_RUST_DAEMON to the Rust pi-env to compare request parsing with it")
	}
	daemons := []*daemonSession{startDaemon(t, rust), startDaemon(t, goDaemon)}
	b := string(rune(92)) // a backslash
	numbers := []string{"1", "1.0", "1e0", "1E0", "-0", "0", "01", "-1", "+1", "1.", ".5", "18446744073709551615", "18446744073709551616",
		"1e400", "-1e400", "1e-400", "4.9e-324", "2.5e-324", "1.7976931348623157e308", "1.7976931348623159e308", "9007199254740993", "0x10", "Infinity", "NaN", "true", "null", `"1"`, "[1]", "{}"}
	strs := []string{`"a"`, `"` + b + `u0041"`, `"` + b + `ud83d` + b + `ude00"`, `"` + b + `udc00"`, `"` + b + `ud800` + b + `ud800"`, `"` + b + `ud800` + b + `u0041"`, `"` + b + `ud800"`,
		`"` + b + `/"`, `"` + b + `x"`, `"` + b + `U0041"`, "\"\t\"", "\"\x01\"", "\"\x7f\"", "\"\xff\"", "\"\xc3\xa9\"", "\"\xed\xa0\x80\"", `"` + b + `u00e9"`, `"` + b + `"`, "'a'", "a"}
	var requests []string
	for _, n := range numbers {
		requests = append(requests, `{"op":"close","handle":`+n+`}`, `{"op":"listFiles","path":"/nonexistent","maxEntries":`+n+`}`)
		// Rust panics on a negative timeout or one beyond Duration, and never
		// replies; the Go daemon refuses those (see execRequestFromJSON).
		if !strings.HasPrefix(n, "-1") && !strings.HasPrefix(n, "1.79") {
			requests = append(requests, `{"op":"exec","argv":["true"],"cwd":"/nonexistent","timeoutMs":`+n+`}`)
		}
	}
	for _, s := range strs {
		requests = append(requests, `{"op":"lstat","path":`+s+`}`, `{"op":`+s+`}`, `{`+s+`:1,"op":"hello"}`)
	}
	requests = append(requests,
		"\xef\xbb\xbf{\"op\":\"close\",\"handle\":1}", "{\"op\":\"close\",\"handle\":1}\n\t ", "{\"op\":\"close\",\"handle\":1}\x00",
		` {"op":"close","handle":1}`, `{"op":"close","handle":1,}`, `{"op":"close","handle":1}{}`, `{"op":"close","op":"bogus","handle":1}`,
		`{"op":"close","handle":1}/`, `{"op" : "close" , "handle" : 1 }`, "{\"op\":\"close\",\r\n\"handle\":1}", `["op","close"]`, `"close"`, `1`, ``,
		`{"op":"close","handle":`+strings.Repeat("[", 126)+strings.Repeat("]", 126)+`}`, `{"op":"close","handle":`+strings.Repeat("[", 127)+strings.Repeat("]", 127)+`}`,
		`{"op":"close","handle":`+strings.Repeat(`{"a":`, 126)+`1`+strings.Repeat("}", 126)+`}`, `{"op":"close","handle":`+strings.Repeat(`{"a":`, 127)+`1`+strings.Repeat("}", 127)+`}`,
	)
	for _, request := range requests {
		var replies [2]string
		for i, d := range daemons {
			d.nextID++
			id := d.nextID
			d.send(1, id, []byte(request), nil)
			for _, f := range d.wait(id, finished) {
				replies[i] += strconv.Itoa(int(f.Kind)) + " " + numericFields.ReplaceAllString(string(f.JSON), `"$1":0`)
			}
		}
		if replies[0] != replies[1] {
			t.Errorf("%q\nrust: %s\ngo:   %s", request, replies[0], replies[1])
		}
	}
}
