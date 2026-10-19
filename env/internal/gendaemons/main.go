// Command gendaemons builds the eight pi-env daemons and writes them,
// compressed, to env/bin with their manifest, for the env package to embed.
// Run it as `go generate ./...` in env (daemons.go holds the directive)
// whenever the daemon's source changes; env's tests fail while the embedded
// daemons are stale.
package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sky-valley/pi/env/internal/daemonsource"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gendaemons:", err)
		os.Exit(1)
	}
}

func run() error {
	moduleDir, err := os.Getwd()
	if err != nil {
		return err
	}
	if module, err := os.ReadFile(filepath.Join(moduleDir, "go.mod")); err != nil || !bytes.Contains(module, []byte("module github.com/sky-valley/pi/env\n")) {
		return fmt.Errorf("run me in the env module's root (go generate ./... in env), not %s", moduleDir)
	}
	binDir := filepath.Join(moduleDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}
	work, err := os.MkdirTemp("", "pi-env-gendaemons-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)

	goVersion, err := daemonsource.GoVersion(moduleDir)
	if err != nil {
		return err
	}
	source, err := daemonsource.Digest(moduleDir)
	if err != nil {
		return err
	}
	manifest := daemonsource.Manifest{Go: goVersion, Source: source, Daemons: map[string]daemonsource.Daemon{}}
	keep := map[string]bool{"manifest.json": true}
	for _, target := range daemonsource.Targets {
		out := filepath.Join(work, target.Name())
		if err := daemonsource.Build(moduleDir, target, out); err != nil {
			return err
		}
		binary, err := os.ReadFile(out)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(binary)
		manifest.Daemons[target.Name()] = daemonsource.Daemon{SHA256: hex.EncodeToString(sum[:]), Size: len(binary)}
		compressed, err := compress(binary)
		if err != nil {
			return err
		}
		name := target.Name() + ".gz"
		keep[name] = true
		if err := os.WriteFile(filepath.Join(binDir, name), compressed, 0o644); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "%s: %d bytes, %d compressed\n", target.Name(), len(binary), len(compressed))
	}
	entries, err := os.ReadDir(binDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !keep[entry.Name()] {
			if err := os.Remove(filepath.Join(binDir, entry.Name())); err != nil {
				return err
			}
		}
	}
	names := make([]string, 0, len(manifest.Daemons))
	for name := range manifest.Daemons {
		names = append(names, name)
	}
	sort.Strings(names)
	data, err := json.MarshalIndent(manifest, "", "\t")
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "built %s with %s, source %s\n", strings.Join(names, ", "), goVersion, source[:12])
	return os.WriteFile(filepath.Join(binDir, "manifest.json"), append(data, '\n'), 0o644)
}

// compress gzips deterministically: no name, no time, the same level.
func compress(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	w, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
