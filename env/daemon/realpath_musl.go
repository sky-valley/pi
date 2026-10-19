//go:build linux

package daemon

import (
	"bytes"

	"golang.org/x/sys/unix"
)

const (
	// muslPathMax and muslSymloopMax are musl's PATH_MAX and SYMLOOP_MAX.
	muslPathMax    = 4096
	muslSymloopMax = 40
)

// realpathMusl is musl's realpath(3) (src/misc/realpath.c, the version
// without /proc), which the Rust daemon's static Linux builds call: the
// path's components are popped off a stack one at a time; a link's contents
// are pushed back onto it, a component that is not a link moves to the
// output. Unlike glibc it keeps exactly two leading slashes ("//x"), fails
// with ELOOP at the 40th link rather than the 41st, and checks that a
// component followed by "/", "/." or "/.." is a directory by reading it as a
// link with the slash. It is a line-for-line port, indices included.
func realpathMusl(filename string) (string, error) {
	var stack [muslPathMax + 1]byte
	var output [muslPathMax]byte
	l := min(len(filename), len(stack))
	if i := bytes.IndexByte([]byte(filename[:l]), 0); i >= 0 {
		l = i
	}
	if l == 0 {
		return "", unix.ENOENT
	}
	if l >= muslPathMax {
		return "", unix.ENAMETOOLONG
	}
	p := len(stack) - l - 1
	q := 0
	copy(stack[p:], filename[:l])
	stack[p+l] = 0
	cnt, nup := 0, 0
	var checkDir byte
	// at is stack[i], NUL past the end as in C.
	at := func(i int) byte { return stack[i] }
	slashLen := func(i int) int {
		n := 0
		for at(i+n) == '/' {
			n++
		}
		return n
	}
restart:
	for ; ; p += slashLen(p) {
		// A component starting with / is / or //, and resets the output.
		if at(p) == '/' {
			checkDir, nup, q = 0, 0, 0
			output[q] = '/'
			q++
			p++
			// Initial // is special.
			if at(p) == '/' && at(p+1) != '/' {
				output[q] = '/'
				q++
			}
			continue
		}
		z := p
		for at(z) != 0 && at(z) != '/' {
			z++
		}
		l0 := z - p
		l = l0
		if l == 0 && checkDir == 0 {
			break
		}
		// Skip any . component but keep checkDir.
		if l == 1 && at(p) == '.' {
			p += l
			continue
		}
		// Copy the component onto the output to read it as a link; the output
		// position moves past it only once it is known not to be one.
		if q > 0 && output[q-1] != '/' {
			if p == 0 {
				return "", unix.ENAMETOOLONG
			}
			p--
			stack[p] = '/'
			l++
		}
		if q+l >= muslPathMax {
			return "", unix.ENAMETOOLONG
		}
		copy(output[q:], stack[p:p+l])
		p += l
		up := false
		if l0 == 2 && at(p-2) == '.' && at(p-1) == '.' {
			up = true
			// Components ".." could cancel start after nup "../"; with none,
			// ".." accumulates for the working directory.
			if q <= 3*nup {
				nup++
				q += l
				continue
			}
			// After components known to be directories, ".." needs no
			// readlink.
			if checkDir == 0 {
				goto skipReadlink
			}
		}
		{
			k, err := unix.Readlink(string(output[:q+l]), stack[:p])
			if err == nil && k == p {
				return "", unix.ENAMETOOLONG
			}
			if err == nil && k == 0 {
				return "", unix.ENOENT
			}
			if err != nil {
				if err != unix.EINVAL {
					return "", err
				}
				goto skipReadlink
			}
			cnt++
			if cnt == muslSymloopMax {
				return "", unix.ELOOP
			}
			// Link contents ending in / drop the slashes already on the stack,
			// so / does not become // and // not ///.
			if stack[k-1] == '/' {
				for at(p) == '/' {
					p++
				}
			}
			p -= k
			copy(stack[p:], stack[:k])
			goto restart
		}
	skipReadlink:
		checkDir = 0
		if up {
			for q > 0 && output[q-1] != '/' {
				q--
			}
			if q > 1 && (q > 2 || output[0] != '/') {
				q--
			}
			continue
		}
		if l0 > 0 {
			q += l
		}
		checkDir = at(p)
	}
	if q == 0 || output[0] != '/' {
		cwd, err := unix.Getwd()
		if err != nil {
			return "", err
		}
		l = len(cwd)
		// Cancel any initial ".." components.
		p = 0
		for ; nup > 0; nup-- {
			for l > 1 && cwd[l-1] != '/' {
				l--
			}
			if l > 1 {
				l--
			}
			p += 2
			if p < q {
				p++
			}
		}
		prefix := cwd[:l]
		if q-p > 0 && prefix[l-1] != '/' {
			prefix += "/"
		}
		if len(prefix)+(q-p)+1 >= muslPathMax {
			return "", unix.ENAMETOOLONG
		}
		return prefix + string(output[p:q]), nil
	}
	return string(output[:q]), nil
}
