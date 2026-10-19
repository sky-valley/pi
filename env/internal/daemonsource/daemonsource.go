// Package daemonsource builds the pi-env daemons the env package embeds
// (env/bin) and fingerprints the source they are built from, for the
// generator (internal/gendaemons) and for the test that fails when the
// embedded daemons are stale.
//
// Builds are reproducible: -trimpath, no VCS stamp, no build id, cgo off, and
// the env module's own go.mod (GOWORK=off), so the same source and toolchain
// give the same bytes, and the SHA-256 that names a deployed daemon is
// stable.
package daemonsource

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Target is one system pi-env ships a daemon for, in Node's names
// (upstream's npm bin/ layout) and Go's.
type Target struct {
	Platform, Arch string
	GOOS, GOARCH   string
	// Tags are build tags: android/amd64 is a linux build with the Android
	// behaviors, because Go links GOOS=android only with cgo there.
	Tags string
}

// Name is the daemon's name, pi-env-<platform>-<arch>.
func (t Target) Name() string { return "pi-env-" + t.Platform + "-" + t.Arch }

// Targets are the eight daemons upstream's npm package ships.
var Targets = []Target{
	{"darwin", "arm64", "darwin", "arm64", ""},
	{"darwin", "x64", "darwin", "amd64", ""},
	{"linux", "arm64", "linux", "arm64", ""},
	{"linux", "x64", "linux", "amd64", ""},
	{"android", "arm64", "android", "arm64", ""},
	{"android", "x64", "linux", "amd64", "pienv_android"},
	{"windows", "arm64", "windows", "arm64", ""},
	{"windows", "x64", "windows", "amd64", ""},
}

// buildFlags are the flags of every daemon build.
var buildFlags = []string{"-trimpath", "-buildvcs=false", "-ldflags=-s -w -buildid="}

// Recipe is how the daemons are built, part of the source fingerprint.
var Recipe = "GOWORK=off CGO_ENABLED=0 go build " + strings.Join(buildFlags, " ") + " ./cmd/pi-env"

// Manifest is env/bin/manifest.json: what the embedded daemons were built
// from and what they are.
type Manifest struct {
	// Go is the toolchain that built them (go env GOVERSION).
	Go string `json:"go"`
	// Source is Digest of the env module when they were built.
	Source  string            `json:"source"`
	Daemons map[string]Daemon `json:"daemons"`
}

// Daemon is one embedded daemon: the SHA-256 and size of the binary, before
// compression.
type Daemon struct {
	SHA256 string `json:"sha256"`
	Size   int    `json:"size"`
}

// goCommand runs the go command in the env module without a workspace.
func goCommand(moduleDir string, environment []string, args ...string) *exec.Cmd {
	cmd := exec.Command("go", args...)
	cmd.Dir = moduleDir
	cmd.Env = append(os.Environ(), append([]string{"GOWORK=off", "CGO_ENABLED=0", "GOFLAGS="}, environment...)...)
	return cmd
}

// Build builds the daemon for t into out.
func Build(moduleDir string, t Target, out string) error {
	args := append([]string{"build"}, buildFlags...)
	if t.Tags != "" {
		args = append(args, "-tags", t.Tags)
	}
	args = append(args, "-o", out, "./cmd/pi-env")
	cmd := goCommand(moduleDir, []string{"GOOS=" + t.GOOS, "GOARCH=" + t.GOARCH}, args...)
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stderr, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("building %s: %v\n%s", t.Name(), err, stderr.String())
	}
	return nil
}

// GoVersion is the toolchain the go command uses.
func GoVersion(moduleDir string) (string, error) {
	out, err := goCommand(moduleDir, nil, "env", "GOVERSION").Output()
	if err != nil {
		return "", fmt.Errorf("go env GOVERSION: %v", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Digest fingerprints what the daemons are built from: every non-test Go
// file (for every system) of the env module's packages in the daemon's
// graph, the versions of the other modules in it, and Recipe. The toolchain
// is not part of it: another Go version builds other bytes from the same
// source, and the manifest records which one built the embedded daemons.
func Digest(moduleDir string) (string, error) {
	root, err := goCommand(moduleDir, nil, "list", "-m", "-f", "{{.Dir}}").Output()
	if err != nil {
		return "", fmt.Errorf("go list -m: %v", err)
	}
	moduleDir = strings.TrimSpace(string(root))
	sum := sha256.New()
	fmt.Fprintf(sum, "recipe %s\n", Recipe)
	modules := map[string]bool{}
	dirs := map[string]bool{}
	for _, t := range Targets {
		args := []string{"list", "-deps", "-f", "{{if not .Standard}}{{.Dir}}|{{with .Module}}{{.Path}}|{{.Version}}{{end}}{{end}}"}
		if t.Tags != "" {
			args = append(args, "-tags", t.Tags)
		}
		cmd := goCommand(moduleDir, []string{"GOOS=" + t.GOOS, "GOARCH=" + t.GOARCH}, append(args, "./cmd/pi-env")...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("go list -deps ./cmd/pi-env for %s: %v\n%s", t.Name(), err, stderr.String())
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			parts := strings.Split(line, "|")
			if len(parts) != 3 {
				continue
			}
			if parts[1] == "github.com/sky-valley/pi/env" {
				dirs[parts[0]] = true
			} else {
				modules[parts[1]+"@"+parts[2]] = true
			}
		}
	}
	for _, module := range sortedKeys(modules) {
		fmt.Fprintf(sum, "module %s\n", module)
	}
	for _, dir := range sortedKeys(dirs) {
		rel, err := filepath.Rel(moduleDir, dir)
		if err != nil {
			return "", err
		}
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			return "", err
		}
		sort.Strings(files)
		for _, file := range files {
			if strings.HasSuffix(file, "_test.go") {
				continue
			}
			content, err := os.ReadFile(file)
			if err != nil {
				return "", err
			}
			fileSum := sha256.Sum256(content)
			fmt.Fprintf(sum, "file %s %x\n", filepath.ToSlash(filepath.Join(rel, filepath.Base(file))), fileSum)
		}
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
