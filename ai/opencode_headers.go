package ai

import (
	"context"
	"strings"
)

const openCodeSessionHeader = "x-opencode-session"

// openCodeStreams is pi's withOpenCodeSessionHeader (providers/
// opencode-headers.ts): the opencode and opencode-go providers add OpenCode's
// required per-conversation routing header from the request's session id
// before api dispatch, unless a request header already names it in any
// spelling. The Models layer adds it too, beneath every other source
// (applyAuth), so this reaches only a request made through the provider
// directly; deferred redemption carries no session and is passed through.
func openCodeStreams(streams ProviderStreams) ProviderStreams {
	wrapped := streams
	if stream := streams.Stream; stream != nil {
		wrapped.Stream = func(ctx context.Context, model *Model, req TranscriptContext, opts *StreamOptions) *AssistantMessageEventStream {
			if opts != nil && opts.SessionID != "" && !hasHeaderName(opts.Headers, openCodeSessionHeader) {
				next := *opts
				next.Headers = withHeader(opts.Headers, openCodeSessionHeader, opts.SessionID)
				opts = &next
			}
			return stream(ctx, model, req, opts)
		}
	}
	if streamSimple := streams.StreamSimple; streamSimple != nil {
		wrapped.StreamSimple = func(ctx context.Context, model *Model, req TranscriptContext, opts *SimpleStreamOptions) *AssistantMessageEventStream {
			if opts != nil && opts.SessionID != "" && !hasHeaderName(opts.Headers, openCodeSessionHeader) {
				next := *opts
				next.Headers = withHeader(opts.Headers, openCodeSessionHeader, opts.SessionID)
				opts = &next
			}
			return streamSimple(ctx, model, req, opts)
		}
	}
	return wrapped
}

// hasHeaderName is pi's hasHeader in opencode-headers.ts: whether headers
// names the header in any spelling, a deletion marker included.
func hasHeaderName(headers ProviderHeaders, name string) bool {
	for key := range headers {
		if strings.EqualFold(key, name) {
			return true
		}
	}
	return false
}

// withHeader is `{ ...headers, [name]: value }`: a copy, so the caller's map
// is never written through.
func withHeader(headers ProviderHeaders, name, value string) ProviderHeaders {
	next := make(ProviderHeaders, len(headers)+1)
	for key, v := range headers {
		next[key] = v
	}
	next[name] = HeaderValue(value)
	return next
}
