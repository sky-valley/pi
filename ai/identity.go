package ai

import (
	"context"
	"net/url"
	"runtime"
	"strings"
	"unicode"
	"unicode/utf8"
)

// How requests identify the calling app to providers: the User-Agent and the
// provider attribution headers. Ported from pi utils/identity.ts (upstream
// adc5fa9f8, 04a93bad8), which moved both out of the coding agent into pi-ai.
// pi-ai's defaults are a detailed User-Agent with its own version and no
// attribution; pi-agent-core attributes requests to pi (see the agent
// package's DefaultAgentIdentity), and the coding agent sets its identity from
// the install-telemetry setting.

// UserAgentMode is the User-Agent a request sends (pi UserAgentMode):
//   - UserAgentDetailed: pi/<version> (<platform>; <runtime>/<runtime version>; <arch>)
//   - UserAgentShort: pi
//   - UserAgentCustom(value): the given value
//
// The zero value is unset, which an identity merge skips and which formats
// as detailed.
type UserAgentMode struct {
	kind   userAgentKind
	custom string
}

type userAgentKind uint8

const (
	userAgentUnset userAgentKind = iota
	userAgentShort
	userAgentDetailed
	userAgentCustom
)

var (
	// UserAgentShort sends "pi".
	UserAgentShort = UserAgentMode{kind: userAgentShort}
	// UserAgentDetailed sends pi's version, platform, runtime and arch.
	UserAgentDetailed = UserAgentMode{kind: userAgentDetailed}
)

// UserAgentCustom sends value as the User-Agent (pi's `{ custom }`).
func UserAgentCustom(value string) UserAgentMode {
	return UserAgentMode{kind: userAgentCustom, custom: value}
}

// IsSet reports whether the mode was chosen, as opposed to left to the
// identity merged beneath it.
func (m UserAgentMode) IsSet() bool { return m.kind != userAgentUnset }

// ClientAttribution is who requests are attributed to (pi ClientAttribution).
// GetAttributionHeaders maps the fields to each provider's headers.
type ClientAttribution struct {
	// Name is the app name: OpenRouter's app title, NVIDIA's invoke origin
	// (first letter upper-cased), OpenCode's client.
	Name string
	// URL is the app URL: OpenRouter's HTTP-Referer, the app's identity in
	// OpenRouter's rankings, grouped by origin.
	URL string
	// Categories are the app categories, e.g. ["cli-agent"]: OpenRouter's
	// X-OpenRouter-Categories, comma-separated.
	Categories []string
}

// PiAttribution is pi's attribution, used by the agent loop and the coding
// agent (pi PI_ATTRIBUTION). Requests share it by pointer, its Categories
// slice included: treat it as read-only.
var PiAttribution = ClientAttribution{Name: "pi", URL: "https://pi.dev", Categories: []string{"cli-agent"}}

// NoAttribution, as ClientIdentity.Attribution, is pi's `attribution: null`:
// no attribution headers, also turning off attribution an identity merged
// beneath set. It is compared by identity, as http.NoBody is.
var NoAttribution = &ClientAttribution{}

// ClientIdentity is how a request identifies the calling app (pi
// ClientIdentity). Every field is optional; an unset one takes pi-ai's
// default: a detailed User-Agent with pi-ai's version and no attribution.
type ClientIdentity struct {
	// UserAgent defaults to UserAgentDetailed.
	UserAgent UserAgentMode
	// Attribution sends provider attribution headers. Nil leaves it to the
	// identity merged beneath (none by default); NoAttribution sends none.
	Attribution *ClientAttribution
	// Version is the version in the detailed User-Agent. Default: PiAIVersion.
	Version string
}

// ClientIdentityTarget is the provider and endpoint a request goes to (pi
// ClientIdentityTarget).
type ClientIdentityTarget struct {
	Provider string
	BaseURL  string
}

// IdentityTarget is the model as a request target.
func (m *Model) IdentityTarget() ClientIdentityTarget {
	return ClientIdentityTarget{Provider: m.Provider, BaseURL: m.BaseURL}
}

