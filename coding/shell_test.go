package coding

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
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

func TestLocalShellHostUsesShellPathExists(t *testing.T) {
	got := reflect.ValueOf(localShellHost.exists).Pointer()
	if want := reflect.ValueOf(shellPathExists).Pointer(); got != want {
		t.Fatal("localShellHost.exists is not shellPathExists")
	}
}
