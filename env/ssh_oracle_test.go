package env

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// The SSH bootstrap's ssh calls are security-relevant and user-observable:
// testdata/ssh/oracle.json records the calls upstream's TypeScript client
// makes (capture.mts, against fakessh.sh) for the scenarios of
// scenarios.json, and the Go client must make the same calls, byte for byte,
// and end the same way.

type sshScenario struct {
	Name       string          `json:"name"`
	API        string          `json:"api"`
	LoginShell bool            `json:"loginShell"`
	Target     json.RawMessage `json:"target"`
	Responses  []struct {
		Code    int     `json:"code"`
		Out     *string `json:"out"`
		Err     *string `json:"err"`
		NoStdin bool    `json:"nostdin"`
	} `json:"responses"`
}

type sshSpec struct {
	BinaryLength int               `json:"binaryLength"`
	Scenarios    []sshScenario     `json:"scenarios"`
	Arguments    []json.RawMessage `json:"arguments"`
}

type sshOracle struct {
	BinarySHA256 string `json:"binarySha256"`
	Scenarios    []struct {
		Name    string          `json:"name"`
		Outcome json.RawMessage `json:"outcome"`
		Logs    []string        `json:"logs"`
		Calls   []struct {
			Argv  []string `json:"argv"`
			Stdin *string  `json:"stdin"`
		} `json:"calls"`
	} `json:"scenarios"`
	Arguments []json.RawMessage `json:"arguments"`
}

// targetJSON reads a scenario's target in upstream's field names.
func targetJSON(t *testing.T, raw json.RawMessage) SSHTarget {
	t.Helper()
	var fields struct {
		Host           string `json:"host"`
		User           string `json:"user"`
		Port           int    `json:"port"`
		IdentityFile   string `json:"identityFile"`
		KnownHostsFile string `json:"knownHostsFile"`
		HostKeyAlias   string `json:"hostKeyAlias"`
		ConfigFile     string `json:"configFile"`
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	return SSHTarget{Host: fields.Host, User: fields.User, Port: fields.Port, IdentityFile: fields.IdentityFile,
		KnownHostsFile: fields.KnownHostsFile, HostKeyAlias: fields.HostKeyAlias, ConfigFile: fields.ConfigFile}
}

func loadSSHOracle(t *testing.T) (sshSpec, sshOracle) {
	t.Helper()
	var spec sshSpec
	var oracle sshOracle
	for path, into := range map[string]any{"testdata/ssh/scenarios.json": &spec, "testdata/ssh/oracle.json": &oracle} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, into); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
	if len(oracle.Scenarios) != len(spec.Scenarios) || len(oracle.Arguments) != len(spec.Arguments) || len(spec.Scenarios) == 0 {
		t.Fatal("testdata/ssh/oracle.json does not answer scenarios.json; recapture it with capture.mts")
	}
	return spec, oracle
}

// errorClass names an error as upstream's class names.
func errorClass(err error) string {
	var unknown *HostKeyUnknownError
	var changed *HostKeyChangedError
	var sshErr *SSHError
	var remote *RemoteError
	switch {
	case errors.As(err, &unknown):
		return "HostKeyUnknownError"
	case errors.As(err, &changed):
		return "HostKeyChangedError"
	case errors.As(err, &sshErr):
		return "SshError"
	case errors.As(err, &remote):
		return "RemoteError"
	}
	return "Error"
}

func errorOutcome(err error) any {
	return map[string]any{"error": map[string]any{"class": errorClass(err), "message": err.Error()}}
}

func remoteJSON(remote RemotePlatform) any {
	out := map[string]any{"platform": remote.Platform, "arch": remote.Arch, "home": remote.Home, "warnings": remote.Warnings}
	if out["warnings"] == nil {
		out["warnings"] = []string{}
	}
	if remote.Shell != "" {
		out["shell"] = remote.Shell
	}
	return out
}

func startOutcome(connection *Connection) string {
	_, err := connection.Info(background())
	connection.Close()
	if err != nil {
		return err.Error()
	}
	return "started"
}

var scanDirectory = regexp.MustCompile(`"[^"]*pi-env-hostkey-[^/"]*/known_hosts"`)

