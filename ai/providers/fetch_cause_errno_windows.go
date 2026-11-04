package providers

import "syscall"

// errnoCodes names the socket errors undiciCause renders as Node's codes. On
// Windows a socket fails with a Winsock or Win32 error, which libuv maps to
// the POSIX code Node reports; syscall's ECONNREFUSED and kin are Go's own
// invented values there, which no socket call returns.
var errnoCodes = map[syscall.Errno]string{
	10061: "ECONNREFUSED",  // WSAECONNREFUSED
	1225:  "ECONNREFUSED",  // ERROR_CONNECTION_REFUSED
	10054: "ECONNRESET",    // WSAECONNRESET
	64:    "ECONNRESET",    // ERROR_NETNAME_DELETED
	10053: "ECONNABORTED",  // WSAECONNABORTED
	1236:  "ECONNABORTED",  // ERROR_CONNECTION_ABORTED
	10060: "ETIMEDOUT",     // WSAETIMEDOUT
	10065: "EHOSTUNREACH",  // WSAEHOSTUNREACH
	1232:  "EHOSTUNREACH",  // ERROR_HOST_UNREACHABLE
	10051: "ENETUNREACH",   // WSAENETUNREACH
	1231:  "ENETUNREACH",   // ERROR_NETWORK_UNREACHABLE
	10049: "EADDRNOTAVAIL", // WSAEADDRNOTAVAIL
}
