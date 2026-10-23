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
			return stream(ctx, model, req, withOpenCodeSession(opts))
		}
	}
	if streamSimple := streams.StreamSimple; streamSimple != nil {
		wrapped.StreamSimple = func(ctx context.Context, model *Model, req TranscriptContext, opts *SimpleStreamOptions) *AssistantMessageEventStream {
			if opts != nil {
				next := *opts
				next.StreamOptions = *withOpenCodeSession(&opts.StreamOptions)
				opts = &next
			}
			return streamSimple(ctx, model, req, opts)
		}
	}
	return wrapped
}

// withOpenCodeSession is pi's withSessionHeader: opts with the session header
// added, or opts itself when it has no session or already names the header.
func withOpenCodeSession(opts *StreamOptions) *StreamOptions {
	if opts == nil || opts.SessionID == "" || hasHeaderName(opts.Headers, openCodeSessionHeader) {
		return opts
	}
	next := *opts
	next.Headers = withHeader(opts.Headers, openCodeSessionHeader, opts.SessionID)
	return &next
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

// stringHeaders is a plain string record as ProviderHeaders, nil when empty.
func stringHeaders(record map[string]string) ProviderHeaders {
	if len(record) == 0 {
		return nil
	}
	headers := make(ProviderHeaders, len(record))
	for name, value := range record {
		headers[name] = HeaderValue(value)
	}
	return headers
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