func TestSSHBootstrapMakesUpstreamsCalls(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fakessh.sh needs a POSIX shell")
	}
	spec, oracle := loadSSHOracle(t)
	fake, err := filepath.Abs("testdata/ssh/fakessh.sh")
	mustDo(t, err)
	binary := make([]byte, spec.BinaryLength)
	for i := range binary {
		binary[i] = byte((i * 7919) % 251)
	}
	if sum := sha256.Sum256(binary); hex.EncodeToString(sum[:]) != oracle.BinarySHA256 {
		t.Fatal("the scenario daemon differs from the captured one")
	}
	for i, scenario := range spec.Scenarios {
		want := oracle.Scenarios[i]
		t.Run(scenario.Name, func(t *testing.T) {
			dir := tempDir(t, "pi-env-fakessh-")
			for n, response := range scenario.Responses {
				prefix := filepath.Join(dir, strconv.Itoa(n))
				mustDo(t, os.WriteFile(prefix+".code", []byte(strconv.Itoa(response.Code)), 0o644))
				if response.Out != nil {
					mustDo(t, os.WriteFile(prefix+".out", []byte(*response.Out), 0o644))
				}
				if response.Err != nil {
					mustDo(t, os.WriteFile(prefix+".err", []byte(*response.Err), 0o644))
				}
				if response.NoStdin {
					mustDo(t, os.WriteFile(prefix+".nostdin", nil, 0o644))
				}
			}
			binaryFile := filepath.Join(dir, "daemon.bin")
			mustDo(t, os.WriteFile(binaryFile, binary, 0o644))
			t.Setenv("FAKE_SSH_DIR", dir)
			target := targetJSON(t, scenario.Target)
			target.SSH = fake
			var logsMu sync.Mutex
			logs := []string{}
			options := SSHConnectOptions{SSHTarget: target, Binary: binaryFile, LoginShell: scenario.LoginShell, OnLog: func(text string) {
				logsMu.Lock()
				logs = append(logs, text)
				logsMu.Unlock()
			}}

			var outcome any
			switch scenario.API {
			case "connectSsh":
				connection, remote, err := ConnectSSH(background(), options)
				if err != nil {
					outcome = errorOutcome(err)
				} else {
					outcome = map[string]any{"remote": remoteJSON(remote), "start": startOutcome(connection)}
				}
			case "detectPlatform":
				remote, err := DetectPlatform(background(), target)
				if err != nil {
					outcome = errorOutcome(err)
				} else {
					outcome = map[string]any{"remote": remoteJSON(remote)}
				}
			case "scanHostKey":
				scan, err := ScanHostKey(background(), target)
				if err != nil {
					outcome = errorOutcome(err)
				} else {
					outcome = map[string]any{"scan": map[string]any{"lines": scan.Lines, "fingerprints": scan.Fingerprints}}
				}
			case "sshConnection":
				connection, remote := SSHConnection(options)
				var starts []any
				for attempt := 0; attempt < 3; attempt++ {
					_, err := connection.Info(background())
					if err != nil {
						var failure *RemoteError
						if !errors.As(err, &failure) {
							t.Fatalf("start %d: %#v", attempt, err)
						}
						starts = append(starts, failure.Code+": "+failure.Message)
					} else {
						starts = append(starts, "started")
					}
					if detected := remote(); detected != nil {
						starts = append(starts, remoteJSON(*detected))
					} else {
						starts = append(starts, nil)
					}
				}
				connection.Close()
				outcome = map[string]any{"starts": starts}
			default:
				t.Fatalf("unknown api %s", scenario.API)
			}

			compareJSON(t, "outcome", outcome, want.Outcome)
			logsMu.Lock()
			if !reflect.DeepEqual(logs, append([]string{}, want.Logs...)) {
				t.Errorf("logs %q, want %q", logs, want.Logs)
			}
			logsMu.Unlock()
			for n := 0; ; n++ {
				prefix := filepath.Join(dir, strconv.Itoa(n))
				raw, err := os.ReadFile(prefix + ".argv")
				if err != nil {
					if n != len(want.Calls) {
						t.Fatalf("%d ssh calls, upstream makes %d", n, len(want.Calls))
					}
					break
				}
				if n >= len(want.Calls) {
					t.Fatalf("ssh call %d, upstream makes %d", n, len(want.Calls))
				}
				argv := strings.Split(string(raw), "\x00")
				argv = argv[:len(argv)-1]
				for i := range argv {
					if i > 0 && argv[i-1] == "--token" {
						argv[i] = "<token>"
					} else {
						argv[i] = scanDirectory.ReplaceAllString(argv[i], `"<scan>/known_hosts"`)
					}
				}
				if !reflect.DeepEqual(argv, want.Calls[n].Argv) {
					t.Errorf("call %d arguments:\n got %q\nwant %q", n, argv, want.Calls[n].Argv)
				}
				var stdin *string
				if data, err := os.ReadFile(prefix + ".stdin"); err == nil {
					encoded := base64.StdEncoding.EncodeToString(data)
					stdin = &encoded
				}
				if (stdin == nil) != (want.Calls[n].Stdin == nil) || (stdin != nil && *stdin != *want.Calls[n].Stdin) {
					t.Errorf("call %d stdin differs", n)
				}
			}
		})
	}
}

func TestSSHArgumentsMatchUpstream(t *testing.T) {
	spec, oracle := loadSSHOracle(t)
	for i, raw := range spec.Arguments {
		target := targetJSON(t, raw)
		args, err := SSHArguments(target)
		var got any
		if err != nil {
			got = errorOutcome(err)
		} else {
			got = map[string]any{"argv": args}
		}
		t.Run(fmt.Sprint(i), func(t *testing.T) { compareJSON(t, string(raw), got, oracle.Arguments[i]) })
	}
}
