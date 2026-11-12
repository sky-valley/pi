package coding

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// LocalEnv.ListDir leaves out only a child removed between the read and the
// stat; any other stat error fails the listing, as NodeExecutionEnv.listDir
// does (upstream 75ca6e7d0). The failure here is ENAMETOOLONG: the directory
// path fits in PATH_MAX, the path of its child does not.
func TestLocalListDirFailsOnStatErrorsOtherThanNotFound(t *testing.T) {
	dir := t.TempDir()
	component := strings.Repeat("d", 200)
	fd, err := syscall.Open(dir, syscall.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	deep := dir
	for len(deep)+1+len(component) < 4000 {
		if err := syscall.Mkdirat(fd, component, 0o755); err != nil {
			t.Fatal(err)
		}
		next, err := syscall.Openat(fd, component, syscall.O_RDONLY|syscall.O_DIRECTORY, 0)
		syscall.Close(fd)
		if err != nil {
			t.Fatal(err)
		}
		fd, deep = next, deep+"/"+component
	}
	child, err := syscall.Openat(fd, strings.Repeat("f", 200), syscall.O_CREAT|syscall.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	syscall.Close(child)
	syscall.Close(fd)
	t.Cleanup(func() { removeDeep(dir, component) })

	_, err = NewLocalEnv(dir).ListDir(context.Background(), deep)
	if !errors.Is(err, syscall.ENAMETOOLONG) {
		t.Fatalf("ListDir = %v, want ENAMETOOLONG", err)
	}
}

// removeDeep removes a tree too deep for os.RemoveAll's paths by renaming
// its top directory level by level up to dir.
func removeDeep(dir, component string) {
	for {
		top := filepath.Join(dir, component)
		inner := filepath.Join(top, component)
		if _, err := os.Lstat(inner); err != nil {
			os.RemoveAll(dir)
			return
		}
		tmp := filepath.Join(dir, "lift")
		os.Rename(inner, tmp)
		os.RemoveAll(top)
		os.Rename(tmp, top)
	}
}
