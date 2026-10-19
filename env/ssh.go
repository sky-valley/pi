package env

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/sky-valley/pi/internal/jstext"
)

// RemotePlatform is a remote system pi-env ships a daemon for.
type RemotePlatform struct {
	// Platform is linux, android, darwin or windows.
	Platform string
	// Arch is x64 or arm64.
	Arch string
	// Home is the remote home directory, in the remote system's own spelling.
	Home string
	// Shell is, on Windows, the shell sshd runs commands with (DefaultShell):
	// cmd or powershell.
	Shell string
	// Warnings are problems worth telling the owner about, such as Termux
	// without termux-exec.
	Warnings []string
}

// SSHTarget says how to reach a machine with the system ssh.
type SSHTarget struct {
	// Host is a host name or ~/.ssh/config alias.
	Host string
	// User, Port and IdentityFile are left to ssh when empty or zero.
	User         string
	Port         int
	IdentityFile string
	// KnownHostsFile holds the host keys this application trusts;
	// AcceptHostKey adds to it.
	KnownHostsFile string
	// HostKeyAlias is the name keys are stored under, independent of aliases,
	// ports and jump hosts, e.g. "pi-env-<env name>".
	HostKeyAlias string
	// SSH is the ssh program; empty is "ssh".
	SSH string
	// ConfigFile is an ssh configuration file to use instead of
	// ~/.ssh/config (-F).
	ConfigFile string
}

func (t SSHTarget) program() string {
	if t.SSH == "" {
		return "ssh"
	}
	return t.SSH
}

// HostKeyUnknownError means the remote host's key is not in KnownHostsFile;
// ScanHostKey shows it so the owner can accept it.
type HostKeyUnknownError struct{ Message string }

func (e *HostKeyUnknownError) Error() string { return e.Message }

// HostKeyChangedError means the remote host's key differs from the one in
// KnownHostsFile; it is never accepted automatically.
type HostKeyChangedError struct{ Message string }

func (e *HostKeyChangedError) Error() string { return e.Message }

// SSHError is an ssh invocation that failed for another reason, with its
// exit code (nil when it did not exit on its own) and diagnostics.
type SSHError struct {
	Message  string
	ExitCode *int
	Stderr   string
}

func (e *SSHError) Error() string { return e.Message }

// controlCharacters are the characters no ssh field may hold.
var controlCharacters = regexp.MustCompile("[\x00-\x1f\x7f]")

// jsonString is JSON.stringify of a string.
func jsonString(s string) string {
	quoted, err := jstext.Stringify(s)
	if err != nil {
		return strconv.Quote(s)
	}
	return quoted
}

// checkField refuses a leading "-" (ssh would read it as an option) and
// whitespace or control characters.
func checkField(name, value string) error {
	if value == "" || strings.HasPrefix(value, "-") || strings.IndexFunc(value, jstext.IsWhitespace) != -1 || controlCharacters.MatchString(value) {
		return fmt.Errorf("Invalid %s: %s", name, jsonString(value))
	}
	return nil
}

// configPath quotes a path for an ssh option that expands % tokens, with %
// literal.
func configPath(name, path string) (string, error) {
	if path == "" || strings.ContainsRune(path, '"') || controlCharacters.MatchString(path) {
		return "", fmt.Errorf("Invalid %s: %s", name, jsonString(path))
	}
	return `"` + strings.ReplaceAll(path, "%", "%%") + `"`, nil
}

// SSHArguments are the arguments for ssh up to the host: no prompts, no
// forwarding of any kind, no shared connections, no commands from the
// configuration, no locale forwarding (the remote uses its own), and host
// keys checked strictly against the application's own file under a fixed
// alias.
func SSHArguments(target SSHTarget) ([]string, error) {
	return sshArguments(target, true, target.KnownHostsFile)
}

