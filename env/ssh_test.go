package env

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Ported from upstream's packages/env/test/ssh.test.ts (d2e7d8b7c): a
// disposable sshd on localhost with its own host key, client key and home
// directory, reached through the system ssh.

type sshFixture struct {
	root, home string
	target     SSHTarget
	// daemon is the binary the cases deploy, and deployedName its remote name.
	daemon       string
	deployedName string
}

func findSSHD() string {
	if path, err := exec.LookPath("sshd"); err == nil {
		return path
	}
	if _, err := os.Stat("/usr/sbin/sshd"); err == nil {
		return "/usr/sbin/sshd"
	}
	return ""
}

func freePort(t *testing.T) int {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	mustDo(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return port
}

func startSSHD(t *testing.T) *sshFixture {
	t.Helper()
	sshd := findSSHD()
	if runtime.GOOS == "windows" || sshd == "" {
		t.Skip("no sshd to test against")
	}
	for _, program := range []string{"ssh", "ssh-keygen"} {
		if _, err := exec.LookPath(program); err != nil {
			t.Skipf("no %s", program)
		}
	}
	root := tempDir(t, "pi-env-ssh-")
	home := filepath.Join(root, "home")
	mustDo(t, os.MkdirAll(home, 0o755))
	keygen := func(name string) {
		out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", filepath.Join(root, name)).CombinedOutput()
		if err != nil {
			t.Fatalf("ssh-keygen: %v %s", err, out)
		}
	}
	keygen("host_key")
	keygen("client_key")
	clientKey, err := os.ReadFile(filepath.Join(root, "client_key.pub"))
	mustDo(t, err)
	mustDo(t, os.WriteFile(filepath.Join(root, "authorized_keys"), clientKey, 0o600))
	port := freePort(t)
	config := strings.Join([]string{
		"Port " + itoa(port),
		"ListenAddress 127.0.0.1",
		"HostKey " + filepath.Join(root, "host_key"),
		"AuthorizedKeysFile " + filepath.Join(root, "authorized_keys"),
		"PidFile " + filepath.Join(root, "sshd.pid"),
		"PasswordAuthentication no",
		"KbdInteractiveAuthentication no",
		"StrictModes no",
		"SetEnv HOME=" + home,
		"",
	}, "\n")
	mustDo(t, os.WriteFile(filepath.Join(root, "sshd_config"), []byte(config), 0o644))
	mustDo(t, os.WriteFile(filepath.Join(root, "ssh_config"), nil, 0o644))
	server := exec.Command(sshd, "-D", "-e", "-f", filepath.Join(root, "sshd_config"))
	mustDo(t, server.Start())
	t.Cleanup(func() {
		_ = server.Process.Kill()
		_ = server.Wait()
	})
	waitFor(t, 5*time.Second, "sshd to listen", func() bool {
		conn, err := net.Dial("tcp", "127.0.0.1:"+itoa(port))
		if err == nil {
			conn.Close()
		}
		return err == nil
	})
	me, err := user.Current()
	mustDo(t, err)
	binary, err := os.ReadFile(testDaemon)
	mustDo(t, err)
	sum := sha256.Sum256(binary)
	return &sshFixture{
		root: root, home: home,
		target: SSHTarget{
			Host: "127.0.0.1", Port: port, User: me.Username,
			IdentityFile:   filepath.Join(root, "client_key"),
			KnownHostsFile: filepath.Join(root, "known_hosts"),
			HostKeyAlias:   "pi-env-test",
			ConfigFile:     filepath.Join(root, "ssh_config"),
		},
		daemon:       testDaemon,
		deployedName: "pi-env-" + hex.EncodeToString(sum[:])[:32],
	}
}

func (f *sshFixture) options() SSHConnectOptions {
	return SSHConnectOptions{SSHTarget: f.target, Binary: f.daemon}
}

func (f *sshFixture) tools() string { return filepath.Join(f.home, ".pi/mobile/tools") }

func sameFile(t *testing.T, a, b string) bool {
	t.Helper()
	x, err := os.ReadFile(a)
	mustDo(t, err)
	y, err := os.ReadFile(b)
	mustDo(t, err)
	return bytes.Equal(x, y)
}

func TestSSHBootstrap(t *testing.T) {
	f := startSSHD(t)
	ctx := background()

	t.Run("refuses an untrusted host, then deploys and runs the daemon once its key is accepted", func(t *testing.T) {
		var unknown *HostKeyUnknownError
		if _, _, err := ConnectSSH(ctx, f.options()); !errors.As(err, &unknown) {
			t.Fatalf("untrusted host: %#v", err)
		}
		scanned, err := ScanHostKey(ctx, f.target)
		mustDo(t, err)
		fingerprint, err := exec.Command("ssh-keygen", "-lf", filepath.Join(f.root, "host_key.pub")).Output()
		mustDo(t, err)
		if expected := strings.Split(string(fingerprint), " ")[1]; !strings.Contains(strings.Join(scanned.Fingerprints, "\n"), expected) {
			t.Fatalf("fingerprints %q lack %s", scanned.Fingerprints, expected)
		}
		mustDo(t, AcceptHostKey(f.target, scanned.Lines))

		mustDo(t, os.MkdirAll(f.tools(), 0o755))
		mustDo(t, os.WriteFile(filepath.Join(f.tools(), "pi-env-0123456789abcdef0123456789abcdef"), []byte("old"), 0o644))
		connection, remote, err := ConnectSSH(ctx, f.options())
		mustDo(t, err)
		defer connection.Close()
		if remote.Home != f.home {
			t.Fatalf("home %q, want %q", remote.Home, f.home)
		}
		if !sameFile(t, filepath.Join(f.tools(), f.deployedName), f.daemon) {
			t.Fatal("the deployed daemon differs")
		}
		// Daemons of other contents are removed.
		entries, err := os.ReadDir(f.tools())
		mustDo(t, err)
		var names []string
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "pi-env-") {
				names = append(names, entry.Name())
			}
		}
		if !reflect.DeepEqual(names, []string{f.deployedName}) {
			t.Fatalf("tools %v", names)
		}
		env := NewRemoteExecutionEnv(RemoteExecutionEnvOptions{Connection: connection, ID: "pi-env:test", Cwd: f.home})
		mustDo(t, env.WriteFile(ctx, "over-ssh.txt", []byte("hello")))
		if text, err := env.ReadTextFile(ctx, "over-ssh.txt"); err != nil || text != "hello" {
			t.Fatalf("read %q %v", text, err)
		}
		var out, errOut strings.Builder
		result, err := env.RunProgram(ctx, []string{"sh", "-c", `printf "%s" "$HOME"`}, &RunOptions{OnOutput: collectInto(&out, &errOut)})
		if err != nil || result.ExitCode != 0 || out.String() != f.home {
			t.Fatalf("exec: %+v %v %q", result, err, out.String())
		}
	})

	t.Run("reuses a verified daemon and replaces a tampered one", func(t *testing.T) {
		file := filepath.Join(f.tools(), f.deployedName)
		before, err := os.Stat(file)
		mustDo(t, err)
		connection, _, err := ConnectSSH(ctx, f.options())
		mustDo(t, err)
		connection.Close()
		if after, err := os.Stat(file); err != nil || !after.ModTime().Equal(before.ModTime()) {
			t.Fatal("a verified daemon was deployed again")
		}
		mustDo(t, os.WriteFile(file, []byte("tampered"), 0o700))
		connection, _, err = ConnectSSH(ctx, f.options())
		mustDo(t, err)
		defer connection.Close()
		if !sameFile(t, file, f.daemon) {
			t.Fatal("the tampered daemon was kept")
		}
		if info, err := connection.Info(ctx); err != nil || info.Home != f.home {
			t.Fatalf("info %+v %v", info, err)
		}
	})

	t.Run("verifies the daemon again before starting it after a lost connection", func(t *testing.T) {
		connection, _, err := ConnectSSH(ctx, f.options())
		mustDo(t, err)
		defer connection.Close()
		file := filepath.Join(f.tools(), f.deployedName)
		info, err := connection.Info(ctx)
		mustDo(t, err)
		killPID(t, info.PID)
		time.Sleep(300 * time.Millisecond)
		// Linux refuses to write a running program's file.
		mustDo(t, os.WriteFile(file, []byte("tampered"), 0o700))
		// The next start redeploys the verified binary instead of running
		// whatever is there.
		again, err := connection.Info(ctx)
		if err != nil || again.PID == info.PID {
			t.Fatalf("restart: %+v %v", again, err)
		}
		if !sameFile(t, file, f.daemon) {
			t.Fatal("the tampered daemon was run")
		}
	})

	t.Run("lists files over SSH as the local environment lists them", func(t *testing.T) {
		tree := filepath.Join(f.home, "tree")
		for _, file := range []string{"a/b.txt", "a/c/d.txt", ".hidden", "out/x.js", "skip/y.txt", ".git/HEAD"} {
			mustDo(t, os.MkdirAll(filepath.Dir(filepath.Join(tree, file)), 0o755))
			mustDo(t, os.WriteFile(filepath.Join(tree, file), nil, 0o644))
		}
		mustDo(t, os.Mkdir(filepath.Join(tree, "empty"), 0o755))
		mustDo(t, os.WriteFile(filepath.Join(tree, ".gitignore"), []byte("out/\n"), 0o644))
		connection, _, err := ConnectSSH(ctx, f.options())
		mustDo(t, err)
		defer connection.Close()
		local := newConnection(t)
		remote := NewRemoteExecutionEnv(RemoteExecutionEnvOptions{Connection: connection, ID: "pi-env:test", Cwd: f.home})
		listing, err := remote.ListFiles(ctx, "tree", &ListFilesOptions{Exclude: []string{"skip"}})
		mustDo(t, err)
		want := FileListing{Entries: []FileListEntry{
			{".gitignore", "file"}, {".hidden", "file"}, {"a", "directory"}, {"a/b.txt", "file"},
			{"a/c", "directory"}, {"a/c/d.txt", "file"}, {"empty", "directory"},
		}}
		if !reflect.DeepEqual(listing, want) {
			t.Fatalf("remote listing %+v", listing)
		}
		env := NewLocalExecutionEnv(LocalExecutionEnvOptions{Connection: local, Cwd: f.home})
		if got, err := env.ListFiles(ctx, "tree", &ListFilesOptions{Exclude: []string{"skip"}}); err != nil || !reflect.DeepEqual(got, listing) {
			t.Fatalf("local listing %+v %v", got, err)
		}
	})

	t.Run("starts the daemon through the login shell only when asked", func(t *testing.T) {
		// The login shell is the account's shell: sh and bash read .profile,
		// zsh (the macOS default) reads .zprofile.
		mustDo(t, os.WriteFile(filepath.Join(f.home, ".profile"), []byte("export PI_ENV_LOGIN=yes\n"), 0o644))
		mustDo(t, os.WriteFile(filepath.Join(f.home, ".zprofile"), []byte("export PI_ENV_LOGIN=yes\n"), 0o644))
		login := func(loginShell bool) string {
			options := f.options()
			options.LoginShell = loginShell
			connection, _, err := ConnectSSH(ctx, options)
			mustDo(t, err)
			defer connection.Close()
			env := NewRemoteExecutionEnv(RemoteExecutionEnvOptions{Connection: connection, ID: "pi-env:test", Cwd: f.home})
			var out, errOut strings.Builder
			_, err = env.RunProgram(ctx, []string{"sh", "-c", `printf "%s" "$PI_ENV_LOGIN"`}, &RunOptions{OnOutput: collectInto(&out, &errOut)})
			mustDo(t, err)
			return out.String()
		}
		if got := login(false); got != "" {
			t.Fatalf("without a login shell: %q", got)
		}
		if got := login(true); got != "yes" {
			t.Fatalf("with a login shell: %q", got)
		}
	})

	t.Run("connects lazily and reports a failed start as the error of the operation", func(t *testing.T) {
		lazyTarget := f.target
		lazyTarget.KnownHostsFile = filepath.Join(f.root, "lazy_known_hosts")
		connection, remote := SSHConnection(SSHConnectOptions{SSHTarget: lazyTarget, Binary: f.daemon})
		defer connection.Close()
		if remote() != nil {
			t.Fatal("remote known before the first start")
		}
		env := NewRemoteExecutionEnv(RemoteExecutionEnvOptions{Connection: connection, ID: "pi-env:test", Cwd: f.home})
		// Nothing is trusted yet: each operation fails with the reason and
		// tries again.
		if _, err := env.ReadTextFile(ctx, "missing.txt"); err == nil || !strings.Contains(err.Error(), "not trusted") {
			t.Fatalf("untrusted read: %v", err)
		}
		if _, err := env.RunProgram(ctx, []string{"sh", "-c", "exit 0"}, nil); errorCode(err) != "spawn_error" {
			t.Fatalf("untrusted exec: %v", err)
		}
		scanned, err := ScanHostKey(ctx, lazyTarget)
		mustDo(t, err)
		mustDo(t, AcceptHostKey(lazyTarget, scanned.Lines))
		if result, err := env.RunProgram(ctx, []string{"sh", "-c", "exit 0"}, nil); err != nil || result.ExitCode != 0 {
			t.Fatalf("trusted exec: %+v %v", result, err)
		}
		if detected := remote(); detected == nil || detected.Home != f.home {
			t.Fatalf("remote %+v", detected)
		}
	})

	t.Run("accepts only host keys for the alias and never replaces a trusted key silently", func(t *testing.T) {
		knownHosts := filepath.Join(f.root, "accept_known_hosts")
		scoped := f.target
		scoped.KnownHostsFile = knownHosts
		key := func(name string) string {
			file := filepath.Join(f.root, name)
			out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", file).CombinedOutput()
			if err != nil {
				t.Fatalf("ssh-keygen: %v %s", err, out)
			}
			public, err := os.ReadFile(file + ".pub")
			mustDo(t, err)
			return strings.Join(strings.Split(string(public), " ")[:2], " ")
		}
		first, second := key("accept_first"), key("accept_second")
		if err := AcceptHostKey(scoped, []string{"other-alias " + first}); err == nil {
			t.Fatal("accepted a line for another alias")
		}
		if err := AcceptHostKey(scoped, []string{"@cert-authority pi-env-test " + first}); err == nil {
			t.Fatal("accepted a marker line")
		}
		done := make(chan error, 2)
		for i := 0; i < 2; i++ {
			go func() { done <- AcceptHostKey(scoped, []string{"pi-env-test " + first}) }()
		}
		mustDo(t, <-done)
		mustDo(t, <-done)
		if content, _ := os.ReadFile(knownHosts); string(content) != "pi-env-test "+first+"\n" {
			t.Fatalf("known hosts %q", content)
		}
		var changed *HostKeyChangedError
		if err := AcceptHostKey(scoped, []string{"pi-env-test " + second}); !errors.As(err, &changed) {
			t.Fatalf("changed key: %#v", err)
		}
		mustDo(t, ForgetHostKey(scoped))
		mustDo(t, AcceptHostKey(scoped, []string{"pi-env-test " + second}))
		if content, _ := os.ReadFile(knownHosts); string(content) != "pi-env-test "+second+"\n" {
			t.Fatalf("known hosts %q", content)
		}
	})

	t.Run("refuses a changed host key", func(t *testing.T) {
		other := filepath.Join(f.root, "other_key")
		if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", other).CombinedOutput(); err != nil {
			t.Fatalf("ssh-keygen: %v %s", err, out)
		}
		public, err := os.ReadFile(other + ".pub")
		mustDo(t, err)
		knownHosts := filepath.Join(f.root, "changed_known_hosts")
		mustDo(t, os.WriteFile(knownHosts, []byte("pi-env-test "+strings.Join(strings.Split(string(public), " ")[:2], " ")+"\n"), 0o600))
		options := f.options()
		options.KnownHostsFile = knownHosts
		var changed *HostKeyChangedError
		if _, _, err := ConnectSSH(ctx, options); !errors.As(err, &changed) {
			t.Fatalf("changed host key: %#v", err)
		}
	})

	t.Run("deploys and runs the embedded daemon for this machine", func(t *testing.T) {
		// Not in upstream's test, which deploys the daemon it built: the
		// daemon a Go program ships is the embedded one.
		local, err := localPlatform()
		mustDo(t, err)
		embedded, err := PackagedDaemon(local)
		mustDo(t, err)
		sum := sha256.Sum256(embedded)
		options := f.options()
		options.Binary = ""
		connection, remote, err := ConnectSSH(ctx, options)
		mustDo(t, err)
		defer connection.Close()
		file := filepath.Join(f.tools(), "pi-env-"+hex.EncodeToString(sum[:])[:32])
		deployed, err := os.ReadFile(file)
		if err != nil || !bytes.Equal(deployed, embedded) {
			t.Fatalf("the embedded daemon was not deployed to %s: %v", file, err)
		}
		if remote.Platform != local.Platform || remote.Arch != local.Arch {
			t.Fatalf("remote %+v", remote)
		}
		info, err := connection.Info(ctx)
		mustDo(t, err)
		if info.Version == "" || !info.hasFeature("listFiles") {
			t.Fatalf("hello %+v", info)
		}
		// The other contents' daemon is gone.
		if _, err := os.Stat(filepath.Join(f.tools(), f.deployedName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the previous daemon is still there: %v", err)
		}
	})
}
