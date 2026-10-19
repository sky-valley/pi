// Package policy holds the env module's invariant tests, as the root
// module's internal/policy does for the root: no source, nothing imports it,
// and `go test ./...` in env/ enforces the module's dependency budget.
package policy

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// daemonModules is the complete set of third-party modules the pi-env daemon
// (./cmd/pi-env and everything it imports) may reach, mapped to why each is
// here. The owner's 2026-10-19 ruling on porting pi-env admitted exactly
// these two; anything else is an owner CONSULT (docs/UPSTREAM.md E2) before
// it lands here. The daemon ships as a binary to every remote system, so its
// budget is tighter than the module's.
var daemonModules = map[string]string{
	"golang.org/x/sys":             "system calls the daemon makes as the Rust daemon makes them through libc and windows-sys (env/daemon/sys_*.go)",
	"github.com/fsnotify/fsnotify": "native file watching for the watch operation, where the Rust daemon uses the notify crate (env/daemon/watch.go)",
}

// rootModules are the modules the root module's own policy test admits; the
// rest of the env module (the client) may reach them through the root.
var rootModules = map[string]string{
	"golang.org/x/image": "reached through the root module (its internal/policy/deps_test.go governs it)",
	"golang.org/x/text":  "reached through the root module (its internal/policy/deps_test.go governs it)",
}

// ownModules are skipped by the allowlist checks; their packages are still
// checked for cgo. The root module counts as own: env requires it.
var ownModules = map[string]bool{
	"github.com/sky-valley/pi/env": true,
	"github.com/sky-valley/pi":     true,
}

type depPackage struct {
	ImportPath string
	Module     string
	CgoFiles   int
}

// listDeps enumerates every package the build of pattern reaches, with cgo
// forced on so a cgo-requiring dependency shows its cgo files (see the root
// module's internal/policy/deps_test.go for why).
func listDeps(t *testing.T, goos, pattern string) []depPackage {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH; cannot inspect the dependency graph")
	}
	gomod, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		t.Fatalf("locating go.mod: %v", err)
	}
	root := strings.TrimSpace(string(gomod))
	if root == "" || root == os.DevNull {
		t.Fatal("no go.mod found; this test must run inside the env module")
	}
	const format = `{{.ImportPath}}|{{with .Module}}{{.Path}}{{end}}|{{len .CgoFiles}}`
	cmd := exec.Command("go", "list", "-deps", "-f", format, pattern)
	cmd.Dir = filepath.Dir(root)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1", "GOOS="+goos)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps %s (GOOS=%s): %v", pattern, goos, err)
	}
	var pkgs []depPackage
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Split(line, "|")
		if len(fields) != 3 {
			t.Fatalf("unexpected go list output line %q", line)
		}
		n, err := strconv.Atoi(fields[2])
		if err != nil {
			t.Fatalf("parsing CgoFiles count in %q: %v", line, err)
		}
		pkgs = append(pkgs, depPackage{ImportPath: fields[0], Module: fields[1], CgoFiles: n})
	}
	return pkgs
}

// targets are the systems the daemon ships for; each pulls different files.
var targets = []string{"darwin", "linux", "android", "windows"}

// outside lists the modules pattern reaches that no allowlist admits.
func outside(t *testing.T, goos, pattern string, allowlists ...map[string]string) []string {
	seen := map[string]string{}
	for _, pkg := range listDeps(t, goos, pattern) {
		if pkg.Module == "" || ownModules[pkg.Module] {
			continue
		}
		admitted := false
		for _, allowlist := range allowlists {
			_, ok := allowlist[pkg.Module]
			admitted = admitted || ok
		}
		if !admitted {
			seen[pkg.Module] = pkg.ImportPath
		}
	}
	var mods []string
	for mod, via := range seen {
		mods = append(mods, mod+" (first reached via "+via+")")
	}
	sort.Strings(mods)
	return mods
}

func TestDaemonDependenciesAreAllowlisted(t *testing.T) {
	for _, goos := range targets {
		if mods := outside(t, goos, "./cmd/pi-env", daemonModules); len(mods) > 0 {
			t.Errorf("GOOS=%s: the daemon reaches modules outside its allowlist:\n\t%s\n\n"+
				"To resolve: the daemon may use the standard library, golang.org/x/sys and\n"+
				"github.com/fsnotify/fsnotify only (owner ruling, 2026-10-19). Implement the need\n"+
				"on those, or take the module to an owner CONSULT under E2 in docs/UPSTREAM.md\n"+
				"and add it to daemonModules with its reason. A root package that imports\n"+
				"golang.org/x/text (internal/jstext) cannot be imported by the daemon for the\n"+
				"same reason.", goos, strings.Join(mods, "\n\t"))
		}
	}
}

func TestBuildDependenciesAreAllowlisted(t *testing.T) {
	for _, goos := range targets {
		if mods := outside(t, goos, "./...", daemonModules, rootModules); len(mods) > 0 {
			t.Errorf("GOOS=%s: the env module reaches modules outside the allowlist:\n\t%s\n\n"+
				"To resolve: adding a third-party module is an owner CONSULT under E2 in\n"+
				"docs/UPSTREAM.md; record the ruling, then add it here with its reason. If it\n"+
				"arrived through the root module, the root's allowlist governs it: mirror that\n"+
				"entry in rootModules.", goos, strings.Join(mods, "\n\t"))
		}
	}
}

func TestNoCgoInBuild(t *testing.T) {
	for _, goos := range targets {
		var offenders []string
		for _, pkg := range listDeps(t, goos, "./...") {
			if pkg.Module != "" && pkg.CgoFiles > 0 {
				offenders = append(offenders, pkg.ImportPath+" ("+strconv.Itoa(pkg.CgoFiles)+" cgo files, module "+pkg.Module+")")
			}
		}
		if len(offenders) == 0 {
			continue
		}
		sort.Strings(offenders)
		t.Errorf("GOOS=%s: cgo is present in the build graph:\n\t%s\n\n"+
			"To resolve: the daemon is cross-compiled with CGO_ENABLED=0 for every system it\n"+
			"ships to; replace the cgo dependency with system calls through golang.org/x/sys.",
			goos, strings.Join(offenders, "\n\t"))
	}
}
