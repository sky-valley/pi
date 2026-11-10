//go:build windows

package coding

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// findWindowsPowerShell is powershell-tool.test.ts findPowerShell: the
// configured shell when it has that file name, else the standard install path.
func findWindowsPowerShell(fileName string) string {
	var candidates []string
	if fileName == "powershell.exe" {
		root := os.Getenv("SystemRoot")
		if root == "" {
			root = `C:\Windows`
		}
		candidates = []string{filepath.Join(root, "System32", "WindowsPowerShell", "v1.0", fileName)}
	} else {
		programFiles := os.Getenv("ProgramFiles")
		if programFiles == "" {
			programFiles = `C:\Program Files`
		}
		candidates = []string{filepath.Join(programFiles, "PowerShell", "7", fileName)}
	}
	if config, err := GetPowerShellConfig(); err == nil && strings.EqualFold(win32Basename(config.Shell), fileName) {
		candidates = append([]string{config.Shell}, candidates...)
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

// TestLocalPowerShellOperationsExitCodes ports powershell-tool.test.ts
// "powershell exit codes" (upstream c9dd2c530, #8842): Windows PowerShell 5.1
// and PowerShell 7 report the exit codes PowerShell 7 reports, and an
// unfinished command is a parse error that runs nothing.
func TestLocalPowerShellOperationsExitCodes(t *testing.T) {
	const stderrExit0 = `cmd /c "echo progress 1>&2 & exit 0"`
	const stderrExit7 = `cmd /c "echo progress 1>&2 & exit 7"`
	cases := []struct {
		command  string
		exitCode int
	}{
		{stderrExit0 + " 2>&1 | Select-Object -Last 3", 0},
		{stderrExit0 + " 2>&1", 0},
		{stderrExit0 + " 2>$null", 0},
		{"$output = " + stderrExit0 + " 2>&1", 0},
		{stderrExit0, 0},
		{`Get-Item C:\pi-missing-8842; ` + stderrExit0 + " 2>&1", 0},
		{stderrExit7 + " 2>&1 | Select-Object -Last 3", 1},
		{stderrExit7, 1},
		{`cmd /c "exit 3"`, 1},
		{`Get-Item C:\pi-missing-8842`, 1},
		{stderrExit0 + ` 2>&1; Get-Item C:\pi-missing-8842`, 1},
		{`Write-Error "failed"`, 1},
		{`throw "failed"`, 1},
		{stderrExit7 + "; Write-Output done", 0},
		{"Write-Output done # comment", 0},
		{"Set-StrictMode -Version Latest; " + stderrExit0 + " 2>&1", 0},
		{"exit 5", 5},
	}
	var unfinished []string
	for _, statement := range []string{
		"Write-Output done `",
		"Get-ChildItem |",
		"$true -and",
		"Write-Output a,",
		"$x =",
		"Write-Output (1 +",
		`Write-Output "unclosed`,
		"if ($true) {",
	} {
		unfinished = append(unfinished, `Write-Output ("pi-" + "ran"); `+statement)
	}

	for _, fileName := range []string{"powershell.exe", "pwsh.exe"} {
		t.Run(fileName, func(t *testing.T) {
			shellPath := findWindowsPowerShell(fileName)
			if shellPath == "" {
				t.Skipf("%s is not installed", fileName)
			}
			ops := powerShellOperations(
				func() (ShellConfig, error) {
					return ShellConfig{Shell: shellPath, Args: slices.Clone(powershellArgs)}, nil
				},
				func(shellConfig ShellConfig) BashOperations {
					return localShellOperations("PowerShell", func() (ShellConfig, error) { return shellConfig, nil })
				},
			)
			cwd, _ := os.Getwd()
			for _, c := range cases {
				code, err := ops.Exec(context.Background(), c.command, cwd, BashExecOptions{OnData: func([]byte) {}})
				if err != nil || code == nil || *code != c.exitCode {
					t.Errorf("%q: exit %v (err %v), want %d", c.command, code, err, c.exitCode)
				}
			}
			for _, command := range unfinished {
				var output strings.Builder
				code, err := ops.Exec(context.Background(), command, cwd, BashExecOptions{OnData: func(b []byte) { output.Write(b) }})
				if err != nil || code == nil || *code != 1 || strings.Contains(output.String(), "pi-ran") {
					t.Errorf("%q: exit %v (err %v), ran=%v; want exit 1 and nothing run", command, code, err, strings.Contains(output.String(), "pi-ran"))
				}
			}
		})
	}
}
