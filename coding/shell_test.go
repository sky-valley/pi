package coding

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"testing"
)

// fakeShellHost stands in for pi's stubbed process.platform, process.env,
// accessSync and spawnSync: env holds the variables that are set, exists
// answers access(F_OK) (every path exists when it is nil), and onPath is what
// `where`/`which` print first for an executable.
func fakeShellHost(windows bool, env map[string]string, exists func(string) bool, onPath map[string]string) shellHost {
	if exists == nil {
		exists = func(string) bool { return true }
	}
	return shellHost{
		windows: windows,
		getenv:  func(key string) string { return env[key] },
		exists:  exists,
		lookPath: func(executable string) (string, error) {
			if p, ok := onPath[executable]; ok {
				return p, nil
			}
			return "", errors.New("executable file not found in %PATH%")
		},
	}
}

// existsOnly is an access(F_OK) that succeeds for exactly the given paths.
func existsOnly(paths ...string) func(string) bool {
	return func(p string) bool {
		for _, ok := range paths {
			if p == ok {
				return true
			}
		}
		return false
	}
}

const storeAliasesDir = `D:\Users\test\AppData\Local\Microsoft\WindowsApps`

// TestWindowsStoreShellAliases ports shell-path-exists.test.ts (upstream
// 28bea006b): runnable Store app-execution aliases pass access(F_OK), so they
// resolve from PATH and as an explicit shell path, while a PATH result that
// fails the check is still rejected.
func TestWindowsStoreShellAliases(t *testing.T) {
	for _, executable := range []string{"bash.exe", "pwsh.exe"} {
		t.Run("accepts "+executable+" found on PATH", func(t *testing.T) {
			alias := storeAliasesDir + `\` + executable
			h := fakeShellHost(true, nil, nil, map[string]string{executable: alias})
			var shell string
			var err error
			if executable == "pwsh.exe" {
				shell, _, _, err = h.powerShellConfig()
			} else {
				shell, _, _, err = h.shellConfig("")
			}
			if err != nil {
				t.Fatal(err)
			}
			if shell != alias {
				t.Fatalf("shell = %q, want %q", shell, alias)
			}
		})
	}

	t.Run("accepts an explicitly configured shell alias", func(t *testing.T) {
		alias := storeAliasesDir + `\bash.exe`
		shell, _, _, err := fakeShellHost(true, nil, nil, nil).shellConfig(alias)
		if err != nil {
			t.Fatal(err)
		}
		if shell != alias {
			t.Fatalf("shell = %q, want %q", shell, alias)
		}
	})

	t.Run("still rejects PATH results that fail the existence check", func(t *testing.T) {
		windowsPowerShell := `D:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`
		h := fakeShellHost(true, nil, existsOnly(windowsPowerShell), map[string]string{
			"pwsh.exe":       storeAliasesDir + `\pwsh.exe`,
			"powershell.exe": windowsPowerShell,
		})
		shell, _, _, err := h.powerShellConfig()
		if err != nil {
			t.Fatal(err)
		}
		if shell != windowsPowerShell {
			t.Fatalf("shell = %q, want %q", shell, windowsPowerShell)
		}
	})
}

// TestUnixShellExistenceChecks ports the Unix half of shell-path-exists.test.ts:
// the access(F_OK) check gates /bin/bash and an explicit shell path, and a
// failing /bin/bash falls back to bash on PATH.
func TestUnixShellExistenceChecks(t *testing.T) {
	t.Run("accepts /bin/bash when access(F_OK) succeeds", func(t *testing.T) {
		shell, _, _, err := fakeShellHost(false, nil, nil, nil).shellConfig("")
		if err != nil {
			t.Fatal(err)
		}
		if shell != "/bin/bash" {
			t.Fatalf("shell = %q, want /bin/bash", shell)
		}
	})

	t.Run("falls back to bash on PATH when /bin/bash fails access(F_OK)", func(t *testing.T) {
		h := fakeShellHost(false, nil, existsOnly(), map[string]string{"bash": "/usr/local/bin/bash"})
		shell, _, _, err := h.shellConfig("")
		if err != nil {
			t.Fatal(err)
		}
		if shell != "/usr/local/bin/bash" {
			t.Fatalf("shell = %q, want /usr/local/bin/bash", shell)
		}
	})

	t.Run("rejects an explicitly configured shell path that fails access(F_OK)", func(t *testing.T) {
		_, _, _, err := fakeShellHost(false, nil, existsOnly(), nil).shellConfig("/missing/bash")
		if err == nil {
			t.Fatal("want an error, got none")
		}
		if got, want := err.Error(), "Custom shell path not found: /missing/bash"; got != want {
			t.Fatalf("error\n got: %q\nwant: %q", got, want)
		}
	})
}

// TestShellPathExistsFollowsSymlinks pins access(F_OK) on the real filesystem:
// it follows symlinks, so a dangling one does not exist, and a directory does.
func TestShellPathExistsFollowsSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows access(F_OK) is GetFileAttributesW, which does not follow links")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "bash")
	if err := os.WriteFile(file, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(dir, "dangling")
	if err := os.Symlink(filepath.Join(dir, "missing"), dangling); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]bool{
		file:                           true,
		link:                           true,
		dir:                            true,
		dangling:                       false,
		filepath.Join(dir, "missing"):  false,
		"":                             false,
		filepath.Join(file, "nothing"): false,
	} {
		if got := shellPathExists(path); got != want {
			t.Errorf("shellPathExists(%q) = %v, want %v", path, got, want)
		}
	}
}

// TestLegacyWslBashUnderSystemRoot ports windows-shell-config.test.ts and the
// durable env-node.test.ts transport table (upstream 61060ddf8): the legacy
// WSL launcher is recognized under the actual Windows directory, a SystemRoot
// that is not drive-absolute recognizes nothing, and the conventional
// C:\Windows paths are the fallback only when SystemRoot is unset or empty.
func TestLegacyWslBashUnderSystemRoot(t *testing.T) {
	for _, tt := range []struct {
		systemRoot *string
		shell      string
		stdin      bool
	}{
		// windows-shell-config.test.ts
		{ptr(`D:\WinNT\`), `d:/WINNT/System32/BASH.EXE`, true},
		{ptr(`D:\WinNT`), `D:\WinNT\Sysnative\bash.exe`, true},
		{nil, `E:\Windows\System32\bash.exe`, true},
		{ptr(`D:\WinNT`), `D:\Git\bin\bash.exe`, false},
		{ptr(`D:\WinNT`), `E:\Windows\System32\bash.exe`, false},
		{ptr(`WinNT`), `C:\Windows\System32\bash.exe`, false},
		// durable env-node.test.ts
		{nil, `C:\Windows\System32\bash.exe`, true},
		{ptr(`D:\WinNT`), `D:\WinNT\System32\bash.exe`, true},
		{ptr(`D:\WinNT`), `D:\WinNT\Sysnative\bash.exe`, true},
		{ptr(``), `D:\WinNT\System32\bash.exe`, false},
		{ptr(`D:\WinNT`), `E:\Windows\System32\bash.exe`, false},
		// JavaScript's toLowerCase maps U+0130 to "i" + U+0307, not to "i",
		// so this is not the conventional launcher path to pi.
		{nil, "C:\\W\u0130NDOWS\\System32\\bash.exe", false},
		// pi's /^[a-z]:[\\/]/i is not a Unicode regex: the Kelvin sign is
		// not a drive letter, so this SystemRoot recognizes nothing.
		{ptr("\u212A:\\WinNT"), `K:\WinNT\System32\bash.exe`, false},
		// A drive-relative SystemRoot is not drive-absolute either, even when
		// the shell path joins to it exactly.
		{ptr(`C:WinNT`), `C:WinNT\System32\bash.exe`, false},
	} {
		env := map[string]string{}
		label := "unset"
		if tt.systemRoot != nil {
			env["SystemRoot"] = *tt.systemRoot
			label = strconv.Quote(*tt.systemRoot)
		}
		shell, args, stdin, err := fakeShellHost(true, env, nil, nil).shellConfig(tt.shell)
		if err != nil {
			t.Fatalf("SystemRoot=%s %q: %v", label, tt.shell, err)
		}
		wantArgs := []string{"-c"}
		if tt.stdin {
			wantArgs = []string{"-s"}
		}
		if shell != tt.shell || stdin != tt.stdin || !reflect.DeepEqual(args, wantArgs) {
			t.Errorf("SystemRoot=%s %q: got shell=%q args=%v stdin=%v, want args=%v stdin=%v",
				label, tt.shell, shell, args, stdin, wantArgs, tt.stdin)
		}
	}
}

// TestGetPowerShellConfigInstallDirs ports powershell-config.test.ts (upstream
// 61060ddf8, pi#9490): PowerShell is found outside PATH under ProgramFiles and
// SystemRoot, on any drive, in the order pwsh on PATH, PowerShell 7's install
// directory, powershell on PATH, Windows PowerShell's system directory.
func TestGetPowerShellConfigInstallDirs(t *testing.T) {
	const pwsh7 = `D:\Applications\PowerShell\7\pwsh.exe`
	const windowsPowerShell = `E:\WinNT\System32\WindowsPowerShell\v1.0\powershell.exe`
	env := map[string]string{"ProgramFiles": `D:\Applications`, "SystemRoot": `E:\WinNT`}

	for _, tt := range []struct {
		name   string
		exists func(string) bool
		onPath map[string]string
		want   string
	}{
		{
			name:   "prefers PowerShell 7 on PATH over known installation locations",
			onPath: map[string]string{"pwsh.exe": `E:\Tools\pwsh.exe`, "powershell.exe": `E:\Tools\pwsh.exe`},
			want:   `E:\Tools\pwsh.exe`,
		},
		{
			name:   "finds PowerShell 7 under ProgramFiles before Windows PowerShell on PATH",
			exists: existsOnly(pwsh7, windowsPowerShell),
			onPath: map[string]string{"powershell.exe": windowsPowerShell},
			want:   pwsh7,
		},
		{
			name:   "prefers Windows PowerShell on PATH over its system installation",
			exists: existsOnly(`E:\Tools\powershell.exe`, windowsPowerShell),
			onPath: map[string]string{"powershell.exe": `E:\Tools\powershell.exe`},
			want:   `E:\Tools\powershell.exe`,
		},
		{
			name:   "finds Windows PowerShell under SystemRoot when PATH lookup is unavailable",
			exists: existsOnly(windowsPowerShell),
			want:   windowsPowerShell,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			shell, args, stdin, err := fakeShellHost(true, env, tt.exists, tt.onPath).powerShellConfig()
			if err != nil {
				t.Fatal(err)
			}
			if shell != tt.want || stdin || !reflect.DeepEqual(args, powershellArgs) {
				t.Fatalf("got shell=%q args=%v stdin=%v, want %q %v", shell, args, stdin, tt.want, powershellArgs)
			}
		})
	}

	t.Run("reports an error when no PowerShell installation exists", func(t *testing.T) {
		_, _, _, err := fakeShellHost(true, env, existsOnly(), nil).powerShellConfig()
		if err == nil {
			t.Fatal("want an error, got none")
		}
		want := "No PowerShell executable found. Install PowerShell or add powershell.exe/pwsh.exe to PATH."
		if got := err.Error(); got != want {
			t.Fatalf("error\n got: %q\nwant: %q", got, want)
		}
	})
}

func TestLocalShellHostUsesShellPathExists(t *testing.T) {
	got := reflect.ValueOf(localShellHost.exists).Pointer()
	if want := reflect.ValueOf(shellPathExists).Pointer(); got != want {
		t.Fatal("localShellHost.exists is not shellPathExists")
	}
}
