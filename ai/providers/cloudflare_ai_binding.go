package providers

// AI Gateway transport over the Workers AI binding (pi
// packages/ai/src/api/cloudflare-ai-binding.ts, upstream 55adba4f2).
//
// pi's Cloudflare AI Gateway support speaks HTTPS
// (`gateway.ai.cloudflare.com/v1/{account}/{gateway}/{provider}/...`, the
// cloudflare-ai-gateway models' base URLs), which needs a Cloudflare API token
// even when the caller is a Worker in the gateway's own account.
//
// A Worker avoids that token by talking to the gateway through the AI
// binding's fetch passthrough (`env.AI.fetch()`), which serves the gateway's
// provider passthrough at
// `https://workers-binding.ai/ai-gateway/gateways/{gateway}/{provider}/{endpoint...}`
// — the same shape as the HTTPS URL, minus the account id (the binding channel
// carries identity). Binding calls are pre-authenticated in-account and return
// the provider's native wire format as a regular (streaming) response, so API
// implementations behave identically over either transport.
//
// A model whose BaseURL names that route therefore needs no translation: point
// it there and pass [CreateAIBindingFetch]'s doer as the request HTTPClient.
// Nothing is rewritten, buffered or re-encoded — method, headers, query string
// and the body stream go to the binding as they arrive.
//
// This surface is LATENT in the Go port: an [AIBinding] can only come from a
// Cloudflare Workers runtime, which Go does not target, so nothing in the port
// supplies one. It is ported because it is public SDK surface under pi-ai's
// "./api/*" subpath export (the 2026-08-11 ruling in docs/UPSTREAM.md: a
// transport is not out of scope for being runtime-specific).

import (
	"errors"
	"net/http"

	"github.com/sky-valley/pi/ai"
)

// AIBinding is the Workers AI binding (`env.AI`), described as the one call
// this module makes on it, so a host adapts whatever reaches the binding.
//
// pi's AiBinding also declares `aiGatewayLogId` and marks `fetch` optional.
// Both answer TypeScript's typing of `env.AI`: the first pins the structural
// type to the AI binding rather than an AiGateway, the second admits the
// published `Ai` type, which does not declare fetch yet. A Go host writes its
// own adapter type, and an interface method cannot be optional, so neither has
// a counterpart; the construction-time check for a missing fetch becomes the
// check for a nil binding.
type AIBinding interface {
	// Fetch sends req to the binding's fetch passthrough (`env.AI.fetch()`)
	// and returns its response.
	Fetch(req *http.Request) (*http.Response, error)
}

// CloudflareGatewayBindingAuthSentinel is the placeholder value for auth
// headers on binding-routed requests. API implementations require an API key
// or a recognized auth header (`authorization`, `x-api-key`,
// `cf-aig-authorization`) before dispatch; binding calls are pre-authenticated,
// so pass `cf-aig-authorization: Bearer ` + CloudflareGatewayBindingAuthSentinel
// to satisfy the check. The gateway ignores (and strips) `cf-aig-authorization`
// on binding-routed requests. Pair it with nil-valued `Authorization` /
// `x-api-key` entries in [ai.ProviderHeaders] so the SDKs' placeholder auth
// headers never reach the gateway, which would treat a request-supplied auth
// header as a BYOK provider key that overrides its stored keys — the same as it
// would over HTTPS.
const CloudflareGatewayBindingAuthSentinel = "cloudflare-gateway-binding"

// CreateAIBindingFetch creates an [ai.HTTPDoer] backed by the AI binding, for
// models whose BaseURL already names a route the binding serves — including
// the gateway's provider passthrough,
// `https://workers-binding.ai/ai-gateway/gateways/{gateway}/{provider}/...`.
// Requests pass through untouched:
//
//	model.BaseURL = "https://workers-binding.ai/ai-gateway/gateways/" + gateway + "/anthropic"
//	doer, err := providers.CreateAIBindingFetch(binding)
//	// ...
//	opts.HTTPClient = doer
//	opts.Headers = ai.ProviderHeaders{
//		"cf-aig-authorization": &sentinelBearer, // "Bearer " + CloudflareGatewayBindingAuthSentinel
//		"Authorization":        nil,
//		"x-api-key":            nil,
//	}
//
// A nil binding is refused here rather than on the first request, as pi
// refuses a binding without fetch().
func CreateAIBindingFetch(binding AIBinding) (ai.HTTPDoer, error) {
	if binding == nil {
		return nil, errors.New("createAiBindingFetch: the AI binding does not expose fetch(); " +
			"pass an AIBinding whose Fetch reaches env.AI.fetch()")
	}
	return aiBindingFetch{binding: binding}, nil
}

// aiBindingFetch is the doer CreateAIBindingFetch returns.
type aiBindingFetch struct{ binding AIBinding }

func (f aiBindingFetch) Do(req *http.Request) (*http.Response, error) { return f.binding.Fetch(req) }