func sshArguments(target SSHTarget, strictHostKeys bool, knownHostsFile string) ([]string, error) {
	if err := checkField("host", target.Host); err != nil {
		return nil, err
	}
	if err := checkField("host key alias", target.HostKeyAlias); err != nil {
		return nil, err
	}
	if target.User != "" {
		if err := checkField("user", target.User); err != nil {
			return nil, err
		}
	}
	if target.Port != 0 && (target.Port < 1 || target.Port > 65535) {
		return nil, fmt.Errorf("Invalid port: %d", target.Port)
	}
	knownHosts, err := configPath("known hosts file", knownHostsFile)
	if err != nil {
		return nil, err
	}
	var args []string
	option := func(setting string) { args = append(args, "-o", setting) }
	if target.ConfigFile != "" {
		args = append(args, "-F", target.ConfigFile)
	}
	args = append(args, "-T", "-a", "-x")
	option("BatchMode=yes")
	option("ClearAllForwardings=yes")
	option("ForwardAgent=no")
	option("ForwardX11=no")
	option("ControlMaster=no")
	option("ControlPath=none")
	option("RemoteCommand=none")
	option("PermitLocalCommand=no")
	option("SendEnv=-*")
	option("ServerAliveInterval=15")
	if strictHostKeys {
		option("StrictHostKeyChecking=yes")
	} else {
		option("StrictHostKeyChecking=accept-new")
	}
	option("UserKnownHostsFile=" + knownHosts)
	option("GlobalKnownHostsFile=none")
	option("HashKnownHosts=no")
	option("HostKeyAlias=" + target.HostKeyAlias)
	if target.User != "" {
		args = append(args, "-l", target.User)
	}
	if target.Port != 0 {
		args = append(args, "-p", strconv.Itoa(target.Port))
	}
	if target.IdentityFile != "" {
		identity, err := configPath("identity file", target.IdentityFile)
		if err != nil {
			return nil, err
		}
		option("IdentityFile=" + identity)
		option("IdentitiesOnly=yes")
	}
	return append(args, "--", target.Host), nil
}

// How long a detection, check or upload over ssh may take before it is
// abandoned.
const (
	sshTimeout    = 60 * time.Second
	uploadTimeout = 300 * time.Second
)

type sshOptions struct {
	stdin   []byte
	args    []string
	timeout time.Duration
}

// lockedBuffer collects output that is read before the process ends.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return jstext.DecodeUTF8(b.buf.Bytes())
}

var (
	hostKeyChanged = regexp.MustCompile(`REMOTE HOST IDENTIFICATION HAS CHANGED`)
	hostKeyUnknown = regexp.MustCompile(`Host key verification failed|No .* host key is known`)
)

// runSSH runs one remote command over ssh, feeding stdin, and returns its
// stdout.
func runSSH(ctx context.Context, target SSHTarget, command string, options sshOptions) (string, error) {
	args := options.args
	if args == nil {
		var err error
		if args, err = SSHArguments(target); err != nil {
			return "", err
		}
	}
	timeout := options.timeout
	if timeout == 0 {
		timeout = sshTimeout
	}
	cmd := exec.Command(target.program(), append(append([]string(nil), args...), command)...)
	cmd.Stdin = bytes.NewReader(options.stdin)
	var stdout, stderr lockedBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		return "", &SSHError{Message: spawnError(err, target.program()).Message}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		killProcess(cmd.Process)
		diagnostics := stderr.String()
		return "", &SSHError{
			Message: fmt.Sprintf("ssh %s did not finish within %s s: %s", target.Host, jstext.NumberToString(timeout.Seconds()), jstext.Trim(diagnostics)),
			Stderr:  diagnostics,
		}
	case <-ctx.Done():
		killProcess(cmd.Process)
		return "", ctx.Err()
	}
	output, diagnostics := stdout.String(), stderr.String()
	code := cmd.ProcessState.ExitCode()
	if code == 0 {
		return output, nil
	}
	if hostKeyChanged.MatchString(diagnostics) {
		return "", &HostKeyChangedError{Message: fmt.Sprintf("The host key of %s changed; remove the old key to continue", target.Host)}
	}
	if hostKeyUnknown.MatchString(diagnostics) {
		return "", &HostKeyUnknownError{Message: fmt.Sprintf("The host key of %s is not trusted yet", target.Host)}
	}
	var exitCode *int
	codeText := "null"
	if code >= 0 {
		exitCode, codeText = &code, strconv.Itoa(code)
	}
	return "", &SSHError{
		Message:  fmt.Sprintf("ssh %s failed with exit code %s: %s", target.Host, codeText, jstext.Trim(diagnostics)),
		ExitCode: exitCode,
		Stderr:   diagnostics,
	}
}

