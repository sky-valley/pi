package providers

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sky-valley/pi/ai"
)

// The connection error cause detail of upstream 4c992154d (#11783): a
// request that gets no response keeps its reason in the error message, the
// deepest cause appended to the SDK's "Connection error." (or fetch's "fetch
// failed", or a body's "terminated"), with its code when the cause's message
// lacks it. The expected strings are what pi at 4c992154d prints, measured
// with node v22.23.4 (undici 6.29.1), openai 7.19.0 and @anthropic-ai/sdk
// 0.129.0 against the same failures.

// postError is the error the port's own client fails a POST to target with
// when cause stops it before a response.
func postError(target string, cause error) error {
	return &url.Error{Op: "Post", URL: target, Err: cause}
}

// errnoFor is the platform's socket error that Node reports as code.
func errnoFor(t *testing.T, code string) syscall.Errno {
	t.Helper()
	for errno, name := range errnoCodes {
		if name == code {
			return errno
		}
	}
	t.Fatalf("no errno for %s", code)
	return 0
}

// dialError is net's error for a connect to ip:port that failed with errno.
func dialError(ip string, port int, errno syscall.Errno) error {
	return &net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.ParseIP(ip), Port: port},
		Err: &os.SyscallError{Syscall: "connect", Err: errno}}
}

// causeTimeoutError is a transport error that reports itself as a timeout.
type causeTimeoutError struct{ msg string }

func (e causeTimeoutError) Error() string { return e.msg }
func (e causeTimeoutError) Timeout() bool { return true }

// TestConnectionErrorAppendsDeepestCause is upstream's "appends the deepest
// cause of a connection error with no status or body": the SDK's
// APIConnectionError wraps undici's TypeError "fetch failed", which wraps
// the socket's error.
func TestConnectionErrorAppendsDeepestCause(t *testing.T) {
	err := sdkFetchError(&fetchRejection{err: postError("http://127.0.0.1:8080/v1/messages", dialError("127.0.0.1", 8080, errnoFor(t, "ECONNREFUSED")))})
	if got, want := withCauseDetail(err), "Connection error.: connect ECONNREFUSED 127.0.0.1:8080"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if !errors.Is(err, errAPIConnection) {
		t.Fatalf("%v is not errAPIConnection", err)
	}
}

