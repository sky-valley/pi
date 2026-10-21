package coding

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/sky-valley/pi/internal/jstext"
	"github.com/sky-valley/pi/internal/nodepath"
)

// Shell resolution, ported from pi's utils/shell.ts.

// CommandTransport is how a command reaches the shell: pi's
// ShellConfig.commandTransport, "argv" | "stdin".
type CommandTransport string

const (
	// CommandTransportArgv appends the command to the shell's args
	// (`bash -c <command>`).
	CommandTransportArgv CommandTransport = "argv"
	// CommandTransportStdin writes the command to the shell's stdin
	// (`bash -s`), for the legacy WSL launcher, which mangles `-c` quoting.
	CommandTransportStdin CommandTransport = "stdin"
)

// ShellConfig is pi's ShellConfig: the shell to spawn, its args, and how the
// command reaches it. An empty CommandTransport is pi's absent
// commandTransport, which is argv.
type ShellConfig struct {
	Shell            string
	Args             []string
	CommandTransport CommandTransport
}

// commandFromStdin is pi's `shellConfig.commandTransport === "stdin"`.
func (c ShellConfig) commandFromStdin() bool {
	return c.CommandTransport == CommandTransportStdin
}

// shellHost is what pi's shell.ts reads from the process it runs in: the
// platform, the environment, the filesystem existence check and the PATH
// search. The resolvers take it as a value so their Windows branches are
// testable on every host, as pi's tests stub process.platform, process.env,
// accessSync and spawnSync.
type shellHost struct {
	windows bool
	getenv  func(string) string
	// exists is pi's shellPathExists.
	exists func(string) bool
	// searchPath runs pi's PATH search command (`where` on Windows, `which`
	// elsewhere) for an executable and reports its stdout and whether it
	// exited 0.
	searchPath func(command, executable string) (stdout string, ok bool)
}

var localShellHost = shellHost{
	windows:    runtime.GOOS == "windows",
	getenv:     os.Getenv,
	exists:     shellPathExists,
	searchPath: spawnPathSearch,
}

// pathSearchTimeout is pi's spawnSync timeout for `where` / `which`.
const pathSearchTimeout = 5 * time.Second

// spawnPathSearch is pi's spawnSync(command, [executable], {timeout: 5000}):
// the search command runs from PATH as Node would spawn it, a relative PATH
// entry included (os/exec reports that as exec.ErrDot, which is cleared), and
// only a zero exit counts.
func spawnPathSearch(command, executable string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), pathSearchTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, command, executable)
	if errors.Is(cmd.Err, exec.ErrDot) {
		cmd.Err = nil
	}
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return string(out), true
}

// lineBreakRE is pi's /\r?\n/.
var lineBreakRE = regexp.MustCompile(`\r?\n`)

// windowsAccessPath is the path Node's fs bindings hand libuv on Windows:
// path.win32.toNamespacedPath of the given one, resolved against the working
// directory and in its long `\\?\` (or `\\?\UNC\`) form, which also lifts
// MAX_PATH. It is here, unconstrained, so it is testable on every host.
func windowsAccessPath(path string) string { return nodepath.Win32.ToNamespacedPath(path) }

// legacyWslBashRE matches the legacy Windows-bundled WSL launcher
// (C:\Windows\System32\bash.exe, or the WoW64 sysnative redirect) after path
// normalization, for when the Windows directory is unknown. The launcher
// mishandles `-c "<cmd>"` quoting, so commands go via stdin.
var legacyWslBashRE = regexp.MustCompile(`^[a-z]:\\windows\\(?:system32|sysnative)\\bash\.exe$`)

// driveAbsoluteRE is pi's /^[a-z]:[\\/]/i. JavaScript's non-Unicode `i`
// folds ASCII only, so the class is spelled out rather than given Go's (?i),
// which would also take the Kelvin sign for a k.
var driveAbsoluteRE = regexp.MustCompile(`^[a-zA-Z]:[\\/]`)

// isLegacyWslBashPath ports pi's isLegacyWslBashPath (shell.ts, upstream
// 61060ddf8): the launcher is System32\bash.exe or Sysnative\bash.exe under
// the actual Windows directory (%SystemRoot%), compared after path.win32
// normalization and JavaScript lowercasing. A SystemRoot that is not
// drive-absolute recognizes nothing; the conventional C:\Windows paths are
// the fallback only when SystemRoot is unset or empty.
func isLegacyWslBashPath(path, systemRoot string) bool {
	normalized := jstext.ToLower(nodepath.Win32.Normalize(path))
	if systemRoot != "" {
		if !driveAbsoluteRE.MatchString(systemRoot) {
			return false
		}
		for _, directory := range []string{"System32", "Sysnative"} {
			if normalized == jstext.ToLower(nodepath.Win32.Join(systemRoot, directory, "bash.exe")) {
				return true
			}
		}
		return false
	}
	return legacyWslBashRE.MatchString(normalized)
}

// bashShellConfig ports pi's getBashShellConfig: legacy WSL bash takes the
// command on stdin (`bash -s`); every other bash takes `bash -c <command>`
// with no commandTransport set.
func (h shellHost) bashShellConfig(shell string) ShellConfig {
	if isLegacyWslBashPath(shell, h.getenv("SystemRoot")) {
		return ShellConfig{Shell: shell, Args: []string{"-s"}, CommandTransport: CommandTransportStdin}
	}
	return ShellConfig{Shell: shell, Args: []string{"-c"}}
}

