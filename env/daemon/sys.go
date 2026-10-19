package daemon

import (
	"crypto/rand"
	"errors"
	"os"
	"runtime"
)

// What differs between operating systems lives in sys_*.go: error names,
// temporary directories, opening files, and starting and killing commands.
// Each implementation follows what Node (libuv) does on that system.

// shellConfig is how a string command runs: the shell, its arguments before
// the command, and whether the command goes to its stdin instead of its
// arguments (legacy WSL bash.exe).
type shellConfig struct {
	program        string
	args           []string
	commandOnStdin bool
}

// errNulData is how Rust's Command refuses a NUL byte in the program, an
// argument or the environment, before it starts anything.
var errNulData = errors.New("nul byte found in provided data")

// openMode is how Node opens files: open(path, "r"), writeFile ("w"),
// appendFile ("a"), and "r+" for truncate and fsync.
type openMode int

const (
	modeRead openMode = iota
	modeWrite
	modeAppend
	modeReadWrite
)

// pathExists reports whether a path exists, following symbolic links, like
// Node's access(path, F_OK).
func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// randomBytes is sixteen random bytes for temporary names.
func randomBytes() [16]byte {
	var b [16]byte
	rand.Read(b[:]) // never fails (crypto/rand)
	return b
}

// osName and archName are Rust's std::env::consts::OS and ARCH, which the
// client's nodePlatform maps to Node's names.
func osName() string {
	switch {
	case onAndroid:
		return "android"
	case runtime.GOOS == "darwin":
		return "macos"
	default:
		return runtime.GOOS
	}
}

func archName() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	case "386":
		return "x86"
	default:
		return runtime.GOARCH
	}
}