// TestConnectionErrorAddsCodeMissingFromCause is upstream's "adds the error
// code when the cause message lacks it": undici's SocketError for a
// connection the server closes before answering.
func TestConnectionErrorAddsCodeMissingFromCause(t *testing.T) {
	err := sdkFetchError(&fetchRejection{err: postError("http://127.0.0.1:8080/v1/messages", io.EOF)})
	if got, want := withCauseDetail(err), "Connection error.: other side closed (UND_ERR_SOCKET)"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestConnectionErrorWithoutCauseUnchanged is upstream's "leaves errors
// without a cause unchanged", plus the SDKs' APIConnectionTimeoutError,
// which they throw with no cause.
func TestConnectionErrorWithoutCauseUnchanged(t *testing.T) {
	timeout := sdkFetchError(&fetchRejection{err: postError("http://127.0.0.1:8080/v1/messages",
		&net.OpError{Op: "dial", Net: "tcp", Err: causeTimeoutError{"i/o timeout"}})})
	for _, tc := range []struct {
		err  error
		want string
	}{
		{errAPIConnection, "Connection error."},
		{errors.New("Connection error."), "Connection error."},
		{timeout, "Request timed out."},
		{errTerminated, "terminated"},
		{fmt.Errorf("wrapped: %w", errors.New("inner")), "wrapped: inner"},
	} {
		if got := withCauseDetail(tc.err); got != tc.want {
			t.Errorf("withCauseDetail(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// TestConnectionErrorFromClosedPort is upstream's "surfaces the reason from a
// real openai SDK connection failure", through each SDK adapter; google, a
// bare fetch through @google/genai, fails "fetch failed: connect ECONNREFUSED
// ..." (TestGoogleRefusedConnectionFailsFetch).
func TestConnectionErrorFromClosedPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	for _, adapter := range []string{"anthropic-messages", "openai-completions", "openai-responses"} {
		t.Run(adapter, func(t *testing.T) {
			final := streamSDKAdapter(t.Context(), adapter, "http://"+addr, ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"}})
			if want := "Connection error.: connect ECONNREFUSED " + addr; final.StopReason != ai.StopError || final.ErrorMessage != want {
				t.Fatalf("stream = %s %q; pi: error %q", final.StopReason, final.ErrorMessage, want)
			}
		})
	}
	// A custom HTTPClient is pi's custom fetch, whose rejection is the
	// APIConnectionError's cause as it is: its chain is the error's own,
	// walked to the deepest of five.
	t.Run("custom client", func(t *testing.T) {
		final := streamSDKAdapter(t.Context(), "openai-completions", "http://"+addr, ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k", HTTPClient: &http.Client{}}})
		if want := "Connection error.: connection refused"; final.ErrorMessage != want {
			t.Fatalf("stream = %s %q; want %q", final.StopReason, final.ErrorMessage, want)
		}
	})
}

// TestConnectionErrorFromBodyCutShort: a connection that fails mid-body
// errors undici's body stream with TypeError "terminated", whose cause is the
// socket's error; the SDKs throw it as it is, and pi's catch appends it.
func TestConnectionErrorFromBodyCutShort(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reset bool
		want  string
	}{
		{"closed", false, "terminated: other side closed (UND_ERR_SOCKET)"},
		{"reset", true, "terminated: read ECONNRESET"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			go func() {
				for {
					conn, err := ln.Accept()
					if err != nil {
						return
					}
					go func() {
						buf := make([]byte, 64<<10)
						conn.Read(buf)
						io.WriteString(conn, "HTTP/1.1 200 OK\r\ncontent-type: text/event-stream\r\ntransfer-encoding: chunked\r\n\r\n5\r\n: hi\n\r\n")
						time.Sleep(50 * time.Millisecond)
						if tc.reset {
							conn.(*net.TCPConn).SetLinger(0)
						}
						conn.Close()
					}()
				}
			}()
			for _, adapter := range []string{"anthropic-messages", "openai-completions", "openai-responses"} {
				t.Run(adapter, func(t *testing.T) {
					final := streamSDKAdapter(t.Context(), adapter, "http://"+ln.Addr().String(), ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"}})
					if final.StopReason != ai.StopError || final.ErrorMessage != tc.want {
						t.Fatalf("stream = %s %q; pi: error %q", final.StopReason, final.ErrorMessage, tc.want)
					}
				})
			}
		})
	}
}

// TestConnectionErrorSelfSignedCertificate: a real TLS handshake against a
// self-signed certificate, which Node's TLS refuses with
// DEPTH_ZERO_SELF_SIGNED_CERT.
func TestConnectionErrorSelfSignedCertificate(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	defer server.Close()
	_, err := sharedClient(5_000).Post(server.URL+"/x", "application/json", strings.NewReader("{}"))
	if err == nil {
		t.Fatal("the handshake succeeded")
	}
	if got, want := withCauseDetail(undiciFetchError(err)), "fetch failed: self-signed certificate (DEPTH_ZERO_SELF_SIGNED_CERT)"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestConnectionErrorNodeRenderings: net/http's failures rendered as Node
// words the socket, DNS and TLS errors undici's "fetch failed" carries; each
// string was measured against the same failure in node (see the file
// comment), the timeouts against undici 6.29.1 with its timeouts shortened.
func TestConnectionErrorNodeRenderings(t *testing.T) {
	const target = "https://api.example.test/v1/messages"
	cert := func(subject, issuer string) *x509.Certificate {
		return &x509.Certificate{RawSubject: []byte(subject), RawIssuer: []byte(issuer)}
	}
	unknownAuthority := func(chain ...*x509.Certificate) error {
		return &tls.CertificateVerificationError{UnverifiedCertificates: chain, Err: x509.UnknownAuthorityError{Cert: chain[0]}}
	}
	invalid := func(notBefore, notAfter time.Time) error {
		c := &x509.Certificate{NotBefore: notBefore, NotAfter: notAfter}
		return &tls.CertificateVerificationError{UnverifiedCertificates: []*x509.Certificate{c}, Err: x509.CertificateInvalidError{Cert: c, Reason: x509.Expired}}
	}
	now := time.Now()
	for _, tc := range []struct {
		name  string
		cause error
		want  string
	}{
		{"refused", dialError("127.0.0.1", 8080, errnoFor(t, "ECONNREFUSED")), "connect ECONNREFUSED 127.0.0.1:8080"},
		{"refused ipv6", dialError("::1", 8080, errnoFor(t, "ECONNREFUSED")), "connect ECONNREFUSED ::1:8080"},
		{"host unreachable", dialError("10.0.0.1", 443, errnoFor(t, "EHOSTUNREACH")), "connect EHOSTUNREACH 10.0.0.1:443"},
		{"network unreachable", dialError("10.0.0.1", 443, errnoFor(t, "ENETUNREACH")), "connect ENETUNREACH 10.0.0.1:443"},
		{"reset", &net.OpError{Op: "read", Net: "tcp", Err: &os.SyscallError{Syscall: "read", Err: errnoFor(t, "ECONNRESET")}}, "read ECONNRESET"},
		{"read timed out", &net.OpError{Op: "read", Net: "tcp", Err: &os.SyscallError{Syscall: "read", Err: errnoFor(t, "ETIMEDOUT")}}, "read ETIMEDOUT"},
		{"not found", &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "api.example.test", IsNotFound: true}}, "getaddrinfo ENOTFOUND api.example.test"},
		{"dns try again", &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "server misbehaving", Name: "api.example.test", IsTemporary: true}}, "getaddrinfo EAI_AGAIN api.example.test"},
		{"closed", io.EOF, "other side closed (UND_ERR_SOCKET)"},
		{"headers timeout", causeTimeoutError{"net/http: timeout awaiting response headers"}, "Headers Timeout Error (UND_ERR_HEADERS_TIMEOUT)"},
		{"connect timeout", &net.OpError{Op: "dial", Net: "tcp", Err: causeTimeoutError{"i/o timeout"}}, "Connect Timeout Error (attempted address: api.example.test:443, timeout: 10000ms) (UND_ERR_CONNECT_TIMEOUT)"},
		{"tls handshake timeout", causeTimeoutError{"net/http: TLS handshake timeout"}, "Connect Timeout Error (attempted address: api.example.test:443, timeout: 10000ms) (UND_ERR_CONNECT_TIMEOUT)"},
		{"self-signed", unknownAuthority(cert("leaf", "leaf")), "self-signed certificate (DEPTH_ZERO_SELF_SIGNED_CERT)"},
		{"self-signed in chain", unknownAuthority(cert("leaf", "ca"), cert("ca", "ca")), "self-signed certificate in certificate chain (SELF_SIGNED_CERT_IN_CHAIN)"},
		{"leaf only", unknownAuthority(cert("leaf", "ca")), "unable to verify the first certificate (UNABLE_TO_VERIFY_LEAF_SIGNATURE)"},
		{"issuer not local", unknownAuthority(cert("leaf", "int"), cert("int", "ca")), "unable to get local issuer certificate (UNABLE_TO_GET_ISSUER_CERT_LOCALLY)"},
		{"expired", invalid(now.Add(-48*time.Hour), now.Add(-24*time.Hour)), "certificate has expired (CERT_HAS_EXPIRED)"},
		{"not yet valid", invalid(now.Add(24*time.Hour), now.Add(48*time.Hour)), "certificate is not yet valid (CERT_NOT_YET_VALID)"},
		// No faithful Node rendering: net/http's own words, the deepest of
		// its chain.
		{"malformed response", fmt.Errorf("net/http: HTTP/1.x transport connection broken: %w", errors.New(`malformed HTTP response "garbage"`)), `malformed HTTP response "garbage"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := withCauseDetail(undiciFetchError(postError(target, tc.cause)))
			if want := "fetch failed: " + tc.want; got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		})
	}
}

// TestConnectionErrorCauseDepth: like JS's cause chain, a custom client's
// Unwrap chain is walked at most five levels below the SDK's error; the
// refusal undici's fetch throws itself (a URL with credentials) has no cause.
func TestConnectionErrorCauseDepth(t *testing.T) {
	var err error = errors.New("level 6")
	for i := 5; i >= 1; i-- {
		err = fmt.Errorf("level %d: %w", i, err)
	}
	if got, want := withCauseDetail(sdkFetchError(&fetchRejection{err: err, custom: true})), "Connection error.: level 5: level 6"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	refused := &fetchCredentialsError{href: "http://u:p@127.0.0.1:8080/v1/messages"}
	if got, want := withCauseDetail(sdkFetchError(&fetchRejection{err: refused})), "Connection error.: Request cannot be constructed from a URL that includes credentials: http://u:p@127.0.0.1:8080/v1/messages"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestTimeoutErrorCauseIsOpenAIsAlone: openai 7.19.0 gives its
// APIConnectionTimeoutError the rejected fetch for a cause; @anthropic-ai/sdk
// 0.129.0 gives it none. A custom client's context errors stand for the
// DOMExceptions a custom fetch's signal rejects with.
func TestTimeoutErrorCauseIsOpenAIsAlone(t *testing.T) {
	for _, tc := range []struct {
		name      string
		rejection *fetchRejection
		openai    string
	}{
		{"custom abort", &fetchRejection{err: context.Canceled, custom: true}, "Request timed out.: This operation was aborted (20)"},
		{"custom timeout", &fetchRejection{err: context.DeadlineExceeded, custom: true}, "Request timed out.: The operation was aborted due to timeout (23)"},
		{"custom text", &fetchRejection{err: errors.New("the custom fetch timed out"), custom: true}, "Request timed out.: the custom fetch timed out"},
		{"timeoutMs", &fetchRejection{err: postError("http://127.0.0.1:8080/v1/responses", causeTimeoutError{"net/http: timeout awaiting response headers"})}, "Request timed out.: This operation was aborted (20)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := withCauseDetail(openaiFetchError(tc.rejection)); got != tc.openai {
				t.Errorf("openai: got %q, want %q", got, tc.openai)
			}
			if got := withCauseDetail(sdkFetchError(tc.rejection)); got != "Request timed out." {
				t.Errorf("anthropic: got %q, want %q", got, "Request timed out.")
			}
		})
	}
}

// causeDetailMessages is the cause-detail overlay of the capture in dir
// (testdata/cause-detail.mts): pi's errorMessage at upstream 4c992154d for
// each row whose message withCauseDetail extends, by the row's JSON pointer.
// The captures predate 4c992154d, and one taken past it on node v22 reads
// and decodes the wire differently elsewhere, so only the errorMessage is
// taken from it.
//
// Where net/http's error does not carry what Node's HTTP parser says of a
// response that is not HTTP (llhttp's HPE_* errors), the port's detail is
// net/http's own words (undiciCause), and the overlay's message is given that
// detail (llhttpDivergences).
func causeDetailMessages(t *testing.T, dir string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", dir, "cause-detail-4c992154d.json"))
	if err != nil {
		t.Fatalf("read %s's cause-detail overlay: %v (derive it with testdata/cause-detail.mts)", dir, err)
	}
	var overlay struct {
		ErrorMessages map[string]string `json:"errorMessages"`
	}
	if err := json.Unmarshal(raw, &overlay); err != nil {
		t.Fatal(err)
	}
	if len(overlay.ErrorMessages) == 0 {
		t.Fatalf("%s's cause-detail overlay is empty", dir)
	}
	for pointer, message := range overlay.ErrorMessages {
		for llhttp, nethttp := range llhttpDivergences {
			message = strings.Replace(message, ": "+llhttp, ": "+nethttp, 1)
		}
		overlay.ErrorMessages[pointer] = message
	}
	return overlay.ErrorMessages
}

// llhttpDivergences maps llhttp's words for a malformed response, as pi's
// cause detail gives them, to net/http's for the same bytes: a status line
// "NOT HTTP", and "HTTP/1.1 OK 200". net/http checks the status code before
// the version and quotes only the code, so its error cannot tell them apart.
var llhttpDivergences = map[string]string{
	"Response does not match the HTTP/1.1 protocol (Expected HTTP/) (HPE_INVALID_CONSTANT)":        `malformed HTTP status code "HTTP"`,
	"Response does not match the HTTP/1.1 protocol (Invalid response status) (HPE_INVALID_STATUS)": `malformed HTTP status code "OK"`,
}

// scanPointer reads a cause-detail overlay's JSON pointer by format, which
// must match it whole.
func scanPointer(pointer, format string, i *int) bool {
	_, err := fmt.Sscanf(pointer, format, i)
	return err == nil && fmt.Sprintf(format, *i) == pointer
}
