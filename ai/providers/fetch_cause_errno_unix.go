//go:build !windows && !plan9

package providers

import (
	"errors"
	"syscall"
)

// errnoCodes names the socket errors undiciCause renders as Node's codes.
var errnoCodes = map[syscall.Errno]string{
	syscall.ECONNREFUSED:  "ECONNREFUSED",
	syscall.ECONNRESET:    "ECONNRESET",
	syscall.ECONNABORTED:  "ECONNABORTED",
	syscall.ETIMEDOUT:     "ETIMEDOUT",
	syscall.EHOSTUNREACH:  "EHOSTUNREACH",
	syscall.ENETUNREACH:   "ENETUNREACH",
	syscall.EADDRNOTAVAIL: "EADDRNOTAVAIL",
	syscall.EPIPE:         "EPIPE",
}

// errnoCode is Node's code for the socket error in err's chain, if it names
// one.
func errnoCode(err error) (string, bool) {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return "", false
	}
	code, known := errnoCodes[errno]
	return code, known
}
