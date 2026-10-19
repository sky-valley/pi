package ai

import (
	"context"
	"strings"
)

// resolveCloudflareModel fills a Cloudflare model's endpoint placeholders,
// {CLOUDFLARE_ACCOUNT_ID} and {CLOUDFLARE_GATEWAY_ID}, from the request's
// provider env (pi providers/cloudflare-stream.ts resolveCloudflareModel,
// upstream 9993c9690, a7d17e39a). It never fails and never reads the process
// environment: a placeholder the env does not name stays in the URL, and a
// request without env goes to the model's URL as it is. Through Models the
// env is the one Cloudflare auth resolved (cloudflare_auth.go); through compat
// with a key of the caller's own, it is whatever the caller passed.
func resolveCloudflareModel(model *Model, env map[string]string) *Model {
	if env == nil {
		return model
	}
	baseURL := model.BaseURL
	for _, name := range []string{cloudflareAccountIDEnv, cloudflareGatewayIDEnv} {
		if value, ok := env[name]; ok {
			baseURL = strings.ReplaceAll(baseURL, "{"+name+"}", value)
		}
	}
	if baseURL == model.BaseURL {
		return model
	}
	resolved := *model
	resolved.BaseURL = baseURL
	return &resolved
}

// cloudflareStreams wraps an api implementation so a Cloudflare model's
// endpoint placeholders are filled from the request's env before dispatch (pi
// cloudflareStreams). Like pi's, the wrapper carries streaming only: a
// Cloudflare provider has no deferred responses.
func cloudflareStreams(streams ProviderStreams) ProviderStreams {
	wrapped := ProviderStreams{}
	if stream := streams.Stream; stream != nil {
		wrapped.Stream = func(ctx context.Context, model *Model, req TranscriptContext, opts *StreamOptions) *AssistantMessageEventStream {
			var env map[string]string
			if opts != nil {
				env = opts.Env
			}
			return stream(ctx, resolveCloudflareModel(model, env), req, opts)
		}
	}
	if streamSimple := streams.StreamSimple; streamSimple != nil {
		wrapped.StreamSimple = func(ctx context.Context, model *Model, req TranscriptContext, opts *SimpleStreamOptions) *AssistantMessageEventStream {
			var env map[string]string
			if opts != nil {
				env = opts.Env
			}
			return streamSimple(ctx, resolveCloudflareModel(model, env), req, opts)
		}
	}
	return wrapped
}