// findExecutableOnPath ports pi's findExecutableOnPath: it runs `where`
// (Windows) or `which` and takes the first line of the trimmed output, or ""
// when the search fails or prints nothing. On Windows pi re-checks the match
// with shellPathExists (`where` can print paths that do not exist);
// elsewhere it trusts `which` (Termux and special filesystems).
func (h shellHost) findExecutableOnPath(executable string) string {
	command := "which"
	if h.windows {
		command = "where"
	}
	stdout, ok := h.searchPath(command, executable)
	if !ok || stdout == "" {
		return ""
	}
	firstMatch := lineBreakRE.Split(jstext.Trim(stdout), 2)[0]
	if firstMatch == "" {
		return ""
	}
	if h.windows && !h.exists(firstMatch) {
		return ""
	}
	return firstMatch
}

// GetShellConfig ports pi's getShellConfig (shell.ts), root-exported upstream.
// pi never uses $SHELL or cmd: Unix resolves /bin/bash, then bash on PATH,
// then sh; Windows resolves Git Bash in known locations, then bash.exe on
// PATH, else errors. A non-empty customShellPath is pi's explicit shellPath:
// it wins over every platform default, and a missing one is an error, not a
// fallback.
func GetShellConfig(customShellPath string) (ShellConfig, error) {
	return localShellHost.shellConfig(customShellPath)
}

func (h shellHost) shellConfig(customShellPath string) (ShellConfig, error) {
	if customShellPath != "" {
		if h.exists(customShellPath) {
			return h.bashShellConfig(customShellPath), nil
		}
		return ShellConfig{}, errors.New("Custom shell path not found: " + customShellPath)
	}
	if h.windows {
		var paths []string
		if pf := h.getenv("ProgramFiles"); pf != "" {
			paths = append(paths, pf+`\Git\bin\bash.exe`)
		}
		if pf86 := h.getenv("ProgramFiles(x86)"); pf86 != "" {
			paths = append(paths, pf86+`\Git\bin\bash.exe`)
		}
		for _, p := range paths {
			if h.exists(p) {
				return h.bashShellConfig(p), nil
			}
		}
		if p := h.findExecutableOnPath("bash.exe"); p != "" {
			return h.bashShellConfig(p), nil
		}
		var b strings.Builder
		b.WriteString("No bash shell found. Options:\n")
		b.WriteString("  1. Install Git for Windows: https://git-scm.com/download/win\n")
		b.WriteString("  2. Add your bash to PATH (Cygwin, MSYS2, etc.)\n")
		b.WriteString("  3. Set shellPath in settings.json\n\n")
		b.WriteString("Searched Git Bash in:\n")
		for i, p := range paths {
			if i > 0 {
				b.WriteString("\n")
			}
			b.WriteString("  " + p)
		}
		return ShellConfig{}, errors.New(b.String())
	}

	// Unix: try /bin/bash, then bash on PATH, then fall back to sh.
	if h.exists("/bin/bash") {
		return h.bashShellConfig("/bin/bash"), nil
	}
	if p := h.findExecutableOnPath("bash"); p != "" {
		return h.bashShellConfig(p), nil
	}
	return ShellConfig{Shell: "sh", Args: []string{"-c"}}, nil
}

// powershellArgs ports pi's POWERSHELL_ARGS (shell.ts, upstream 80e62761f):
// no profile, no interactive prompts, and a process-scoped execution-policy
// bypass so the machine policy is left alone.
var powershellArgs = []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command"}

// GetPowerShellConfig ports pi's getPowerShellConfig, root-exported upstream:
// Windows only, preferring PowerShell 7 (pwsh) over Windows PowerShell. The
// order (upstream 61060ddf8) is pwsh.exe on PATH,
// %ProgramFiles%\PowerShell\7\pwsh.exe, powershell.exe on PATH, then
// %SystemRoot%\System32\WindowsPowerShell\v1.0\powershell.exe, the
// directories joined as path.win32 does. pi throws on both failures; the
// error strings are byte-exact, since the tool surfaces them to the model.
func GetPowerShellConfig() (ShellConfig, error) {
	return localShellHost.powerShellConfig()
}

func (h shellHost) powerShellConfig() (ShellConfig, error) {
	if !h.windows {
		return ShellConfig{}, errors.New("The powershell tool is only available on Windows.")
	}
	shell := h.findExecutableOnPath("pwsh.exe")
	// Installation directories may be on any drive and need not be on PATH.
	if programFiles := h.getenv("ProgramFiles"); shell == "" && programFiles != "" {
		if pwsh7 := nodepath.Win32.Join(programFiles, "PowerShell", "7", "pwsh.exe"); h.exists(pwsh7) {
			shell = pwsh7
		}
	}
	if shell == "" {
		shell = h.findExecutableOnPath("powershell.exe")
	}
	if systemRoot := h.getenv("SystemRoot"); shell == "" && systemRoot != "" {
		windowsPowerShell := nodepath.Win32.Join(systemRoot, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
		if h.exists(windowsPowerShell) {
			shell = windowsPowerShell
		}
	}
	if shell == "" {
		return ShellConfig{}, errors.New("No PowerShell executable found. Install PowerShell or add powershell.exe/pwsh.exe to PATH.")
	}
	return ShellConfig{Shell: shell, Args: slices.Clone(powershellArgs)}, nil
}
