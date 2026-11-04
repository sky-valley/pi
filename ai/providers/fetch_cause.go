package providers

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The errors below stand for the JS errors pi finds a cause on when a request
// fails before, or while, its response arrives (withCauseDetail, upstream
// 4c992154d): the SDKs' APIConnectionError, whose cause is the rejected fetch
// (and openai's APIConnectionTimeoutError, which keeps it too), and undici's
// TypeErrors "fetch failed" and "terminated", whose cause is the socket's
// error. For the port's own client, the stand-in for undici's fetch, that last
// cause is net/http's failure rendered as Node words it (undiciCause).

// sdkConnectionError is an SDK's error for a rejected fetch (sdkFetchError,
// openaiFetchError): err is its message's sentinel — errAPIConnection,
// errAPIConnectionTimeout or errOpenAIHeadersTimeout — and cause the
// rejection (fetchRejection.cause).
type sdkConnectionError struct{ err, cause error }

func (e *sdkConnectionError) Error() string { return e.err.Error() }

func (e *sdkConnectionError) Is(target error) bool { return target == e.err }

func (e *sdkConnectionError) jsCause() error { return e.cause }

// fetchFailedError is undici's TypeError "fetch failed", which fetch rejects
// with when a request sent through the port's own client gets no response
// (undiciFetchError), or that undici refuses to read: err is the failure, and
// target the request's URL.
type fetchFailedError struct {
	err    error
	target string
}

func (e *fetchFailedError) Error() string { return "fetch failed" }

func (e *fetchFailedError) jsCause() error { return undiciCause(e.err, e.target) }

// errFetchAborted and errFetchTimedOut are the DOMExceptions a fetch rejects
// with when its signal aborts: an AbortError (code 20), the reason a bare
// abort() gives, as the SDKs' own timeoutMs timer does; and a TimeoutError
// (code 23), AbortSignal.timeout's. A custom HTTPClient's context.Canceled
// and context.DeadlineExceeded stand for them.
var (
	errFetchAborted  = &nodeSystemError{errOperationAborted.Error(), "20"}
	errFetchTimedOut = &nodeSystemError{"The operation was aborted due to timeout", "23"}
)

// terminatedError is undici's TypeError "terminated", which errors a response
// body's stream when the connection fails mid-body (fetchBody): err is the
// read's error.
type terminatedError struct{ err error }

func (e *terminatedError) Error() string { return errTerminated.Error() }

func (e *terminatedError) Is(target error) bool { return target == errTerminated }

func (e *terminatedError) jsCause() error { return undiciCause(e.err, "") }

// nodeSystemError is a Node socket, DNS or TLS error, or an undici one, as
// undici hands it to fetch's TypeError for a cause: its message and its code.
type nodeSystemError struct{ message, code string }

func (e *nodeSystemError) Error() string { return e.message }

// undiciConnectTimeoutMs is undici's connect timeout, which bounds the DNS
// lookup, the TCP connect and the TLS handshake together.
const undiciConnectTimeoutMs = 10_000

// undiciCause is the cause undici gives fetch's TypeError for err, a failure
// of the port's own client to the request URL target ("" for a body read): the
// error Node or undici raises for the same failure, as each was measured in
// node v22 (undici 6.29.1). A failure Node words in a way net/http's error
// does not carry — a malformed response (undici's HTTPParserError), an HTTPS
// request answered in plain HTTP (OpenSSL's text), a certificate that does not
// match the host — is err itself, so its own chain gives the detail.
func undiciCause(err error, target string) error {
	if timeout := undiciTimeout(err, target); timeout != nil {
		return timeout
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		switch {
		case dns.IsNotFound:
			return &nodeSystemError{"getaddrinfo ENOTFOUND " + dns.Name, "ENOTFOUND"}
		case dns.IsTimeout || dns.IsTemporary:
			return &nodeSystemError{"getaddrinfo EAI_AGAIN " + dns.Name, "EAI_AGAIN"}
		}
		return err
	}
	var op *net.OpError
	var errno syscall.Errno
	if errors.As(err, &op) && errors.As(op.Err, &errno) {
		code, known := errnoCodes[errno]
		switch {
		case !known:
		case op.Op == "dial" && op.Addr != nil:
			// Node's address is the bare IP, an IPv6 one unbracketed.
			if host, port, splitErr := net.SplitHostPort(op.Addr.String()); splitErr == nil {
				return &nodeSystemError{"connect " + code + " " + host + ":" + port, code}
			}
		case op.Op == "read" || op.Op == "write":
			return &nodeSystemError{op.Op + " " + code, code}
		}
		return err
	}
	// The server closed the connection before the response, or short of the
	// body the head promised.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return &nodeSystemError{"other side closed", "UND_ERR_SOCKET"}
	}
	// A body undici decodes fails in zlib's words.
	switch {
	case errors.Is(err, gzip.ErrHeader) || errors.Is(err, zlib.ErrHeader):
		return &nodeSystemError{"incorrect header check", "Z_DATA_ERROR"}
	case errors.Is(err, gzip.ErrChecksum) || errors.Is(err, zlib.ErrChecksum):
		return &nodeSystemError{"incorrect data check", "Z_DATA_ERROR"}
	}
	// A header value undici's client refuses, which net/http refuses too.
	if m := invalidHeaderValue.FindStringSubmatch(err.Error()); m != nil {
		if name, unquoteErr := strconv.Unquote(m[1]); unquoteErr == nil {
			return &nodeSystemError{"invalid " + strings.ToLower(name) + " header", "UND_ERR_INVALID_ARG"}
		}
	}
	var verify *tls.CertificateVerificationError
	if errors.As(err, &verify) {
		if cert := nodeCertificateError(verify); cert != nil {
			return cert
		}
	}
	return err
}