// HostKeyScan is what ScanHostKey received: known-hosts lines and their
// fingerprints.
type HostKeyScan struct {
	Lines        []string
	Fingerprints []string
}

// ScanHostKey connects once with accept-new against a temporary known-hosts
// file to capture the key the host presents, through the same route
// (~/.ssh/config, jump hosts) as real connections. Showing a fingerprint is
// not authentication: compare it with one obtained out of band before
// accepting it.
func ScanHostKey(ctx context.Context, target SSHTarget) (HostKeyScan, error) {
	directory, err := os.MkdirTemp("", "pi-env-hostkey-")
	if err != nil {
		return HostKeyScan{}, err
	}
	defer os.RemoveAll(directory)
	scanned := filepath.Join(directory, "known_hosts")
	// Authentication may fail without the key; the key is recorded before
	// authentication.
	args, err := sshArguments(target, false, scanned)
	if err == nil {
		_, err = runSSH(ctx, target, "exit 0", sshOptions{args: args})
	}
	if err != nil {
		var sshErr *SSHError
		var unknown *HostKeyUnknownError
		if !errors.As(err, &sshErr) && !errors.As(err, &unknown) {
			return HostKeyScan{}, err
		}
	}
	var lines []string
	if content, err := os.ReadFile(scanned); err == nil {
		for _, line := range strings.Split(jstext.DecodeUTF8(content), "\n") {
			if jstext.Trim(line) != "" {
				lines = append(lines, line)
			}
		}
	}
	if len(lines) == 0 {
		return HostKeyScan{}, fmt.Errorf("No host key received from %s", target.Host)
	}
	keygen := exec.CommandContext(ctx, "ssh-keygen", "-lf", scanned)
	var output bytes.Buffer
	keygen.Stdout = &output
	if err := keygen.Start(); err != nil {
		return HostKeyScan{}, spawnError(err, "ssh-keygen")
	}
	_ = keygen.Wait()
	var fingerprints []string
	for _, line := range strings.Split(jstext.DecodeUTF8(output.Bytes()), "\n") {
		if jstext.Trim(line) != "" {
			fingerprints = append(fingerprints, line)
		}
	}
	return HostKeyScan{Lines: lines, Fingerprints: fingerprints}, nil
}

var (
	hostKeyType = regexp.MustCompile(`^(ssh-[a-z0-9-]+|ecdsa-sha2-[a-z0-9-]+|sk-[a-z0-9@.-]+)$`)
	hostKeyData = regexp.MustCompile(`^[A-Za-z0-9+/]+={0,2}$`)
)

// splitWhitespace is JavaScript's s.trim().split(/\s+/) for a line with
// content.
func splitWhitespace(s string) []string {
	return strings.FieldsFunc(jstext.Trim(s), jstext.IsWhitespace)
}

// parseHostKeyLine reads a known-hosts line for alias: "alias type key
// [comment]", no markers, patterns or hashed names.
func parseHostKeyLine(alias, line string) (keyType, key string, err error) {
	parts := splitWhitespace(line)
	if len(parts) < 3 || parts[0] != alias || !hostKeyType.MatchString(parts[1]) || !hostKeyData.MatchString(parts[2]) {
		return "", "", fmt.Errorf("Not a host key line for %s: %s", alias, jsonString(line))
	}
	return parts[1], parts[2], nil
}

