package env

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/sky-valley/pi/env/internal/daemonsource"
)

func readManifest(t *testing.T) daemonsource.Manifest {
	t.Helper()
	data, err := embeddedDaemons.ReadFile("bin/manifest.json")
	mustDo(t, err)
	var manifest daemonsource.Manifest
	mustDo(t, json.Unmarshal(data, &manifest))
	return manifest
}

func requireGo(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go command to inspect the daemon's source")
	}
}

// TestEmbeddedDaemonsAreCurrent fails once the daemon's source changes
// without the embedded daemons being built again.
func TestEmbeddedDaemonsAreCurrent(t *testing.T) {
	requireGo(t)
	digest, err := daemonsource.Digest(".")
	mustDo(t, err)
	if manifest := readManifest(t); manifest.Source != digest {
		t.Fatalf("the embedded daemons were built from other source (%.12s, now %.12s): run go generate ./... in env", manifest.Source, digest)
	}
}

// TestEmbeddedDaemonsRebuildIdentically rebuilds the daemon for this machine
// and compares it with the embedded one; builds are reproducible, so a
// difference means the embedded daemons do not come from this source. It
// needs the toolchain that built them.
func TestEmbeddedDaemonsRebuildIdentically(t *testing.T) {
	requireGo(t)
	manifest := readManifest(t)
	version, err := daemonsource.GoVersion(".")
	mustDo(t, err)
	if version != manifest.Go {
		t.Skipf("the embedded daemons were built with %s, this is %s", manifest.Go, version)
	}
	local, err := localPlatform()
	mustDo(t, err)
	var target daemonsource.Target
	for _, candidate := range daemonsource.Targets {
		if candidate.Platform == local.Platform && candidate.Arch == local.Arch {
			target = candidate
		}
	}
	out := filepath.Join(t.TempDir(), target.Name())
	mustDo(t, daemonsource.Build(".", target, out))
	rebuilt, err := os.ReadFile(out)
	mustDo(t, err)
	embedded, err := PackagedDaemon(local)
	mustDo(t, err)
	if !bytes.Equal(rebuilt, embedded) {
		t.Fatalf("%s rebuilt from source differs from the embedded one: run go generate ./... in env", target.Name())
	}
}

// TestDaemonBuildsIgnoreTheCallersGoEnvironment: the embedded daemons must
// not depend on who runs go generate. A GOAMD64 or GOARM64 level, or a go env
// file (go env -w) with GOFLAGS or GOEXPERIMENT, must give the same source
// digest and the same bytes; a daemon built for x86-64-v3 would not run on
// older machines.
func TestDaemonBuildsIgnoreTheCallersGoEnvironment(t *testing.T) {
	requireGo(t)
	manifest := readManifest(t)
	version, err := daemonsource.GoVersion(".")
	mustDo(t, err)
	if version != manifest.Go {
		t.Skipf("the embedded daemons were built with %s, this is %s", manifest.Go, version)
	}
	goenv := filepath.Join(t.TempDir(), "env")
	mustDo(t, os.WriteFile(goenv, []byte("GOFLAGS=-gcflags=all=-N\nGOEXPERIMENT=nogreenteagc\n"), 0o600))
	t.Setenv("GOENV", goenv)
	t.Setenv("GOAMD64", "v3")
	t.Setenv("GOARM64", "v9.0")
	t.Setenv("GOFLAGS", "")
	t.Setenv("GOEXPERIMENT", "")
	digest, err := daemonsource.Digest(".")
	mustDo(t, err)
	if digest != manifest.Source {
		t.Errorf("source digest %.12s with the caller's go environment, manifest %.12s", digest, manifest.Source)
	}
	for _, target := range daemonsource.Targets {
		if target.Platform != "linux" {
			continue
		}
		out := filepath.Join(t.TempDir(), target.Name())
		mustDo(t, daemonsource.Build(".", target, out))
		binary, err := os.ReadFile(out)
		mustDo(t, err)
		if sum := sha256.Sum256(binary); hex.EncodeToString(sum[:]) != manifest.Daemons[target.Name()].SHA256 {
			t.Errorf("%s built with the caller's go environment differs from the embedded one", target.Name())
		}
	}
}

func TestEveryEmbeddedDaemonUnpacksToItsManifestEntry(t *testing.T) {
	manifest := readManifest(t)
	if len(manifest.Daemons) != len(daemonsource.Targets) {
		t.Fatalf("%d daemons in the manifest, %d targets", len(manifest.Daemons), len(daemonsource.Targets))
	}
	for _, target := range daemonsource.Targets {
		binary, err := PackagedDaemon(RemotePlatform{Platform: target.Platform, Arch: target.Arch})
		mustDo(t, err)
		sum := sha256.Sum256(binary)
		if entry := manifest.Daemons[target.Name()]; entry.SHA256 != hex.EncodeToString(sum[:]) || entry.Size != len(binary) {
			t.Fatalf("%s: %d bytes, manifest %+v", target.Name(), len(binary), entry)
		}
	}
	if _, err := PackagedDaemon(RemotePlatform{Platform: "plan9", Arch: "x64"}); err == nil || err.Error() != "No pi-env daemon for plan9-x64" {
		t.Fatalf("an unknown system: %v", err)
	}
}

func TestLocalDaemonRunsTheEmbeddedDaemon(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("LocalAppData", os.Getenv("HOME"))
	path, err := LocalDaemon()
	mustDo(t, err)
	// Written again when it was changed.
	mustDo(t, os.WriteFile(path, []byte("tampered"), 0o700))
	if again, err := LocalDaemon(); err != nil || again != path {
		t.Fatalf("LocalDaemon again: %s %v", again, err)
	}
	connection := NewConnection(ConnectionOptions{Command: []string{path}})
	defer connection.Close()
	info, err := connection.Info(background())
	mustDo(t, err)
	if info.Protocol != 1 || !info.hasFeature("listFiles") {
		t.Fatalf("hello %+v", info)
	}
}

// TestLocalDaemonSurvivesACacheCleanup: the user's cache directory may be
// cleaned (macOS purges ~/Library/Caches) while a connection to the local
// daemon lives; its next start writes the daemon again.
func TestLocalDaemonSurvivesACacheCleanup(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("LocalAppData", os.Getenv("HOME"))
	path, err := LocalDaemon()
	mustDo(t, err)
	connection := NewConnection(ConnectionOptions{Command: []string{path}})
	defer connection.Close()
	info, err := connection.Info(background())
	mustDo(t, err)
	mustDo(t, os.RemoveAll(filepath.Dir(path)))
	killPID(t, info.PID)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		again, err := connection.Info(background())
		if err == nil && again.PID != info.PID {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the daemon did not start again: %v", err)
		}
	}
}
