package env

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

// The daemons for every supported remote system, built from ./cmd/pi-env and
// gzipped, with their manifest (internal/daemonsource).
//
//go:generate go run ./internal/gendaemons
//go:embed bin/*.gz bin/manifest.json
var embeddedDaemons embed.FS

var (
	unpackMu sync.Mutex
	unpacked = map[string][]byte{}

	// localDaemons are the paths LocalDaemon returned. A Connection starting
	// one verifies it first, and writes it again if it is missing or
	// changed: the user's cache directory may be cleaned (macOS purges
	// ~/Library/Caches) while a connection lives.
	localDaemons sync.Map
)

type daemonManifest struct {
	Daemons map[string]struct {
		SHA256 string `json:"sha256"`
		Size   int    `json:"size"`
	} `json:"daemons"`
}

// PackagedDaemon is the daemon embedded for a remote system (its Platform and
// Arch), uncompressed and checked against the manifest's SHA-256.
func PackagedDaemon(remote RemotePlatform) ([]byte, error) {
	name := "pi-env-" + remote.Platform + "-" + remote.Arch
	unpackMu.Lock()
	defer unpackMu.Unlock()
	if binary, ok := unpacked[name]; ok {
		return binary, nil
	}
	var manifest daemonManifest
	data, err := embeddedDaemons.ReadFile("bin/manifest.json")
	if err == nil {
		err = json.Unmarshal(data, &manifest)
	}
	if err != nil {
		return nil, fmt.Errorf("reading the embedded daemons' manifest: %w; run go generate ./... in env", err)
	}
	expected, ok := manifest.Daemons[name]
	if !ok {
		return nil, fmt.Errorf("No pi-env daemon for %s-%s", remote.Platform, remote.Arch)
	}
	compressed, err := embeddedDaemons.ReadFile("bin/" + name + ".gz")
	if err != nil {
		return nil, fmt.Errorf("the embedded %s is missing: %w; run go generate ./... in env", name, err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("the embedded %s is corrupt: %w; run go generate ./... in env", name, err)
	}
	binary, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("the embedded %s is corrupt: %w; run go generate ./... in env", name, err)
	}
	if sum := sha256.Sum256(binary); hex.EncodeToString(sum[:]) != expected.SHA256 {
		return nil, fmt.Errorf("the embedded %s does not match its manifest; run go generate ./... in env", name)
	}
	unpacked[name] = binary
	return binary, nil
}

// localPlatform is this machine as a RemotePlatform.
func localPlatform() (RemotePlatform, error) {
	platform := runtime.GOOS
	switch platform {
	case "linux", "android", "darwin", "windows":
	default:
		return RemotePlatform{}, fmt.Errorf("No pi-env daemon for %s", platform)
	}
	// Node's names for Go's architectures, which normalizeArch reads.
	arch := map[string]string{"amd64": "x64", "386": "ia32"}[runtime.GOARCH]
	if arch == "" {
		arch = runtime.GOARCH
	}
	arch, err := normalizeArch(arch)
	if err != nil {
		return RemotePlatform{}, err
	}
	return RemotePlatform{Platform: platform, Arch: arch}, nil
}

// LocalDaemon is the path of the daemon for the machine this program runs
// on, e.g. for terminals of local sessions and LocalExecutionEnv's
// ListFiles: NewConnection(ConnectionOptions{Command: []string{path}}). The
// embedded daemon is written to the user's cache directory, named by its
// SHA-256 like a deployed one, and verified before every use: each call, and
// each start of a Connection running it, writes it again if it is missing or
// changed.
func LocalDaemon() (string, error) {
	binary, err := localDaemonBinary()
	if err != nil {
		return "", err
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("finding a directory for the pi-env daemon: %w; set HOME (or XDG_CACHE_HOME, LocalAppData on Windows)", err)
	}
	sum := sha256.Sum256(binary)
	name := "pi-env-" + hex.EncodeToString(sum[:])[:32]
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(cache, "pi-env", name)
	if err := writeLocalDaemon(path, binary); err != nil {
		return "", err
	}
	localDaemons.Store(path, true)
	return path, nil
}

// localDaemonBinary is the embedded daemon for this machine.
func localDaemonBinary() ([]byte, error) {
	remote, err := localPlatform()
	if err != nil {
		return nil, err
	}
	return PackagedDaemon(remote)
}

// verifyLocalDaemon writes the daemon at path again, before a Connection
// starts it, if path is one LocalDaemon returned and it is missing or
// changed.
func verifyLocalDaemon(path string) error {
	if _, ok := localDaemons.Load(path); !ok {
		return nil
	}
	binary, err := localDaemonBinary()
	if err != nil {
		return err
	}
	return writeLocalDaemon(path, binary)
}

// writeLocalDaemon writes binary to path (0700, through a temporary file)
// unless it is there already.
func writeLocalDaemon(path string, binary []byte) error {
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, binary) {
		return nil
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("reading %s: %w; remove it to have it written again", path, err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s for the pi-env daemon: %w", dir, err)
	}
	temporary, err := os.CreateTemp(dir, ".pi-env-*.tmp")
	if err != nil {
		return fmt.Errorf("writing the pi-env daemon to %s: %w", dir, err)
	}
	_, err = temporary.Write(binary)
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Chmod(temporary.Name(), 0o700)
	}
	if err == nil {
		err = os.Rename(temporary.Name(), path)
	}
	if err != nil {
		os.Remove(temporary.Name())
		return fmt.Errorf("writing the pi-env daemon to %s: %w", path, err)
	}
	return nil
}
