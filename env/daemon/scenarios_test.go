package daemon

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sky-valley/pi/env/internal/frame"
)

// Scenarios for TestOracle. Each runs in a fresh directory (%R in a request)
// against a fresh daemon whose HOME and git configuration point into that
// directory, so the user's global excludes do not leak in.

func writeFixture(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for path, content := range files {
		full := filepath.Join(root, filepath.FromSlash(path))
		if strings.HasSuffix(path, "/") {
			if err := os.MkdirAll(full, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func unixOnly() string {
	if runtime.GOOS == "windows" {
		return "Unix fixtures (symbolic links, FIFOs, shells)"
	}
	return ""
}

var (
	helloFields = regexp.MustCompile(`"(cwd|home|tmpdir)":"[^"]*"`)
	tempSuffix  = regexp.MustCompile(`(pre-|tmp-)[A-Za-z0-9]{6}`)
	spillName   = regexp.MustCompile(`pi-output-[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\.log`)
	spillDir    = regexp.MustCompile(`"spillPath":"[^"]*/(tmp-XXXXXX/pi-output-UUID\.log)"`)
)

func scenarios() []scenario {
	t := func(r *recorder) *testing.T { return r.t }
	return []scenario{
		{
			name:  "protocol",
			clean: []func(string) string{func(s string) string { return helloFields.ReplaceAllString(s, `"$1":"*"`) }},
			run: func(r *recorder) {
				r.do("hello", `{"op":"hello","protocol":1}`)
				r.do("unknown op", `{"op":"frobnicate"}`)
				r.do("no op", `{"path":"x"}`)
				r.do("not an object", `[1,2]`)
				r.do("null", `null`)
				r.do("invalid JSON", `{"op":`)
				r.do("trailing garbage", `{"op":"hello"} x`)
				r.do("lone surrogate", `{"op":"lstat","path":"\ud800x"}`)
				r.do("paired surrogates", `{"op":"lstat","path":"%R/\ud83d\ude00"}`)
				r.do("number out of range", `{"op":"lstat","path":"%R","n":1e400}`)
				r.do("deep nesting", `{"op":"lstat","a":`+strings.Repeat("[", 130)+strings.Repeat("]", 130)+`}`)
				r.do("nesting at the limit", `{"op":"close","a":`+strings.Repeat("[", 126)+strings.Repeat("]", 126)+`}`)
				r.do("missing path", `{"op":"lstat"}`)
				r.do("path not a string", `{"op":"lstat","path":7}`)
				r.do("handle as float", `{"op":"close","handle":1.0}`)
				r.do("handle negative zero", `{"op":"close","handle":-0}`)
				r.do("handle exponent", `{"op":"close","handle":1e0}`)
				r.do("close unknown handle", `{"op":"close","handle":42}`)
				r.do("pread unknown handle", `{"op":"pread","handle":42,"length":3}`)
				r.do("readdir unknown handle", `{"op":"readdir","handle":42,"max":3}`)
				// A frame without JSON reads as {}.
				r.nextID++
				id := r.nextID
				r.send(frame.Request, id, nil, nil)
				r.keep("empty JSON", r.wait(id, finished))
			},
		},
		{
			name: "escaping",
			skip: unixOnly,
			run: func(r *recorder) {
				name := "a\x01b\x1f\x7f\"q\\é\u2028\u2029<>&/"
				name = strings.TrimSuffix(name, "/")
				writeFixture(t(r), r.root, map[string]string{name: "x"})
				r.do("lstat odd name", `{"op":"lstat","path":"%R/a\u0001b\u001f\u007f\"q\\é\u2028\u2029<>&"}`)
				r.do("missing odd name", `{"op":"lstat","path":"%R/\b\f\n\r\t\u0000"}`)
				r.do("opendir odd", `{"op":"opendir","path":"%R"}`)
				r.do("readdir odd", `{"op":"readdir","handle":1,"max":10}`)
			},
		},
		{
			// Rust refuses a NUL byte in a path, argument or variable before
			// any system call, with an error that has no OS code.
			name: "nul",
			skip: unixOnly,
			run: func(r *recorder) {
				writeFixture(t(r), r.root, map[string]string{"file": "x", "dir/": ""})
				r.do("lstat", `{"op":"lstat","path":"%R/a\u0000b"}`)
				r.do("realpath", `{"op":"realpath","path":"%R/a\u0000b"}`)
				r.do("write", `{"op":"write","path":"%R/a\u0000b"}`, []byte("x")...)
				r.do("truncate", `{"op":"truncate","path":"%R/a\u0000b","size":0}`)
				r.do("fsync", `{"op":"fsync","path":"%R/a\u0000b"}`)
				r.do("rename from", `{"op":"rename","path":"%R/a\u0000b","to":"%R/c"}`)
				r.do("rename to", `{"op":"rename","path":"%R/file","to":"%R/a\u0000b"}`)
				r.do("mkdir", `{"op":"mkdir","path":"%R/a\u0000b","recursive":true}`)
				r.do("rm", `{"op":"rm","path":"%R/a\u0000b","force":true}`)
				r.do("mkdtemp", `{"op":"mkdtemp","path":"%R/a\u0000b"}`)
				r.do("open", `{"op":"open","path":"%R/a\u0000b"}`)
				r.do("open read mode", `{"op":"open","path":"%R/a\u0000b","mode":"read"}`)
				r.do("opendir", `{"op":"opendir","path":"%R/a\u0000b"}`)
				r.do("opendir trailing nul", `{"op":"opendir","path":"%R/dir\u0000"}`)
				r.do("listFiles", `{"op":"listFiles","path":"%R/a\u0000b"}`)
				r.do("exec cwd", `{"op":"exec","argv":["true"],"cwd":"%R/a\u0000b","env":{}}`)
				r.do("exec argument", `{"op":"exec","argv":["echo","a\u0000b"],"cwd":"%R","env":{}}`)
				r.do("exec variable", `{"op":"exec","argv":["true"],"cwd":"%R","env":{"A":"a\u0000b"}}`)
				r.do("exec variable name", `{"op":"exec","argv":["true"],"cwd":"%R","env":{"A\u0000B":"x"}}`)
				r.do("pty cwd", `{"op":"pty","argv":["true"],"cwd":"%R/a\u0000b","env":{},"cols":80,"rows":24}`)
				r.do("pty argument", `{"op":"pty","argv":["echo","a\u0000b"],"cwd":"%R","env":{},"cols":80,"rows":24}`)
				r.do("pty variable", `{"op":"pty","argv":["true"],"cwd":"%R","env":{"A":"a\u0000b"},"cols":80,"rows":24}`)
				id := r.start(strings.ReplaceAll(`{"op":"watch","targets":[{"path":"%R/a\u0000b"}],"mode":"polling"}`, "%R", jsonEscape(r.root)), nil)
				r.keep("watch", r.wait(id, func(f []frame.Frame) bool { return len(f) > 0 }))
				r.send(frame.Cancel, id, nil, nil)
				r.keep("watch closed", r.wait(id, finished)[1:])
			},
		},
		{
			// Rust reads its own variables with env::var, which passes over
			// values that are not UTF-8, and decodes paths lossily, so replies
			// never carry raw bytes. File names cannot be other than UTF-8 on
			// macOS, so the working directory, the shell and the watched file
			// below are not UTF-8 on Linux only.
			name:   "not UTF-8",
			skip:   needsPython,
			launch: notUTF8Launch,
			clean:  []func(string) string{func(s string) string { return foreignCwd.ReplaceAllString(s, `"cwd":"*"`) }},
			run: func(r *recorder) {
				r.do("hello", `{"op":"hello","protocol":1}`)
				// $SHELL, when it is not UTF-8, names a script that is not a
				// login shell: the default /bin/sh runs instead and exits 7.
				id := r.start(strings.ReplaceAll(`{"op":"pty","cwd":"%R","env":{},"cols":80,"rows":24}`, "%R", jsonEscape(r.root)), nil)
				r.wait(id, func(f []frame.Frame) bool { return len(f) > 0 })
				time.Sleep(500 * time.Millisecond)
				r.do("exit", `{"op":"ptyWrite","target":`+strconv.Itoa(int(id))+`}`, []byte("exit 7\n")...)
				frames := r.wait(id, finished)
				r.keep("login shell", frames[len(frames)-1:])
				if runtime.GOOS != "linux" {
					return
				}
				writeFixture(t(r), r.root, map[string]string{"w/a\xffb": "x"})
				id = r.start(strings.ReplaceAll(`{"op":"watch","targets":[{"path":"%R/w","recursive":true}],"mode":"native"}`, "%R", jsonEscape(r.root)), nil)
				r.keep("watch ready", r.wait(id, func(f []frame.Frame) bool { return len(f) > 0 }))
				f, err := os.OpenFile(filepath.Join(r.root, "w", "a\xffb"), os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t(r).Fatal(err)
				}
				f.WriteString("y")
				f.Close()
				r.keep("watch change", r.wait(id, func(f []frame.Frame) bool { return len(f) > 1 })[1:2])
				r.send(frame.Cancel, id, nil, nil)
				all := r.wait(id, finished)
				r.keep("watch closed", all[len(all)-1:])
			},
		},
		{
			// Command runs the program through the C library's execvp(3): which
			// failures move the search on to the next PATH entry, and whether a
			// file the kernel cannot execute runs through sh, is the library's
			// (macOS and bionic run it; musl, which the Linux builds use, fails).
			name: "execvp",
			skip: unixOnly,
			run: func(r *recorder) {
				writeFixture(t(r), r.root, map[string]string{
					"bin/script": "echo from script\n", "bin/dir/": "", "bin/plain": "x",
					"dirs/script/": "", "plains/script": "x", "later/script": "#!/bin/sh\necho later\n",
				})
				os.Chmod(filepath.Join(r.root, "bin", "script"), 0o755)
				os.Chmod(filepath.Join(r.root, "later", "script"), 0o755)
				r.do("script without shebang", `{"op":"exec","argv":["%R/bin/script"],"cwd":"%R","env":{}}`)
				r.do("relative script without shebang", `{"op":"exec","argv":["bin/script"],"cwd":"%R","env":{}}`)
				r.do("script in PATH", `{"op":"exec","argv":["script"],"cwd":"%R","env":{"PATH":"%R/bin:%R/later"}}`)
				r.do("directory in PATH", `{"op":"exec","argv":["dir"],"cwd":"%R","env":{"PATH":"%R/bin"}}`)
				r.do("directory then program", `{"op":"exec","argv":["script"],"cwd":"%R","env":{"PATH":"%R/dirs:%R/later"}}`)
				r.do("not executable in PATH", `{"op":"exec","argv":["plain"],"cwd":"%R","env":{"PATH":"%R/bin"}}`)
				r.do("not executable then program", `{"op":"exec","argv":["script"],"cwd":"%R","env":{"PATH":"%R/plains:%R/later"}}`)
				r.do("file as directory then program", `{"op":"exec","argv":["script"],"cwd":"%R","env":{"PATH":"%R/bin/plain:%R/later"}}`)
				r.do("missing then program", `{"op":"exec","argv":["script"],"cwd":"%R","env":{"PATH":"%R/nope:%R/later"}}`)
				r.do("missing everywhere", `{"op":"exec","argv":["nothing"],"cwd":"%R","env":{"PATH":"%R/nope:%R/bin"}}`)
				r.do("pty script without shebang", `{"op":"pty","argv":["%R/bin/script"],"cwd":"%R","env":{},"cols":80,"rows":24}`)
			},
		},
		{
			// The Rust daemon's Linux builds are static musl: realpath(3) is
			// musl's, not glibc's.
			name: "musl",
			skip: muslOnly,
			run: func(r *recorder) {
				writeFixture(t(r), r.root, map[string]string{"file": "", "dir/sub/": ""})
				symlink(t(r), "file", filepath.Join(r.root, "flink"))
				symlink(t(r), "dir/", filepath.Join(r.root, "dslash"))
				symlink(t(r), "nowhere", filepath.Join(r.root, "dangling"))
				// chainN is a chain of N links ending at file.
				for _, n := range []int{39, 40, 41} {
					previous := "file"
					for i := n; i >= 1; i-- {
						link := "chain" + strconv.Itoa(n) + "-" + strconv.Itoa(i)
						symlink(t(r), previous, filepath.Join(r.root, link))
						previous = link
					}
				}
				for _, path := range []string{
					"%R/chain39-1", "%R/chain40-1", "%R/chain41-1", "//", "//tmp", "///tmp", "//tmp/", "//%R", "/%R//dir",
					"%R/file/", "%R/file/.", "%R/file/..", "%R/file/x/..", "%R/missing/..", "%R/dangling/..", "%R/flink/",
					"%R/flink/.", "%R/dslash", "%R/dslash/sub/..", "%R/dir/sub/../..", "%R/dir/../file", "%R/.", "%R/..",
					"/..", "/../..", "%R/dir/sub/", "%R/dir//sub/.//", "%R/dangling",
				} {
					r.do("realpath "+path, `{"op":"realpath","path":"`+path+`"}`)
				}
			},
		},
		{
			// musl's execvp searches /usr/local/bin:/bin:/usr/bin when PATH is
			// unset, glibc /bin:/usr/bin.
			name: "musl default PATH",
			skip: func() string {
				if reason := muslOnly(); reason != "" {
					return reason
				}
				f, err := os.CreateTemp("/usr/local/bin", ".pi-env-oracle-")
				if err != nil {
					return "needs to write a program into /usr/local/bin (run as root, as in a container)"
				}
				f.Close()
				os.Remove(f.Name())
				return ""
			},
			run: func(r *recorder) {
				program := "/usr/local/bin/pi-env-oracle-local"
				if err := os.WriteFile(program, []byte("#!/bin/sh\necho local\n"), 0o755); err != nil {
					t(r).Fatal(err)
				}
				defer os.Remove(program)
				r.do("no PATH", `{"op":"exec","argv":["pi-env-oracle-local"],"cwd":"%R","env":{},"inheritEnv":false}`)
				r.do("PATH set", `{"op":"exec","argv":["pi-env-oracle-local"],"cwd":"%R","env":{"PATH":"/bin"},"inheritEnv":false}`)
			},
		},
		{
			name: "watch through links",
			skip: unixOnly,
			run:  watchThroughLinks,
		},
		{
			name: "stat",
			skip: unixOnly,
			run: func(r *recorder) {
				writeFixture(t(r), r.root, map[string]string{"a.txt": "hello", "dir/sub/": "", "dir/f": "12345678"})
				symlink(t(r), "a.txt", filepath.Join(r.root, "link"))
				symlink(t(r), "missing", filepath.Join(r.root, "dangling"))
				r.do("file", `{"op":"lstat","path":"%R/a.txt"}`)
				r.do("dir", `{"op":"lstat","path":"%R/dir"}`)
				r.do("dir trailing slash", `{"op":"lstat","path":"%R/dir/"}`)
				r.do("dir dot", `{"op":"lstat","path":"%R/dir/."}`)
				r.do("dir dotdot", `{"op":"lstat","path":"%R/dir/sub/.."}`)
				r.do("symlink", `{"op":"lstat","path":"%R/link"}`)
				r.do("dangling", `{"op":"lstat","path":"%R/dangling"}`)
				r.do("missing", `{"op":"lstat","path":"%R/nope"}`)
				r.do("through file", `{"op":"lstat","path":"%R/a.txt/x"}`)
				r.do("root", `{"op":"lstat","path":"/"}`)
				r.do("device", `{"op":"lstat","path":"/dev/null"}`)
				r.do("empty path", `{"op":"lstat","path":""}`)
			},
		},
		{
			name: "realpath",
			skip: unixOnly,
			run: func(r *recorder) {
				writeFixture(t(r), r.root, map[string]string{"CaseDir/File.txt": "x", "dir/sub/": "", "file": ""})
				symlink(t(r), "dir", filepath.Join(r.root, "rel"))
				symlink(t(r), filepath.Join(r.root, "dir", "sub"), filepath.Join(r.root, "abs"))
				symlink(t(r), "loop2", filepath.Join(r.root, "loop1"))
				symlink(t(r), "loop1", filepath.Join(r.root, "loop2"))
				symlink(t(r), "nowhere", filepath.Join(r.root, "dangling"))
				r.do("root", `{"op":"realpath","path":"%R"}`)
				r.do("case", `{"op":"realpath","path":"%R/casedir/file.TXT"}`)
				r.do("relative link", `{"op":"realpath","path":"%R/rel/sub"}`)
				r.do("absolute link", `{"op":"realpath","path":"%R/abs"}`)
				r.do("link then dotdot", `{"op":"realpath","path":"%R/abs/../sub/."}`)
				r.do("loop", `{"op":"realpath","path":"%R/loop1"}`)
				r.do("dangling", `{"op":"realpath","path":"%R/dangling"}`)
				r.do("missing", `{"op":"realpath","path":"%R/missing"}`)
				r.do("missing below missing", `{"op":"realpath","path":"%R/missing/x"}`)
				r.do("file with slash", `{"op":"realpath","path":"%R/file/"}`)
				r.do("file with dot", `{"op":"realpath","path":"%R/file/."}`)
				r.do("through file", `{"op":"realpath","path":"%R/file/x"}`)
				r.do("slash", `{"op":"realpath","path":"/"}`)
				r.do("double slash", `{"op":"realpath","path":"%R//dir///sub"}`)
				r.do("empty", `{"op":"realpath","path":""}`)
				r.do("tmp", `{"op":"realpath","path":"/tmp"}`)
			},
		},
		{
			name: "mkdir",
			skip: unixOnly,
			run: func(r *recorder) {
				writeFixture(t(r), r.root, map[string]string{"file": "", "dir/": ""})
				r.do("new", `{"op":"mkdir","path":"%R/new","recursive":false}`)
				r.do("existing", `{"op":"mkdir","path":"%R/dir","recursive":false}`)
				r.do("existing recursive", `{"op":"mkdir","path":"%R/dir","recursive":true}`)
				r.do("file recursive", `{"op":"mkdir","path":"%R/file","recursive":true}`)
				r.do("missing parent", `{"op":"mkdir","path":"%R/a/b","recursive":false}`)
				r.do("deep recursive", `{"op":"mkdir","path":"%R/a/b/c/d","recursive":true}`)
				r.do("through file", `{"op":"mkdir","path":"%R/file/x/y","recursive":true}`)
				r.do("through file plain", `{"op":"mkdir","path":"%R/file/x","recursive":false}`)
				r.do("lstat deep", `{"op":"lstat","path":"%R/a/b/c/d"}`)
			},
		},
		{
			name: "write",
			skip: unixOnly,
			run: func(r *recorder) {
				writeFixture(t(r), r.root, map[string]string{"file": "old", "dir/": ""})
				r.do("new with parents", `{"op":"write","path":"%R/x/y/z.txt","append":false}`, []byte("hello")...)
				r.do("read back", `{"op":"lstat","path":"%R/x/y/z.txt"}`)
				r.do("no parents", `{"op":"write","path":"%R/m/n.txt","append":false,"parents":false}`, []byte("x")...)
				r.do("onto dir", `{"op":"write","path":"%R/dir","append":false}`, []byte("x")...)
				r.do("parent is file", `{"op":"write","path":"%R/file/x","append":false}`, []byte("x")...)
				r.do("append", `{"op":"write","path":"%R/file","append":true}`, []byte("+new")...)
				r.do("size after append", `{"op":"lstat","path":"%R/file"}`)
				r.do("keep", `{"op":"write","path":"%R/kept","append":false,"keep":true}`, []byte("ab")...)
				r.do("chunk", `{"op":"writeChunk","handle":1}`, []byte("cd")...)
				r.do("chunk 2", `{"op":"writeChunk","handle":1}`, []byte("ef")...)
				r.do("size kept", `{"op":"lstat","path":"%R/kept"}`)
				r.do("close", `{"op":"close","handle":1}`)
				r.do("chunk after close", `{"op":"writeChunk","handle":1}`, []byte("x")...)
				r.do("open reader", `{"op":"open","path":"%R/kept"}`)
				r.do("chunk to reader", `{"op":"writeChunk","handle":2}`, []byte("x")...)
				r.do("chunk again", `{"op":"writeChunk","handle":2}`, []byte("x")...)
				r.do("opendir", `{"op":"opendir","path":"%R"}`)
				r.do("chunk to dir", `{"op":"writeChunk","handle":3}`, []byte("x")...)
				r.do("pread dir", `{"op":"pread","handle":3,"length":1}`)
			},
		},
		{
			name: "truncate-fsync-rename",
			skip: unixOnly,
			run: func(r *recorder) {
				writeFixture(t(r), r.root, map[string]string{"file": "0123456789", "dir/": "", "full/x": "", "other": "o"})
				r.do("truncate", `{"op":"truncate","path":"%R/file","size":3}`)
				r.do("size", `{"op":"lstat","path":"%R/file"}`)
				r.do("truncate grow", `{"op":"truncate","path":"%R/file","size":20}`)
				r.do("truncate dir", `{"op":"truncate","path":"%R/dir","size":0}`)
				r.do("truncate missing", `{"op":"truncate","path":"%R/nope","size":0}`)
				r.do("truncate huge", `{"op":"truncate","path":"%R/file","size":18446744073709551615}`)
				r.do("truncate no size", `{"op":"truncate","path":"%R/file"}`)
				r.do("fsync", `{"op":"fsync","path":"%R/file"}`)
				r.do("fsync dir", `{"op":"fsync","path":"%R/dir"}`)
				r.do("fsync missing", `{"op":"fsync","path":"%R/nope"}`)
				r.do("rename", `{"op":"rename","path":"%R/file","to":"%R/renamed"}`)
				r.do("rename missing", `{"op":"rename","path":"%R/file","to":"%R/x"}`)
				r.do("rename no to", `{"op":"rename","path":"%R/renamed"}`)
				r.do("dir onto file", `{"op":"rename","path":"%R/dir","to":"%R/other"}`)
				r.do("file onto dir", `{"op":"rename","path":"%R/other","to":"%R/dir"}`)
				r.do("dir onto full dir", `{"op":"rename","path":"%R/dir","to":"%R/full"}`)
				r.do("file onto file", `{"op":"rename","path":"%R/other","to":"%R/renamed"}`)
			},
		},
		{
			name: "rm",
			skip: unixOnly,
			run: func(r *recorder) {
				writeFixture(t(r), r.root, map[string]string{"file": "", "dir/a/b/c": "", "empty/": "", "target/x": ""})
				symlink(t(r), "target", filepath.Join(r.root, "link"))
				r.do("missing", `{"op":"rm","path":"%R/nope"}`)
				r.do("missing force", `{"op":"rm","path":"%R/nope","force":true}`)
				r.do("dir", `{"op":"rm","path":"%R/empty"}`)
				r.do("dir force", `{"op":"rm","path":"%R/empty","force":true}`)
				r.do("dir recursive", `{"op":"rm","path":"%R/dir","recursive":true}`)
				r.do("gone", `{"op":"lstat","path":"%R/dir"}`)
				r.do("file", `{"op":"rm","path":"%R/file"}`)
				r.do("link to dir", `{"op":"rm","path":"%R/link"}`)
				r.do("target kept", `{"op":"lstat","path":"%R/target/x"}`)
				r.do("through file", `{"op":"rm","path":"%R/target/x/y","force":true}`)
			},
		},
		{
			name:  "mkdtemp",
			skip:  unixOnly,
			clean: []func(string) string{func(s string) string { return tempSuffix.ReplaceAllString(s, "${1}XXXXXX") }},
			run: func(r *recorder) {
				writeFixture(t(r), r.root, map[string]string{"file": ""})
				r.do("new", `{"op":"mkdtemp","path":"%R/pre-"}`)
				r.do("missing parent", `{"op":"mkdtemp","path":"%R/missing/pre-"}`)
				r.do("parent is file", `{"op":"mkdtemp","path":"%R/file/pre-"}`)
			},
		},
		{
			name: "readers",
			skip: unixOnly,
			run: func(r *recorder) {
				writeFixture(t(r), r.root, map[string]string{"f.txt": "hello\nworld", "dir/": ""})
				symlink(t(r), "f.txt", filepath.Join(r.root, "link"))
				symlink(t(r), "missing", filepath.Join(r.root, "dangling"))
				makeFIFO(t(r), filepath.Join(r.root, "fifo"))
				r.do("open", `{"op":"open","path":"%R/f.txt"}`)
				r.do("pread", `{"op":"pread","handle":1,"offset":0,"length":5}`)
				r.do("pread middle", `{"op":"pread","handle":1,"offset":3,"length":100}`)
				r.do("pread past end", `{"op":"pread","handle":1,"offset":100,"length":4}`)
				r.do("pread empty", `{"op":"pread","handle":1,"offset":0,"length":0}`)
				r.do("read current", `{"op":"pread","handle":1,"length":3}`)
				r.do("read current again", `{"op":"pread","handle":1,"length":3}`)
				r.do("pread no length", `{"op":"pread","handle":1,"offset":0}`)
				r.do("fstat", `{"op":"fstat","handle":1}`)
				r.do("close", `{"op":"close","handle":1}`)
				r.do("pread closed", `{"op":"pread","handle":1,"offset":0,"length":1}`)
				r.do("open dir", `{"op":"open","path":"%R/dir"}`)
				r.do("open fifo", `{"op":"open","path":"%R/fifo"}`)
				r.do("open link", `{"op":"open","path":"%R/link"}`)
				r.do("open link no follow", `{"op":"open","path":"%R/link","noFollow":true}`)
				r.do("open dangling", `{"op":"open","path":"%R/dangling"}`)
				r.do("open dangling no follow", `{"op":"open","path":"%R/dangling","noFollow":true}`)
				r.do("open missing", `{"op":"open","path":"%R/missing"}`)
				r.do("read mode file", `{"op":"open","path":"%R/f.txt","mode":"read"}`)
				r.do("read mode dir", `{"op":"open","path":"%R/dir","mode":"read"}`)
				r.do("read dir", `{"op":"pread","handle":4,"length":10}`)
				r.do("read mode null", `{"op":"open","path":"/dev/null","mode":"read"}`)
				r.do("read null", `{"op":"pread","handle":5,"length":10}`)
				r.do("read mode missing", `{"op":"open","path":"%R/missing","mode":"read"}`)
				r.do("fstat dir handle", `{"op":"fstat","handle":4}`)
			},
		},
		{
			// A handle closed while a request uses it stays open until that
			// request is done, as dropping the Rust daemon's last Arc closes it:
			// an endless scan goes on after close until it is cancelled.
			name: "close in flight",
			skip: unixOnly,
			run: func(r *recorder) {
				r.do("open", `{"op":"open","path":"/dev/zero","mode":"read"}`)
				scan := r.start(`{"op":"scanLines","handle":1,"startLine":0}`, nil)
				time.Sleep(300 * time.Millisecond)
				r.do("close", `{"op":"close","handle":1}`)
				time.Sleep(300 * time.Millisecond)
				r.send(frame.Cancel, scan, nil, nil)
				r.keep("scan", r.wait(scan, finished))
				r.do("closed", `{"op":"fstat","handle":1}`)
			},
		},
		{
			name: "scanLines",
			skip: unixOnly,
			run: func(r *recorder) {
				writeFixture(t(r), r.root, map[string]string{
					"mixed":   "\xef\xbb\xbfa\n\xe2\x82\n\n\xef\xbb\xbfb\n\xc3\xa9",
					"plain":   "one\ntwo\nthree\n",
					"empty":   "",
					"bom":     "\xef\xbb\xbf",
					"partial": "\xef\xbb",
					"long":    strings.Repeat("é", 70000) + "\n" + strings.Repeat("x\n", 40000),
				})
				for i, name := range []string{"mixed", "plain", "empty", "bom", "partial", "long"} {
					handle := strconv.Itoa(i + 1)
					r.do("open "+name, `{"op":"open","path":"%R/`+name+`"}`)
					for _, lines := range []string{`"startLine":0`, `"startLine":1,"endLine":3`, `"startLine":2,"endLine":3`, `"startLine":9`, `"startLine":0,"endLine":1`, `"startLine":3,"endLine":40001`} {
						r.do("scan "+name+" "+lines, `{"op":"scanLines","handle":`+handle+`,`+lines+`}`)
					}
				}
				r.do("invalid range", `{"op":"scanLines","handle":1,"startLine":3,"endLine":3}`)
				r.do("no start", `{"op":"scanLines","handle":1}`)
			},
		},
		{
			name: "readdir",
			skip: unixOnly,
			run: func(r *recorder) {
				writeFixture(t(r), r.root, map[string]string{"d/b": "", "d/a": "", "d/c": "", "d/sub/": "", "file": ""})
				symlink(t(r), "a", filepath.Join(r.root, "d", "e"))
				r.do("opendir", `{"op":"opendir","path":"%R/d"}`)
				r.do("page 2", `{"op":"readdir","handle":1,"max":2}`)
				r.do("page 0", `{"op":"readdir","handle":1,"max":0}`)
				r.do("page rest", `{"op":"readdir","handle":1,"max":10}`)
				r.do("page after end", `{"op":"readdir","handle":1,"max":10}`)
				r.do("opendir again", `{"op":"opendir","path":"%R/d"}`)
				r.do("exact page", `{"op":"readdir","handle":2,"max":5}`)
				r.do("after exact page", `{"op":"readdir","handle":2,"max":5}`)
				r.do("opendir file", `{"op":"opendir","path":"%R/file"}`)
				r.do("opendir missing", `{"op":"opendir","path":"%R/missing"}`)
				r.do("open file", `{"op":"open","path":"%R/file"}`)
				r.do("readdir file handle", `{"op":"readdir","handle":3,"max":1}`)
				r.do("fstat dir handle", `{"op":"fstat","handle":1}`)
				r.do("readdir no max", `{"op":"readdir","handle":1}`)
			},
		},
		{
			name: "listFiles",
			skip: unixOnly,
			run: func(r *recorder) {
				writeFixture(t(r), r.root, map[string]string{
					"b.txt": "", "a/z.txt": "", "a/b/c.txt": "", "empty/": "", ".hidden/x": "", "A.txt": "",
					"node_modules/m/i.js": "", "skip/inner": "", ".git/HEAD": "", "é/ü": "", "a b/c d": "",
				})
				symlink(t(r), "a", filepath.Join(r.root, "link"))
				symlink(t(r), "missing", filepath.Join(r.root, "dangling"))
				symlink(t(r), r.root, filepath.Join(r.root, "a", "loop"))
				makeFIFO(t(r), filepath.Join(r.root, "fifo"))
				r.do("default", `{"op":"listFiles","path":"%R"}`)
				r.do("no hidden", `{"op":"listFiles","path":"%R","hidden":false}`)
				r.do("exclude", `{"op":"listFiles","path":"%R","exclude":["node_modules","skip"]}`)
				r.do("max entries", `{"op":"listFiles","path":"%R","maxEntries":2}`)
				r.do("max entries exact", `{"op":"listFiles","path":"%R","maxEntries":0}`)
				r.do("max bytes", `{"op":"listFiles","path":"%R","maxBytes":12}`)
				r.do("timeout", `{"op":"listFiles","path":"%R","timeoutMs":0}`)
				r.do("sub", `{"op":"listFiles","path":"%R/a"}`)
				r.do("through link", `{"op":"listFiles","path":"%R/link"}`)
				r.do("missing", `{"op":"listFiles","path":"%R/missing"}`)
				r.do("file", `{"op":"listFiles","path":"%R/b.txt"}`)
				r.do("bad max", `{"op":"listFiles","path":"%R","maxEntries":-1}`)
				r.do("bad exclude", `{"op":"listFiles","path":"%R","exclude":"x"}`)
				r.do("bad exclude item", `{"op":"listFiles","path":"%R","exclude":[1]}`)
				r.do("no path", `{"op":"listFiles"}`)
				r.do("null fields", `{"op":"listFiles","path":"%R","maxEntries":null,"hidden":null,"exclude":null}`)
			},
		},
		{
			name: "gitignore",
			skip: unixOnly,
			run: func(r *recorder) {
				fixture := map[string]string{}
				for path, content := range gitignoreFixture {
					content = strings.ReplaceAll(content, "GLOBAL", filepath.Join(r.root, "global-ignore"))
					fixture[path] = strings.ReplaceAll(content, "GITDIR", filepath.Join(r.root, "gitdir"))
				}
				writeFixture(t(r), r.root, fixture)
				symlink(t(r), "../sub", filepath.Join(r.root, "repo", "linked"))
				r.do("repo", `{"op":"listFiles","path":"%R/repo"}`)
				r.do("repo hidden off", `{"op":"listFiles","path":"%R/repo","hidden":false}`)
				r.do("repo gitignore off", `{"op":"listFiles","path":"%R/repo","gitignore":false}`)
				r.do("subdirectory", `{"op":"listFiles","path":"%R/repo/sub"}`)
				r.do("deep subdirectory", `{"op":"listFiles","path":"%R/repo/docs/a"}`)
				r.do("nested repo", `{"op":"listFiles","path":"%R/repo/nested"}`)
				r.do("not a repo", `{"op":"listFiles","path":"%R/plain"}`)
				r.do("worktree", `{"op":"listFiles","path":"%R/worktree"}`)
				r.do("root with trailing slash", `{"op":"listFiles","path":"%R/repo/"}`)
				r.do("root with dot", `{"op":"listFiles","path":"%R/repo/sub/./"}`)
			},
		},
		{
			name: "exec",
			skip: unixOnly,
			clean: []func(string) string{func(s string) string {
				return spillDir.ReplaceAllString(tempSuffix.ReplaceAllString(spillName.ReplaceAllString(s, "pi-output-UUID.log"), "${1}XXXXXX"), `"spillPath":"<tmp>/$1"`)
			}},
			run: func(r *recorder) {
				lines := ""
				for i := 1; i <= 25; i++ {
					lines += strconv.Itoa(i) + "\n"
				}
				writeFixture(t(r), r.root, map[string]string{"script": "echo from script\n", "file": "", "lines": lines})
				r.do("argv", `{"op":"exec","argv":["sh","-c","printf hi"],"cwd":"%R","env":{}}`)
				r.do("stderr", `{"op":"exec","argv":["sh","-c","printf oops >&2; exit 3"],"cwd":"%R","env":{}}`)
				r.do("signal", `{"op":"exec","argv":["sh","-c","kill -9 $$"],"cwd":"%R","env":{}}`)
				r.do("invalid utf-8", `{"op":"exec","argv":["sh","-c","printf 'a\\377\\342\\202b'"],"cwd":"%R","env":{}}`)
				r.do("leading bom", `{"op":"exec","argv":["sh","-c","printf '\\357\\273\\277x\\357\\273\\277'"],"cwd":"%R","env":{}}`)
				r.do("string command", `{"op":"exec","command":"echo $((6*7)) $0","cwd":"%R","env":{}}`)
				r.do("shell path", `{"op":"exec","command":"echo $0","shellPath":"/bin/sh","cwd":"%R","env":{}}`)
				r.do("missing shell", `{"op":"exec","command":"true","shellPath":"%R/nosh","cwd":"%R","env":{}}`)
				r.do("environment", `{"op":"exec","argv":["/usr/bin/env"],"cwd":"%R","env":{"B":"2","A":"1","C":7},"inheritEnv":false}`)
				r.do("path search", `{"op":"exec","argv":["sh","-c","printf ok"],"cwd":"%R","env":{"PATH":"/nonexistent:/bin"},"inheritEnv":false}`)
				r.do("no such program", `{"op":"exec","argv":["no-such-program-pi-env"],"cwd":"%R","env":{}}`)
				r.do("not executable", `{"op":"exec","argv":["%R/file"],"cwd":"%R","env":{}}`)
				r.do("script without shebang", `{"op":"exec","argv":["./script"],"cwd":"%R","env":{}}`)
				r.do("missing cwd", `{"op":"exec","argv":["true"],"cwd":"%R/missing","env":{}}`)
				r.do("cwd is a file", `{"op":"exec","argv":["true"],"cwd":"%R/file","env":{}}`)
				r.do("empty program", `{"op":"exec","argv":[""],"cwd":"%R","env":{}}`)
				r.do("directory as program", `{"op":"exec","argv":["%R"],"cwd":"%R","env":{}}`)
				r.do("inherited PATH replaced", `{"op":"exec","argv":["sh","-c","true"],"cwd":"%R","env":{"PATH":"/nonexistent"}}`)
				r.do("empty PATH entry", `{"op":"exec","argv":["script"],"cwd":"%R","env":{"PATH":":/nonexistent"}}`)
				r.do("no PATH", `{"op":"exec","argv":["sh","-c","printf ok"],"cwd":"%R","env":{},"inheritEnv":false}`)
				r.do("empty argv", `{"op":"exec","argv":[],"cwd":"%R","env":{}}`)
				r.do("nothing to run", `{"op":"exec","cwd":"%R","env":{}}`)
				r.do("no cwd", `{"op":"exec","argv":["true"]}`)
				r.do("timeout", `{"op":"exec","argv":["sleep","5"],"cwd":"%R","env":{},"timeoutMs":200}`)
				r.do("spill", `{"op":"exec","argv":["sh","-c","printf 'hello\\nworld\\n'"],"cwd":"%R","env":{},"spill":{"afterBytes":5,"afterLines":100}}`)
				r.do("no spill", `{"op":"exec","argv":["sh","-c","printf 'hello'"],"cwd":"%R","env":{},"spill":{"afterBytes":5,"afterLines":1}}`)
				// cat writes the file with one write, so the output arrives in one read:
				// how reads split the output decides the events.
				r.do("window", `{"op":"exec","argv":["cat","lines"],"cwd":"%R","env":{},"window":{"maxBytes":1000,"maxLines":2,"minIntervalMs":0,"bytesPerSecond":1000000}}`)
				r.do("incomplete window", `{"op":"exec","argv":["sh","-c","printf hi"],"cwd":"%R","env":{},"window":{"maxBytes":10}}`)
				id := r.start(strings.ReplaceAll(`{"op":"exec","argv":["sleep","5"],"cwd":"%R","env":{}}`, "%R", jsonEscape(r.root)), nil)
				r.send(frame.Cancel, id, []byte(`{}`), nil)
				r.keep("aborted", r.wait(id, finished))
				id = r.start(strings.ReplaceAll(`{"op":"exec","argv":["sleep","5"],"cwd":"%R","env":{}}`, "%R", jsonEscape(r.root)), nil)
				r.send(frame.Cancel, id, []byte(`{"mode":"kill"}`), nil)
				r.keep("killed", r.wait(id, finished))
			},
		},
		{
			name: "pty",
			skip: unixOnly,
			run: func(r *recorder) {
				r.do("raw output and exit code", `{"op":"pty","argv":["sh","-c","printf 'a\\377b'; exit 3"],"cwd":"%R","env":{},"cols":80,"rows":24}`)
				r.do("size", `{"op":"pty","argv":["stty","size"],"cwd":"%R","env":{},"cols":91,"rows":17}`)
				// ^C reaches the program only through its controlling terminal, as
				// SIGINT to the terminal's foreground process group: 128 + 2.
				id := r.start(strings.ReplaceAll(`{"op":"pty","argv":["sleep","5"],"cwd":"%R","env":{},"cols":80,"rows":24}`, "%R", jsonEscape(r.root)), nil)
				r.keep("interrupt started", r.wait(id, func(f []frame.Frame) bool { return len(f) > 0 }))
				r.do("interrupt", `{"op":"ptyWrite","target":`+strconv.Itoa(int(id))+`}`, 3)
				r.keep("interrupted", r.wait(id, finished)[1:])
				r.do("missing cwd", `{"op":"pty","argv":["true"],"cwd":"%R/missing","env":{},"cols":80,"rows":24}`)
				r.do("empty argv", `{"op":"pty","argv":[],"cwd":"%R","env":{},"cols":80,"rows":24}`)
				r.do("bad argv", `{"op":"pty","argv":"sh","cwd":"%R","env":{},"cols":80,"rows":24}`)
				r.do("no cwd", `{"op":"pty","argv":["true"],"env":{},"cols":80,"rows":24}`)
				r.do("zero cols", `{"op":"pty","argv":["true"],"cwd":"%R","env":{},"cols":0,"rows":24}`)
				r.do("too many rows", `{"op":"pty","argv":["true"],"cwd":"%R","env":{},"cols":80,"rows":4097}`)
				r.do("no such program", `{"op":"pty","argv":["no-such-program-pi-env"],"cwd":"%R","env":{},"cols":80,"rows":24}`)
				r.do("write to nothing", `{"op":"ptyWrite","target":999}`, []byte("x")...)
				r.do("resize nothing", `{"op":"ptyResize","target":999,"cols":1,"rows":1}`)
				r.do("foreground nothing", `{"op":"ptyForeground","target":999}`)
				r.do("write without target", `{"op":"ptyWrite"}`)
			},
		},
		{
			// Children get the default disposition of every signal and an empty
			// signal mask, also when the daemon was started ignoring or blocking
			// signals (libuv resets them all).
			name: "signals",
			launch: "for s in (signal.SIGHUP, signal.SIGINT, signal.SIGTERM, signal.SIGUSR1, signal.SIGTSTP, signal.SIGTTIN, signal.SIGTTOU):\n" +
				"    signal.signal(s, signal.SIG_IGN)\n" +
				"signal.pthread_sigmask(signal.SIG_BLOCK, {signal.SIGUSR2, signal.SIGWINCH, signal.SIGALRM})",
			skip: func() string {
				if reason := unixOnly(); reason != "" {
					return reason
				}
				if _, err := os.Stat("/usr/bin/python3"); err != nil {
					return "needs /usr/bin/python3 to start the daemon and report a child's dispositions"
				}
				return ""
			},
			run: func(r *recorder) {
				r.do("ignored in child", `{"op":"exec","argv":["/usr/bin/python3","-I","-c","import signal; print(sorted(s.name for s in signal.Signals if signal.getsignal(s) == signal.SIG_IGN and s not in (signal.SIGPIPE, signal.SIGXFSZ)), sorted(s.name for s in signal.pthread_sigmask(signal.SIG_BLOCK, [])))"],"cwd":"%R","env":{}}`)
			},
		},
		{
			// Rust's Command passes the environment it captured, sorted by name,
			// once a variable was set, and the daemon's own (in its order)
			// otherwise; values that are not strings set nothing. env(1) prints
			// the environment in order; the payload is reduced to whether it is
			// sorted and whether ZZ is set, since the rest is the machine's.
			name:  "environment order",
			skip:  unixOnly,
			clean: []func(string) string{summarizeEnvironment},
			run: func(r *recorder) {
				// The harness appends HOME and the git variables to the daemon's
				// environment, so it is not in name order to begin with.
				r.do("inherited", `{"op":"exec","argv":["/usr/bin/env"],"cwd":"%R","env":{}}`)
				r.do("inherited, a variable set", `{"op":"exec","argv":["/usr/bin/env"],"cwd":"%R","env":{"ZZ":"1"}}`)
				r.do("inherited, only a non-string", `{"op":"exec","argv":["/usr/bin/env"],"cwd":"%R","env":{"ZZ":1}}`)
				r.do("inherited, null env", `{"op":"exec","argv":["/usr/bin/env"],"cwd":"%R","env":null}`)
				r.do("cleared", `{"op":"exec","argv":["/usr/bin/env"],"cwd":"%R","env":{"B":"2","ZZ":"3","A":"1"},"inheritEnv":false}`)
			},
		},
		{
			name: "watch",
			skip: unixOnly,
			run: func(r *recorder) {
				writeFixture(t(r), r.root, map[string]string{"dir/a": "", "dir/b/c": ""})
				r.do("no targets", `{"op":"watch"}`)
				r.do("target without path", `{"op":"watch","targets":[{}]}`)
				r.do("too many directories", `{"op":"watch","targets":[{"path":"%R/dir","recursive":true}],"maxDirectories":1}`)
				r.do("zero directories", `{"op":"watch","targets":[{"path":"%R/dir"}],"maxDirectories":0}`)
				for _, mode := range []string{"native", "polling"} {
					id := r.start(strings.ReplaceAll(`{"op":"watch","targets":[{"path":"%R/dir","recursive":true,"exclude":{"hidden":true,"names":[]}}],"mode":"`+mode+`","pollIntervalMs":50}`, "%R", jsonEscape(r.root)), nil)
					r.keep(mode+" ready", r.wait(id, func(f []frame.Frame) bool { return len(f) > 0 }))
					writeFixture(t(r), r.root, map[string]string{"dir/b/new": "x"})
					frames := r.wait(id, func(f []frame.Frame) bool { return len(f) > 1 })
					r.keep(mode+" change", frames[1:2])
					r.send(frame.Cancel, id, nil, nil)
					all := r.wait(id, finished)
					r.keep(mode+" closed", all[len(all)-1:])
					os.Remove(filepath.Join(r.root, "dir", "b", "new"))
				}
			},
		},
	}
}

// notUTF8Launch starts the daemon with HOME, TMPDIR and SHELL not UTF-8
// (TMP is the first temporary directory variable that is) and, where file
// names may be any bytes, in a working directory whose name is not UTF-8.
const notUTF8Launch = `root = os.path.dirname(os.environb[b'HOME'])
os.environb[b'HOME'] = b'/h\xffome'
os.environb[b'TMPDIR'] = b'/t\xffmp'
os.environb[b'TMP'] = b'/second'
os.environb[b'SHELL'] = root + b'/s\xffh'
try:
    with open(root + b'/s\xffh', 'wb') as script:
        script.write(b'#!/bin/sh\necho not the login shell\n')
    os.chmod(root + b'/s\xffh', 0o755)
    os.mkdir(root + b'/c\xffwd')
    os.chdir(root + b'/c\xffwd')
except OSError:
    pass`

// foreignCwd is a working directory outside the scenario's directory, the
// test's own.
var foreignCwd = regexp.MustCompile(`"cwd":"[^<"][^"]*"`)

func muslOnly() string {
	if runtime.GOOS != "linux" || onAndroid {
		return "musl's realpath(3) and execvp(3): the Rust daemon's Linux builds are static musl"
	}
	return ""
}

func needsPython() string {
	if reason := unixOnly(); reason != "" {
		return reason
	}
	if _, err := os.Stat("/usr/bin/python3"); err != nil {
		return "needs /usr/bin/python3 to start the daemon"
	}
	return ""
}

// watchThroughLinks watches a tree whose links lead out of it: what changes
// behind a link is not a change of the tree (FSEvents and inotify do not
// follow links), while the links themselves coming and going are. Steps are a
// second apart, so each reports on its own.
func watchThroughLinks(r *recorder) {
	t := r.t
	writeFixture(t, r.root, map[string]string{"P/plain": "x", "Q/file": "x"})
	symlink(t, filepath.Join(r.root, "Q"), filepath.Join(r.root, "P", "dlink"))
	symlink(t, filepath.Join(r.root, "Q", "file"), filepath.Join(r.root, "P", "flink"))
	appendTo := func(path string) {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		f.WriteString("y")
		f.Close()
	}
	// Outside every target, below an ancestor that is a link on macOS (/tmp
	// is private/tmp there).
	missing := "/tmp/pi-env-oracle-missing-" + filepath.Base(r.root)
	elsewhere := "/tmp/pi-env-oracle-elsewhere-" + filepath.Base(r.root)
	// FSEvents may still deliver the fixture's creation after the watch
	// starts: what arrives before the first step is not kept.
	time.Sleep(time.Second)
	id := r.start(strings.ReplaceAll(`{"op":"watch","targets":[{"path":"%R/P","recursive":true},{"path":"`+missing+`"}],"mode":"native"}`, "%R", jsonEscape(r.root)), nil)
	r.wait(id, func(f []frame.Frame) bool { return len(f) > 0 })
	step := func(change func()) {
		change()
		time.Sleep(time.Second)
	}
	time.Sleep(time.Second)
	settled := len(r.wait(id, func([]frame.Frame) bool { return true }))
	step(func() { writeFixture(t, r.root, map[string]string{"Q/new": ""}) })
	step(func() { appendTo(filepath.Join(r.root, "Q", "file")) })
	step(func() { os.Remove(filepath.Join(r.root, "Q", "new")) })
	step(func() { os.WriteFile(elsewhere, nil, 0o644) })
	step(func() { os.Remove(elsewhere) })
	step(func() { symlink(t, filepath.Join(r.root, "Q"), filepath.Join(r.root, "P", "l2")) })
	step(func() { os.Remove(filepath.Join(r.root, "P", "l2")) })
	step(func() { appendTo(filepath.Join(r.root, "P", "plain")) })
	step(func() { os.Remove(filepath.Join(r.root, "Q", "file")) })
	step(func() { os.Rename(filepath.Join(r.root, "P", "flink"), filepath.Join(r.root, "P", "flink2")) })
	r.send(frame.Cancel, id, nil, nil)
	r.keep("watch", r.wait(id, finished)[settled:])
}

// summarizeEnvironment reduces env(1) output to whether the variables are in
// name order and whether ZZ is set.
func summarizeEnvironment(text string) string {
	if !strings.Contains(text, "HOME=") && !strings.Contains(text, "ZZ=") {
		return text
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		name, _, _ := strings.Cut(line, "=")
		names = append(names, name)
	}
	return "sorted=" + strconv.FormatBool(slices.IsSorted(names)) + " ZZ=" + strconv.FormatBool(slices.Contains(names, "ZZ"))
}

// gitignoreFixture is a work tree whose ignore files exercise the gitignore
// glob syntax, precedence between files, and work tree boundaries.
var gitignoreFixture = map[string]string{
	"home/":                  "",
	"gitconfig":              "[core]\n\texcludesFile = \"" + "GLOBAL" + "\"\n",
	"global-ignore":          "global.txt\n/anchored-global\n",
	"repo/.git/HEAD":         "ref: refs/heads/main\n",
	"repo/.git/info/exclude": "excluded.md\n!keep-excluded.md\n",
	"repo/.gitignore":        "\xef\xbb\xbf# comment\n*.log\n!keep.log\n/build/\nout/\ndocs/**/*.tmp\n**/cache\na?c.txt\n[bc]at.md\n[!x]y.z\n\\#hash\n\\!bang\ntrailing\\ \nspaces   \nfoo/**\nsub/inner.txt\n*.é\n[a-é]q\n{x,y}.alt\n[z\nkeep-excluded.md\nmid/*/deep\ndironly/\n\\[lit]\nre\r\n",
	"repo/a.log":             "", "repo/keep.log": "", "repo/build/x": "", "repo/sub/build/y": "", "repo/out/z": "", "repo/sub/out/w": "",
	"repo/docs/a/b.tmp": "", "repo/docs/c.tmp": "", "repo/docs/a/d.md": "", "repo/x/cache/f": "", "repo/cache": "",
	"repo/abc.txt": "", "repo/abbc.txt": "", "repo/bat.md": "", "repo/cat.md": "", "repo/dat.md": "", "repo/ay.z": "", "repo/xy.z": "",
	"repo/#hash": "", "repo/!bang": "", "repo/trailing ": "", "repo/spaces": "", "repo/foo/in": "", "repo/foo/deep/in": "",
	"repo/sub/inner.txt": "", "repo/inner.txt": "", "repo/file.é": "", "repo/bq": "", "repo/éq": "", "repo/x.alt": "", "repo/{x,y}.alt": "",
	"repo/[z": "", "repo/excluded.md": "", "repo/keep-excluded.md": "", "repo/global.txt": "", "repo/anchored-global": "", "repo/sub/anchored-global": "",
	"repo/mid/a/deep": "", "repo/mid/a/b/deep": "", "repo/dironly": "", "repo/x/dironly/f": "", "repo/[lit]": "", "repo/l": "", "repo/re": "",
	"repo/sub/.gitignore":    "!important.log\n*.txt\n!keep.txt\n",
	"repo/sub/important.log": "", "repo/sub/other.log": "", "repo/sub/a.txt": "", "repo/sub/keep.txt": "", "repo/sub/.dot": "",
	"repo/.hidden/visible":   "",
	"repo/nested/.git/HEAD":  "ref: refs/heads/main\n",
	"repo/nested/.gitignore": "*.md\n",
	"repo/nested/a.log":      "", "repo/nested/b.md": "", "repo/nested/c.txt": "",
	"plain/.gitignore": "*.log\n",
	"plain/a.log":      "", "plain/b.txt": "",
	"worktree/.git":       "gitdir: GITDIR\n",
	"worktree/.gitignore": "*.tmp\n",
	"worktree/a.tmp":      "", "worktree/b.md": "", "worktree/c.wt": "",
	"gitdir/commondir":    "../common\n",
	"common/info/exclude": "*.wt\n",
}