// knownHostsLocks serializes changes to one known-hosts file in this process.
var knownHostsLocks sync.Map

// changeKnownHosts rewrites the known-hosts file one change at a time,
// atomically.
func changeKnownHosts(file string, change func(lines []string) ([]string, error)) error {
	lock, _ := knownHostsLocks.LoadOrStore(file, &sync.Mutex{})
	lock.(*sync.Mutex).Lock()
	defer lock.(*sync.Mutex).Unlock()
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	var existing []string
	if content, err := os.ReadFile(file); err == nil {
		for _, line := range strings.Split(jstext.DecodeUTF8(content), "\n") {
			if line != "" {
				existing = append(existing, line)
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	lines, err := change(existing)
	if err != nil {
		return err
	}
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	temporary := file + "." + hex.EncodeToString(suffix) + ".tmp"
	content := ""
	if len(lines) > 0 {
		content = strings.Join(lines, "\n") + "\n"
	}
	out, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = out.WriteString(content)
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(temporary, file)
	}
	if err != nil {
		os.Remove(temporary)
		return err
	}
	return nil
}

// AcceptHostKey trusts host-key lines from ScanHostKey by adding them to the
// target's known-hosts file. Only plain lines for the target's alias are
// accepted. A key that differs from a trusted key of the same type is
// refused with HostKeyChangedError; ForgetHostKey must remove the old keys
// first.
func AcceptHostKey(target SSHTarget, lines []string) error {
	alias := target.HostKeyAlias
	type accepted struct{ line, keyType, key string }
	var keys []accepted
	for _, line := range lines {
		keyType, key, err := parseHostKeyLine(alias, line)
		if err != nil {
			return err
		}
		keys = append(keys, accepted{jstext.Trim(line), keyType, key})
	}
	return changeKnownHosts(target.KnownHostsFile, func(existing []string) ([]string, error) {
		trusted := map[string]string{}
		for _, line := range existing {
			if parts := splitWhitespace(line); len(parts) >= 3 && parts[0] == alias {
				trusted[parts[1]] = parts[2]
			}
		}
		var added []string
		for _, k := range keys {
			known, ok := trusted[k.keyType]
			if ok && known == k.key {
				continue
			}
			if ok {
				return nil, &HostKeyChangedError{Message: fmt.Sprintf("The %s host key of %s changed; forget the old key first", k.keyType, target.Host)}
			}
			trusted[k.keyType] = k.key
			added = append(added, k.line)
		}
		return append(existing, added...), nil
	})
}

// ForgetHostKey stops trusting every key stored for the target's alias, e.g.
// after the owner confirmed a changed host key.
func ForgetHostKey(target SSHTarget) error {
	return changeKnownHosts(target.KnownHostsFile, func(existing []string) ([]string, error) {
		var kept []string
		for _, line := range existing {
			if parts := splitWhitespace(line); len(parts) == 0 || parts[0] != target.HostKeyAlias {
				kept = append(kept, line)
			}
		}
		return kept, nil
	})
}

// posixProbe is the POSIX detection, run by the remote login shell; a Windows
// host answers through PowerShell instead.
const posixProbe = `sh -c 'echo PI-ENV-PROBE; uname -s; uname -m; uname -o 2>/dev/null || echo -; printf "%s\n" "$HOME" "${TMPDIR:--}" "${LD_PRELOAD:--}"'`

// powershell runs a script through PowerShell as an encoded command.
func powershell(script string) string {
	units := utf16.Encode([]rune(script))
	encoded := make([]byte, 2*len(units))
	for i, unit := range units {
		binary.LittleEndian.PutUint16(encoded[2*i:], unit)
	}
	return "powershell -NoProfile -NonInteractive -EncodedCommand " + base64.StdEncoding.EncodeToString(encoded)
}

var windowsProbe = powershell("'PI-ENV-PROBE'; 'Windows'; [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString(); '-'; $HOME; '-'; '-'")

func normalizeArch(machine string) (string, error) {
	switch jstext.ToLower(machine) {
	case "x86_64", "amd64", "x64":
		return "x64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	}
	return "", fmt.Errorf("Unsupported remote architecture: %s", machine)
}

type probe struct{ system, machine, os, home, tmpdir, preload string }

var lineBreak = regexp.MustCompile(`\r?\n`)

func parseProbe(target SSHTarget, output string) (probe, error) {
	// Login shells may print a banner first.
	lines := lineBreak.Split(output, -1)
	start := -1
	for i, line := range lines {
		if line == "PI-ENV-PROBE" {
			start = i
			break
		}
	}
	if start == -1 {
		return probe{}, fmt.Errorf("Unexpected answer from %s: %s", target.Host, jstext.Trim(output))
	}
	rest := lines[start+1:]
	value := func(i int, fallback string) string {
		if i < len(rest) {
			return rest[i]
		}
		return fallback
	}
	return probe{value(0, ""), value(1, ""), value(2, ""), value(3, ""), value(4, "-"), value(5, "-")}, nil
}

// termuxWarnings: Termux works only with its own TMPDIR and with
// termux-exec, which makes #!/usr/bin/env shebangs work.
func termuxWarnings(p probe) []string {
	var warnings []string
	if p.tmpdir == "-" {
		warnings = append(warnings, "TMPDIR is not set; Termux's sshd normally sets it to $PREFIX/tmp.")
	}
	if !strings.Contains(p.preload, "termux-exec") {
		warnings = append(warnings, "termux-exec is not loaded (LD_PRELOAD); scripts with #!/usr/bin/env shebangs will fail.")
	}
	return append(warnings, "Android may suspend Termux; run termux-wake-lock on the device to keep the connection alive.")
}

var gitBashSystem = regexp.MustCompile(`^(MINGW|MSYS|CYGWIN)`)

// DetectPlatform finds which system the target runs: uname through the login
// shell, or PowerShell on Windows.
func DetectPlatform(ctx context.Context, target SSHTarget) (RemotePlatform, error) {
	output, err := runSSH(ctx, target, posixProbe, sshOptions{})
	var p probe
	if err == nil {
		p, err = parseProbe(target, output)
	}
	if err != nil {
		// cmd.exe or PowerShell as the remote shell: no sh, or one that gets
		// the probe's quotes wrong.
		var unknown *HostKeyUnknownError
		var changed *HostKeyChangedError
		if errors.As(err, &unknown) || errors.As(err, &changed) || ctx.Err() != nil {
			return RemotePlatform{}, err
		}
		if p, err = windowsProbeOf(ctx, target); err != nil {
			return RemotePlatform{}, err
		}
	}
	// Git Bash as Windows' default SSH shell: ask PowerShell for Windows' own
	// architecture and home spelling.
	if gitBashSystem.MatchString(p.system) {
		if p, err = windowsProbeOf(ctx, target); err != nil {
			return RemotePlatform{}, err
		}
	}
	arch, err := normalizeArch(p.machine)
	if err != nil {
		return RemotePlatform{}, err
	}
	switch p.system {
	case "Windows":
		// cmd.exe expands %OS%; PowerShell prints it as is.
		output, err := runSSH(ctx, target, "echo %OS%", sshOptions{})
		if err != nil {
			return RemotePlatform{}, err
		}
		shell := "powershell"
		if strings.Contains(output, "Windows_NT") {
			shell = "cmd"
		}
		return RemotePlatform{Platform: "windows", Arch: arch, Home: p.home, Shell: shell, Warnings: []string{}}, nil
	case "Darwin":
		return RemotePlatform{Platform: "darwin", Arch: arch, Home: p.home, Warnings: []string{}}, nil
	case "Linux":
		if p.os == "Android" {
			return RemotePlatform{Platform: "android", Arch: arch, Home: p.home, Warnings: termuxWarnings(p)}, nil
		}
		return RemotePlatform{Platform: "linux", Arch: arch, Home: p.home, Warnings: []string{}}, nil
	}
	return RemotePlatform{}, fmt.Errorf("Unsupported remote system: %s", p.system)
}

func windowsProbeOf(ctx context.Context, target SSHTarget) (probe, error) {
	output, err := runSSH(ctx, target, windowsProbe, sshOptions{})
	if err != nil {
		return probe{}, err
	}
	return parseProbe(target, output)
}

func quotePosix(text string) string { return "'" + strings.ReplaceAll(text, "'", `'\''`) + "'" }

func quotePowerShell(text string) string { return "'" + strings.ReplaceAll(text, "'", "''") + "'" }

// daemonPath is where a daemon with this content lives on the remote machine:
// named by its SHA-256, so versions never collide.
func daemonPath(remote RemotePlatform, sha string) string {
	name := "pi-env-" + sha[:32]
	if remote.Platform == "windows" {
		return remote.Home + `\.pi\mobile\tools\` + name + ".exe"
	}
	return remote.Home + "/.pi/mobile/tools/" + name
}

const posixHash = `hash() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; elif command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | cut -d' ' -f1; elif command -v openssl >/dev/null 2>&1; then openssl dgst -sha256 "$1" | sed 's/.*= //'; else echo none; fi; }`

// checkDaemon reports whether the remote file has this content: present,
// missing, or (POSIX without a hash tool) nohash.
func checkDaemon(ctx context.Context, target SSHTarget, remote RemotePlatform, file, sha string) (string, error) {
	if remote.Platform == "windows" {
		check := powershell("$f = " + quotePowerShell(file) + "; if ((Test-Path -LiteralPath $f) -and ((Get-FileHash -Algorithm SHA256 -LiteralPath $f).Hash.ToLower() -eq '" + sha + "')) { 'present' } else { 'missing' }")
		output, err := runSSH(ctx, target, check, sshOptions{})
		if err != nil {
			return "", err
		}
		if strings.HasSuffix(jstext.Trim(output), "present") {
			return "present", nil
		}
		return "missing", nil
	}
	check := posixHash + "; f=" + quotePosix(file) + `; if [ -f "$f" ] && [ "$(hash "$f")" = ` + sha + ` ]; then echo present; elif [ "$(hash /dev/null)" = none ]; then echo nohash; else echo missing; fi`
	output, err := runSSH(ctx, target, "sh -c "+quotePosix(check), sshOptions{})
	if err != nil {
		return "", err
	}
	lines := strings.Split(jstext.Trim(output), "\n")
	return lines[len(lines)-1], nil
}

// DeployDaemon makes sure the daemon is on the remote machine, verified by
// its SHA-256 before it ever runs, and removes daemons of other contents. The
// upload goes to a new temporary file and is renamed into place only once its
// hash matches. binary is a daemon file to deploy; empty deploys the daemon
// embedded for remote (PackagedDaemon). It returns the remote path of the
// daemon.
func DeployDaemon(ctx context.Context, target SSHTarget, remote RemotePlatform, binary string) (string, error) {
	var content []byte
	if binary == "" {
		packaged, err := PackagedDaemon(remote)
		if err != nil {
			return "", err
		}
		content = packaged
	} else {
		read, err := os.ReadFile(binary)
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("No pi-env daemon for %s-%s at %s", remote.Platform, remote.Arch, binary)
		}
		if err != nil {
			return "", err
		}
		content = read
	}
	digest := sha256.Sum256(content)
	sha := hex.EncodeToString(digest[:])
	file := daemonPath(remote, sha)
	state, err := checkDaemon(ctx, target, remote, file, sha)
	if err != nil {
		return "", err
	}
	if state == "present" {
		return file, nil
	}
	if state == "nohash" {
		return "", fmt.Errorf("%s has no sha256sum, shasum or openssl to verify pi-env", target.Host)
	}
	if remote.Platform == "windows" {
		upload := powershell(strings.Join([]string{
			"$ErrorActionPreference = 'Stop'",
			"$f = " + quotePowerShell(file),
			"$d = Split-Path -Parent $f",
			"New-Item -ItemType Directory -Force -Path $d | Out-Null",
			"$t = Join-Path $d ('.pi-env-' + [guid]::NewGuid().ToString() + '.tmp')",
			// PowerShell reads redirected stdin itself, as text lines for
			// $input, so the binary comes as base64 lines up to an end
			// marker: Windows' sshd may never pass on the end of stdin.
			"$text = New-Object System.Text.StringBuilder",
			"foreach ($line in $input) { if ($line -eq 'PI-ENV-END') { break }; [void]$text.Append($line) }",
			"$bytes = [Convert]::FromBase64String($text.ToString())",
			"$out = [IO.File]::Open($t, 'CreateNew', 'Write', 'None'); $out.Write($bytes, 0, $bytes.Length); $out.Close()",
			"if ((Get-FileHash -Algorithm SHA256 -LiteralPath $t).Hash.ToLower() -ne '" + sha + "') { Remove-Item -LiteralPath $t; throw 'pi-env upload is corrupt' }",
			// A running daemon or a virus scanner can hold the old file for a
			// moment.
			"for ($i = 0; ; $i++) { try { Move-Item -Force -LiteralPath $t -Destination $f; break } catch { if ($i -ge 20) { throw }; Start-Sleep -Milliseconds 250 } }",
			// Older daemons; one that is running stays until it exits.
			"Get-ChildItem -LiteralPath $d -Filter 'pi-env-*.exe' | Where-Object { $_.FullName -ne $f } | ForEach-Object { Remove-Item -LiteralPath $_.FullName -ErrorAction SilentlyContinue }",
			"'deployed'",
		}, "; "))
		encoded := base64.StdEncoding.EncodeToString(content)
		var lines strings.Builder
		for len(encoded) > 0 {
			n := min(len(encoded), 76)
			lines.WriteString(encoded[:n])
			lines.WriteByte('\n')
			encoded = encoded[n:]
		}
		lines.WriteString("PI-ENV-END\n")
		if _, err := runSSH(ctx, target, upload, sshOptions{stdin: []byte(lines.String()), timeout: uploadTimeout}); err != nil {
			return "", err
		}
		return file, nil
	}
	upload := strings.Join([]string{
		"set -e",
		posixHash,
		"f=" + quotePosix(file),
		`d=$(dirname "$f")`,
		`mkdir -p "$d"`,
		`chmod 700 "$d"`,
		`t=$(mktemp "$d/.pi-env.XXXXXX")`,
		`cat > "$t"`,
		`if [ "$(hash "$t")" != ` + sha + ` ]; then rm -f "$t"; echo "pi-env upload is corrupt" >&2; exit 1; fi`,
		`chmod 700 "$t"`,
		`mv -f "$t" "$f"`,
		// Older daemons; running ones keep their file open until they exit.
		`for old in "$d"/pi-env-*; do [ "$old" = "$f" ] || rm -f "$old"; done`,
		"echo deployed",
	}, "\n")
	if _, err := runSSH(ctx, target, "sh -c "+quotePosix(upload), sshOptions{stdin: content, timeout: uploadTimeout}); err != nil {
		return "", err
	}
	return file, nil
}

// SSHConnectOptions are the options of SSHConnection and ConnectSSH: the
// target, plus the daemon to deploy.
type SSHConnectOptions struct {
	SSHTarget
	// Binary is a daemon file to deploy; empty is the embedded daemon for
	// the remote system.
	Binary string
	// LoginShell starts the daemon (POSIX) through the user's login shell
	// ($SHELL -l), so commands see the environment of ~/.profile and similar
	// files. Off by default: ssh runs commands without a login shell.
	LoginShell bool
	OnLog      func(text string)
}

// launchCommand is the remote command that starts the daemon at file, before
// the serve arguments.
func launchCommand(remote RemotePlatform, file string, loginShell bool) string {
	if remote.Platform == "windows" {
		// PowerShell runs a quoted path only with the call operator; cmd.exe
		// keeps one pair of quotes around a program.
		if remote.Shell == "powershell" {
			return "& " + quotePowerShell(file)
		}
		if strings.Contains(file, " ") {
			return `"` + file + `"`
		}
		return file
	}
	// $0 is the daemon, "$@" its arguments; $SHELL is the login shell's own
	// name for itself.
	if loginShell {
		return `exec "$SHELL" -lc 'exec "$0" "$@"' ` + quotePosix(file)
	}
	return quotePosix(file)
}

// sshState is what an SSH connection learned about its remote system, and
// the daemon it verified last.
type sshState struct {
	mu       sync.Mutex
	remote   *RemotePlatform
	verified string
}

// sshConnectionFor is a Connection whose every start detects the remote
// system (once), verifies or deploys the daemon, then starts it.
func sshConnectionFor(options SSHConnectOptions, state *sshState) *Connection {
	var connection *Connection
	connection = NewConnection(ConnectionOptions{
		CommandFunc: func(ctx context.Context) ([]string, error) {
			state.mu.Lock()
			remote, verified := state.remote, state.verified
			state.mu.Unlock()
			if remote == nil {
				detected, err := DetectPlatform(ctx, options.SSHTarget)
				if err != nil {
					return nil, err
				}
				for _, warning := range detected.Warnings {
					connection.log(warning + "\n")
				}
				remote = &detected
				state.mu.Lock()
				state.remote = remote
				state.mu.Unlock()
			}
			// A deployment that just verified the binary counts for the first
			// start.
			file := verified
			if file == "" {
				deployed, err := DeployDaemon(ctx, options.SSHTarget, *remote, options.Binary)
				if err != nil {
					return nil, err
				}
				file = deployed
			}
			state.mu.Lock()
			state.verified = ""
			state.mu.Unlock()
			args, err := SSHArguments(options.SSHTarget)
			if err != nil {
				return nil, err
			}
			command := append([]string{options.SSHTarget.program()}, args...)
			return append(command, launchCommand(*remote, file, options.LoginShell)), nil
		},
		OnLog: options.OnLog,
	})
	return connection
}

// SSHConnection returns a Connection to the target that does nothing until
// its first request. Each start detects the remote system (the first time),
// verifies the daemon and deploys it if it is missing or changed, then
// starts it over ssh. A failure (no network, an untrusted or changed host
// key) fails the requests waiting for that start with code spawn_error and
// the ssh diagnostics as message; the next request tries again. remote
// returns the detected system, once known.
func SSHConnection(options SSHConnectOptions) (connection *Connection, remote func() *RemotePlatform) {
	state := &sshState{}
	return sshConnectionFor(options, state), func() *RemotePlatform {
		state.mu.Lock()
		defer state.mu.Unlock()
		if state.remote == nil {
			return nil
		}
		copied := *state.remote
		return &copied
	}
}

// ConnectSSH detects the remote system and deploys the daemon now, then
// returns a Connection that starts it over ssh, like SSHConnection. Host keys
// must already be trusted (ScanHostKey, AcceptHostKey); otherwise it fails
// with HostKeyUnknownError.
func ConnectSSH(ctx context.Context, options SSHConnectOptions) (*Connection, RemotePlatform, error) {
	remote, err := DetectPlatform(ctx, options.SSHTarget)
	if err != nil {
		return nil, RemotePlatform{}, err
	}
	verified, err := DeployDaemon(ctx, options.SSHTarget, remote, options.Binary)
	if err != nil {
		return nil, RemotePlatform{}, err
	}
	for _, warning := range remote.Warnings {
		if options.OnLog != nil {
			options.OnLog(warning + "\n")
		}
	}
	return sshConnectionFor(options, &sshState{remote: &remote, verified: verified}), remote, nil
}
