//go:build !windows

package providers

import "syscall"

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