const (
	nvidiaNimHost = "integrate.api.nvidia.com"
	openCodeHost  = "opencode.ai"
)

// MergeClientIdentity merges identities field by field (pi
// mergeClientIdentity): later sources win, unset fields are skipped, and nil
// sources contribute nothing. It returns nil when every source is nil.
func MergeClientIdentity(sources ...*ClientIdentity) *ClientIdentity {
	var merged *ClientIdentity
	for _, source := range sources {
		if source == nil {
			continue
		}
		next := ClientIdentity{}
		if merged != nil {
			next = *merged
		}
		if source.UserAgent.IsSet() {
			next.UserAgent = source.UserAgent
		}
		if source.Attribution != nil {
			next.Attribution = source.Attribution
		}
		if source.Version != "" {
			next.Version = source.Version
		}
		merged = &next
	}
	return merged
}

// GetClientVersion is the version in the detailed User-Agent (pi
// getClientVersion).
func GetClientVersion(identity *ClientIdentity) string {
	if identity != nil && identity.Version != "" {
		return identity.Version
	}
	return PiAIVersion
}

// FormatUserAgent is the User-Agent value of an identity (pi
// formatUserAgent). pi's detailed form names the Node.js or Bun runtime it
// runs on, and its platform and arch in Node's spelling; the port names the
// Go runtime in that slot, as go/<version>, and keeps Node's spelling for the
// other two (win32, x64, ia32), so only the runtime differs from pi's string.
func FormatUserAgent(identity *ClientIdentity) string {
	mode := UserAgentDetailed
	if identity != nil && identity.UserAgent.IsSet() {
		mode = identity.UserAgent
	}
	switch mode.kind {
	case userAgentCustom:
		return mode.custom
	case userAgentShort:
		return "pi"
	}
	return "pi/" + GetClientVersion(identity) + " (" + nodePlatform() + "; " + goRuntimeDescription() + "; " + nodeArch() + ")"
}

// goRuntimeDescription is the runtime slot of the detailed User-Agent, which
// pi fills with node/<process.version> or bun/<version>: here the Go runtime,
// go/1.26.4 for go1.26.4 (a development toolchain's version is kept whole).
func goRuntimeDescription() string {
	return "go/" + strings.TrimPrefix(runtime.Version(), "go")
}

// nodePlatform maps runtime.GOOS to Node's process.platform name. They agree
// everywhere the port builds except Windows.
func nodePlatform() string {
	if runtime.GOOS == "windows" {
		return "win32"
	}
	return runtime.GOOS
}

// nodeArch maps runtime.GOARCH to Node's process.arch name (documented values:
// arm, arm64, ia32, loong64, mips, mipsel, ppc64, riscv64, s390x, x64).
func nodeArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "386":
		return "ia32"
	case "mipsle":
		return "mipsel"
	case "ppc64le":
		return "ppc64"
	default:
		return runtime.GOARCH
	}
}

// matchesHost is pi's matchesHost: whether baseURL parses as a URL whose
// hostname is expectedHost. A URL that does not parse matches nothing. pi
// parses with the WHATWG URL parser, so the input is first normalized as that
// parser normalizes it (see whatwgURLInput), and the host of a special-scheme
// URL is compared lowercased, as the parser lowers it; any other scheme's
// host stays as written.
func matchesHost(baseURL, expectedHost string) bool {
	u, err := url.Parse(whatwgURLInput(baseURL))
	if err != nil || u.Hostname() == "" {
		return false
	}
	host := u.Hostname()
	if whatwgSpecialSchemes[u.Scheme] { // url.Parse lowercases the scheme
		host = strings.ToLower(host)
	}
	return host == expectedHost
}

var whatwgSpecialSchemes = map[string]bool{"http": true, "https": true, "ws": true, "wss": true, "ftp": true, "file": true}

