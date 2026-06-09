package coding

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebFetchStripsHTML(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<!doctype html><html><head><style>.x{}</style><script>var x=1;</script></head>` +
			`<body><h1>Title</h1><p>Hello &amp; welcome to the <b>docs</b>.</p><p>Second para.</p></body></html>`))
	}))
	defer server.Close()

	r, err := run(t, webFetchTool(t.TempDir()), map[string]any{"url": server.URL})
	if err != nil {
		t.Fatal(err)
	}
	out := resultText(r)
	if strings.Contains(out, "<") || strings.Contains(out, "var x=1") || strings.Contains(out, ".x{}") {
		t.Fatalf("HTML/script/style not stripped: %q", out)
	}
	if !strings.Contains(out, "Title") || !strings.Contains(out, "Hello & welcome") || !strings.Contains(out, "Second para") {
		t.Fatalf("text content lost: %q", out)
	}
}

func TestWebFetchRejectsNonHTTP(t *testing.T) {
	_, err := run(t, webFetchTool(t.TempDir()), map[string]any{"url": "file:///etc/passwd"})
	if err == nil || !strings.Contains(err.Error(), "http(s)") {
		t.Fatalf("expected http(s)-only rejection, got %v", err)
	}
}

func TestWebFetchSurfacesHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	}))
	defer server.Close()
	_, err := run(t, webFetchTool(t.TempDir()), map[string]any{"url": server.URL})
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("expected 404 surfaced, got %v", err)
	}
}