// invalidHeaderValue is net/http's refusal of a header value, naming the
// header in Go's quoting.
var invalidHeaderValue = regexp.MustCompile(`^net/http: invalid header field value for ("(?:[^"\\]|\\.)*")$`)

// undiciTimeout is undici's error for err when it is one of the timeouts
// undici has: its headers timeout (net/http's ResponseHeaderTimeout), and
// its connect timeout, for a dial or TLS handshake that timed out. net/http
// words neither of its own as an exported error, so its text tells them.
func undiciTimeout(err error, target string) error {
	var op *net.OpError
	switch {
	case strings.Contains(err.Error(), "timeout awaiting response headers"):
		return &nodeSystemError{"Headers Timeout Error", "UND_ERR_HEADERS_TIMEOUT"}
	case errors.As(err, &op) && op.Op == "dial" && op.Timeout(),
		strings.Contains(err.Error(), "TLS handshake timeout"):
		u, parseErr := url.Parse(target)
		if parseErr != nil || u.Host == "" {
			return nil
		}
		// undici's hostname is the URL's, an IPv6 one in brackets, and its
		// port the scheme's default when the URL names none.
		host, port := u.Hostname(), u.Port()
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		if port == "" {
			port = "80"
			if u.Scheme == "https" {
				port = "443"
			}
		}
		return &nodeSystemError{
			"Connect Timeout Error (attempted address: " + host + ":" + port + ", timeout: " + strconv.Itoa(undiciConnectTimeoutMs) + "ms)",
			"UND_ERR_CONNECT_TIMEOUT",
		}
	}
	return nil
}

// nodeCertificateError is OpenSSL's verify error, as Node's TLS raises it, for
// the certificate failures crypto/x509 reports alike: an unknown authority,
// told apart by the chain the server sent, and a certificate outside its
// validity period. Any other is nil.
func nodeCertificateError(verify *tls.CertificateVerificationError) error {
	var unknown x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	chain := verify.UnverifiedCertificates
	switch {
	case errors.As(verify.Err, &unknown) && len(chain) > 0:
		selfIssued := func(c *x509.Certificate) bool { return bytes.Equal(c.RawSubject, c.RawIssuer) }
		switch {
		case selfIssued(chain[0]):
			return &nodeSystemError{"self-signed certificate", "DEPTH_ZERO_SELF_SIGNED_CERT"}
		case slices.ContainsFunc(chain[1:], selfIssued):
			return &nodeSystemError{"self-signed certificate in certificate chain", "SELF_SIGNED_CERT_IN_CHAIN"}
		case len(chain) == 1:
			return &nodeSystemError{"unable to verify the first certificate", "UNABLE_TO_VERIFY_LEAF_SIGNATURE"}
		}
		return &nodeSystemError{"unable to get local issuer certificate", "UNABLE_TO_GET_ISSUER_CERT_LOCALLY"}
	case errors.As(verify.Err, &invalid) && invalid.Reason == x509.Expired && invalid.Cert != nil:
		if time.Now().Before(invalid.Cert.NotBefore) {
			return &nodeSystemError{"certificate is not yet valid", "CERT_NOT_YET_VALID"}
		}
		return &nodeSystemError{"certificate has expired", "CERT_HAS_EXPIRED"}
	}
	return nil
}