// whatwgURLInput is the WHATWG parser's preprocessing of a URL string: leading
// and trailing C0 controls and spaces are trimmed, every tab and newline is
// removed, and after a special scheme a backslash reads as a slash.
func whatwgURLInput(raw string) string {
	raw = strings.TrimFunc(raw, func(r rune) bool { return r <= 0x20 })
	raw = strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, raw)
	if i := strings.IndexByte(raw, ':'); i > 0 && whatwgSpecialSchemes[strings.ToLower(raw[:i])] {
		raw = raw[:i+1] + strings.ReplaceAll(raw[i+1:], `\`, "/")
	}
	return raw
}

func isOpenRouterTarget(target ClientIdentityTarget) bool {
	// A substring test, not a host match, as pi's isOpenRouter is.
	return target.Provider == "openrouter" || strings.Contains(target.BaseURL, "openrouter.ai")
}

func isNvidiaNimTarget(target ClientIdentityTarget) bool {
	return target.Provider == "nvidia" || matchesHost(target.BaseURL, nvidiaNimHost)
}

func isOpenCodeTarget(target ClientIdentityTarget) bool {
	return target.Provider == "opencode" || target.Provider == "opencode-go" || matchesHost(target.BaseURL, openCodeHost)
}

// GetAttributionHeaders is an identity's provider attribution headers for a
// request target (pi getAttributionHeaders): empty when attribution is off.
func GetAttributionHeaders(target ClientIdentityTarget, identity *ClientIdentity) map[string]string {
	if identity == nil || identity.Attribution == nil || identity.Attribution == NoAttribution {
		return map[string]string{}
	}
	attribution := identity.Attribution
	switch {
	case isOpenRouterTarget(target):
		headers := map[string]string{
			"HTTP-Referer":       attribution.URL,
			"X-OpenRouter-Title": attribution.Name,
		}
		if len(attribution.Categories) > 0 {
			headers["X-OpenRouter-Categories"] = strings.Join(attribution.Categories, ",")
		}
		return headers
	case isNvidiaNimTarget(target):
		// NVIDIA expects a capitalized origin, such as "Pi".
		return map[string]string{"X-BILLING-INVOKE-ORIGIN": upperFirst(attribution.Name)}
	case isOpenCodeTarget(target):
		return map[string]string{"x-opencode-client": attribution.Name}
	}
	return map[string]string{}
}

// upperFirst is JavaScript's `name.charAt(0).toUpperCase() + name.slice(1)`.
// charAt(0) is one UTF-16 code unit, so a name that opens with an astral
// character is left as it is; toUpperCase can lengthen a character (ß is SS).
func upperFirst(name string) string {
	r, size := utf8.DecodeRuneInString(name)
	if size == 0 || r > 0xFFFF || r == utf8.RuneError && size == 1 {
		return name
	}
	return jsToUpper(r) + name[size:]
}

// jsToUpper is String.prototype.toUpperCase of one BMP character: Unicode's
// full case mapping in Node's tables, which unicode.ToUpper (the simple
// mapping, in Go's tables) matches but for jsUpperDiffs.
func jsToUpper(r rune) string {
	if upper, ok := jsUpperDiffs[r]; ok {
		return upper
	}
	return string(unicode.ToUpper(r))
}

// GetOpenCodeSessionHeaders is OpenCode's per-conversation routing header for
// requests to OpenCode, custom providers pointing at it included (pi
// getOpenCodeSessionHeaders); nil without a session or for other targets.
func GetOpenCodeSessionHeaders(target ClientIdentityTarget, sessionID string) map[string]string {
	if sessionID == "" || !isOpenCodeTarget(target) {
		return nil
	}
	return map[string]string{"x-opencode-session": sessionID}
}

// GetIdentityHeaders is the default headers an identity adds to a request:
// the User-Agent and provider attribution (pi getIdentityHeaders). Adapters
// apply them below model, provider and request headers, so configured
// headers override them and a deletion marker in request headers removes them.
func GetIdentityHeaders(target ClientIdentityTarget, identity *ClientIdentity) map[string]string {
	headers := GetAttributionHeaders(target, identity)
	headers["User-Agent"] = FormatUserAgent(identity)
	return headers
}

// WithClientIdentity is models with identity as the default identity of its
// requests (pi withClientIdentity): a request's identity overrides it field
// by field, and it overrides the identity models was created with.
// Everything else is delegated unchanged; a MutableModels stays mutable.
func WithClientIdentity(models Models, identity ClientIdentity) Models {
	wrapped := identityModels{Models: models, identity: identity}
	if mutable, ok := models.(MutableModels); ok {
		return identityMutableModels{identityModels: wrapped, mutable: mutable}
	}
	return wrapped
}

type identityModels struct {
	Models
	identity ClientIdentity
}

type identityMutableModels struct {
	identityModels
	mutable MutableModels
}

func (m identityMutableModels) SetProvider(provider Provider) { m.mutable.SetProvider(provider) }
func (m identityMutableModels) DeleteProvider(id string)      { m.mutable.DeleteProvider(id) }
func (m identityMutableModels) ClearProviders()               { m.mutable.ClearProviders() }

// with merges the wrapper's identity beneath the request's own.
func (m identityModels) with(options *ProviderRequestOptions) {
	options.Identity = MergeClientIdentity(&m.identity, options.Identity)
}

func (m identityModels) Stream(ctx context.Context, model *Model, req Context, opts *ModelsStreamOptions) *AssistantMessageEventStream {
	next := ModelsStreamOptions{}
	if opts != nil {
		next = *opts
	}
	m.with(&next.ProviderRequestOptions)
	return m.Models.Stream(ctx, model, req, &next)
}

func (m identityModels) Complete(ctx context.Context, model *Model, req Context, opts *ModelsStreamOptions) *AssistantMessage {
	next := ModelsStreamOptions{}
	if opts != nil {
		next = *opts
	}
	m.with(&next.ProviderRequestOptions)
	return m.Models.Complete(ctx, model, req, &next)
}

func (m identityModels) StreamSimple(ctx context.Context, model *Model, req Context, opts *ModelsSimpleStreamOptions) *AssistantMessageEventStream {
	next := ModelsSimpleStreamOptions{}
	if opts != nil {
		next = *opts
	}
	m.with(&next.ProviderRequestOptions)
	return m.Models.StreamSimple(ctx, model, req, &next)
}

func (m identityModels) CompleteSimple(ctx context.Context, model *Model, req Context, opts *ModelsSimpleStreamOptions) *AssistantMessage {
	next := ModelsSimpleStreamOptions{}
	if opts != nil {
		next = *opts
	}
	m.with(&next.ProviderRequestOptions)
	return m.Models.CompleteSimple(ctx, model, req, &next)
}

func (m identityModels) StreamDeferred(ctx context.Context, model *Model, handle DeferredHandle, opts *ModelsDeferredFetchOptions) *AssistantMessageEventStream {
	next := ModelsDeferredFetchOptions{}
	if opts != nil {
		next = *opts
	}
	m.with(&next.ProviderRequestOptions)
	return m.Models.StreamDeferred(ctx, model, handle, &next)
}

func (m identityModels) FetchDeferred(ctx context.Context, model *Model, handle DeferredHandle, opts *ModelsDeferredFetchOptions) *AssistantMessage {
	next := ModelsDeferredFetchOptions{}
	if opts != nil {
		next = *opts
	}
	m.with(&next.ProviderRequestOptions)
	return m.Models.FetchDeferred(ctx, model, handle, &next)
}

func (m identityModels) CancelDeferred(ctx context.Context, model *Model, handle DeferredHandle, opts *ModelsDeferredCancelOptions) error {
	next := ModelsDeferredCancelOptions{}
	if opts != nil {
		next = *opts
	}
	m.with(&next.DeferredCancelOptions)
	return m.Models.CancelDeferred(ctx, model, handle, &next)
}
