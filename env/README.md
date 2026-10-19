# github.com/sky-valley/pi/env

The Go port of [`@earendil-works/pi-env`](https://github.com/earendil-works/pi/tree/main/packages/env): remote
execution environments, where an agent's tools run on another machine, usually over SSH, while the agent, its storage
and credentials stay local.

- `pi-env` (`cmd/pi-env`, `daemon/`): a small program that runs on the remote machine. It speaks a framed protocol on
  stdin and stdout ([docs/protocol.md](docs/protocol.md)) and performs file operations and commands there. Upstream's
  daemon is written in Rust; this one is Go, and both pass upstream's test suite.
- `RemoteExecutionEnv`: an execution environment (`coding.ExecutionEnv`, plus the rest of Durable's `ExecutionEnv` as
  methods) that talks to the daemon through a `Connection`. Its results match `NodeExecutionEnv` running on the remote
  machine; only error messages may differ ([docs/semantics.md](docs/semantics.md)).

```go
target := env.SSHTarget{Host: "gpu-box", KnownHostsFile: "/data/ssh/known_hosts", HostKeyAlias: "pi-env-gpu"}
// Once: show the host's key fingerprint to the owner, who compares it out of band and accepts it.
scan, err := env.ScanHostKey(ctx, target)
err = env.AcceptHostKey(target, scan.Lines)

// Detects the remote system, deploys the daemon if missing (named by its SHA-256 and verified before every start),
// and returns a connection that starts it over ssh.
connection, remote, err := env.ConnectSSH(ctx, env.SSHConnectOptions{SSHTarget: target})
remoteEnv := env.NewRemoteExecutionEnv(env.RemoteExecutionEnvOptions{Connection: connection, ID: "pi-env:gpu", Cwd: "/home/me/project"})

// Or lazily: nothing happens until the first operation, which detects, deploys and connects. A failure (no network,
// untrusted host key) is that operation's error, and the next operation tries again.
lazy, detected := env.SSHConnection(env.SSHConnectOptions{SSHTarget: target})
lazyEnv := env.NewRemoteExecutionEnv(env.RemoteExecutionEnvOptions{Connection: lazy, ID: "pi-env:gpu", Cwd: "/home/me/project"})
```

The package embeds the daemon for every supported remote system (`bin/`, gzipped, built by `go generate`), and deploys
the uncompressed binary. `ssh` runs with `BatchMode`, strict host-key checking against the application's own
known-hosts file under a fixed alias, no forwarding of any kind, no shared connections or configured commands, and
without forwarding the local locale. On Windows, detection and deployment go through PowerShell, and the daemon starts
through the server's default shell (cmd.exe or PowerShell).

`ListFiles(ctx, path, options)` lists the tree below a directory in the daemon, as `fd --hidden --exclude .git` does
without following symbolic links: files, directories and links as relative `/` paths, skipping what git ignores inside
work trees, bounded by `MaxEntries`, `MaxBytes` and `TimeoutMS` with a `Truncated` flag, and stopped by ending the
context. A daemon released before it reports `not_supported` (`hello.features` lacks `listFiles`), and callers fall
back to their own walk. For local sessions, `LocalExecutionEnv` is the local environment (`coding.LocalEnv`) plus
`ListFiles` through a daemon on the same machine:

```go
daemon, err := env.LocalDaemon() // the embedded daemon for this machine, written to the user's cache directory
local := env.NewConnection(env.ConnectionOptions{Command: []string{daemon}}) // owned by the caller; Close it at shutdown
localEnv := env.NewLocalExecutionEnv(env.LocalExecutionEnvOptions{Cwd: "/home/me/project", Connection: local})
listing, err := localEnv.ListFiles(ctx, ".", &env.ListFilesOptions{Exclude: []string{"node_modules"}})
// listing.Entries: [{Path: "src", Kind: "directory"}, {Path: "src/a.go", Kind: "file"}], listing.Truncated
```

`NodePlatform(info)` gives the remote system of `connection.Info(ctx)` in Node's names (`win32`, `darwin`, `x64`,
`arm64`), as `process.platform` and `process.arch` spell them.

`OpenTerminal(ctx, connection, options)` runs a program, by default the user's login shell, in a pseudo-terminal of the
daemon: raw output, input, resizing and its exit code, for interactive terminals. `LocalDaemon()` is the daemon for
this machine, for terminals without SSH. Terminals are not supported on Windows yet.

Supported remote systems: Linux, macOS, Android (Termux) and Windows, on x86-64 and arm64. On Windows, string commands
run through Git Bash as `NodeExecutionEnv` runs them there; argv commands run directly.

## Development

The env module is its own Go module; work on it together with the root module through a workspace (`go.work` with
both) or with `GOWORK=off` against the published root.

`go generate ./...` in `env` builds the eight daemons reproducibly (`internal/daemonsource`: `-trimpath`, no VCS stamp
or build id, cgo off, the module's own `go.mod`) into `bin/` with their manifest. Run it whenever the daemon's source
changes: `TestEmbeddedDaemonsAreCurrent` fails until then, and `TestEmbeddedDaemonsRebuildIdentically` rebuilds this
machine's daemon and compares it byte for byte when the toolchain is the one that built them.

The client's tests build the daemon from `./cmd/pi-env`, or use the binary named by `PI_ENV_DAEMON` (the Rust daemon of
`@earendil-works/pi-env`, to check the client against it). They run Durable's env conformance suite against
`RemoteExecutionEnv`, compare random operation sequences with `NodeExecutionEnv`'s results, compare the frames the
client sends and the ssh calls it makes with upstream's TypeScript client (`testdata/*/capture.mts` record them), and
deploy and run the daemon over a disposable `sshd` on localhost when one is installed.
