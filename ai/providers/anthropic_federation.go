package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"

	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/internal/jstext"
)

// Anthropic workload identity federation (upstream a9424cd43). pi hands the
// federation config to @anthropic-ai/sdk, which exchanges the identity token
// for an access token, caches it and sends it as a bearer; the port has no
// SDK, so this file is the part of @anthropic-ai/sdk 0.124.0 pi reaches:
// lib/credentials/oidc-federation.ts (the exchange), identity-token.ts (the
// file read), token-cache.ts (the cache), the config branch of
// credential-chain.ts resolveCredentialsFromConfig, and the token-auth arms of
// client.ts (authHeaders, prepareRequest, shouldRetry). pi's config carries no
// credentials_path, so the SDK's on-disk token cache never comes into play.

// anthropicFederation is pi's AnthropicFederationConfig: the ids it reads
// from the ANTHROPIC_* variables. An empty optional id is pi's undefined.
type anthropicFederation struct {
	ruleID, organizationID, identityTokenFile string
	serviceAccountID, workspaceID             string
}

// getAnthropicFederation mirrors pi getAnthropicFederation: only the
// anthropic provider federates (the exchange is an Anthropic API endpoint),
// only when no key or auth header was resolved, and only when all three
// required variables are set. authToken is the port's own ANTHROPIC_AUTH_TOKEN
// arm (see StreamAnthropic), which pi's resolver turns into the Authorization
// header hasRequestAuth sees.
func getAnthropicFederation(model *ai.Model, apiKey, authToken string, headers ai.ProviderHeaders, env map[string]string) *anthropicFederation {
	if model.Provider != "anthropic" || apiKey != "" || authToken != "" || hasAnthropicAuthHeader(headers) {
		return nil
	}
	f := &anthropicFederation{
		ruleID:            ai.ProviderEnvValue(ai.AnthropicFederationRuleIDEnv, env),
		organizationID:    ai.ProviderEnvValue(ai.AnthropicOrganizationIDEnv, env),
		identityTokenFile: ai.ProviderEnvValue(ai.AnthropicIdentityTokenFileEnv, env),
		serviceAccountID:  ai.ProviderEnvValue(ai.AnthropicServiceAccountIDEnv, env),
		workspaceID:       ai.ProviderEnvValue(ai.AnthropicWorkspaceIDEnv, env),
	}
	if f.ruleID == "" || f.organizationID == "" || f.identityTokenFile == "" {
		return nil
	}
	return f
}

// anthropicFederationClient is pi's module-level federationClient: the SDK
// caches the access token per client and pi builds a client per request, so
// pi keeps ONE client for the current base URL, config and fetch and clones
// it per request (withOptions shares the token cache). A request under a
// different key replaces it, cache and all.
var anthropicFederationClient struct {
	mu         sync.Mutex
	key        string
	httpClient ai.HTTPDoer
	tokens     *anthropicTokenCache
}

// anthropicFederationTokens returns the token cache for this base URL, config
// and HTTP client, starting a fresh one when any of them changed. baseURL is
// the client's (model.baseUrl, or the default); httpClient is a custom client
// or nil for the default, pi's fetch.
func anthropicFederationTokens(baseURL string, f *anthropicFederation, httpClient ai.HTTPDoer) *anthropicTokenCache {
	// pi: JSON.stringify([model.baseUrl, federation]); any injective encoding
	// of the same values keys the same.
	keyBytes, _ := json.Marshal([]string{baseURL, f.ruleID, f.organizationID, f.identityTokenFile, f.serviceAccountID, f.workspaceID})
	key := string(keyBytes)
	c := &anthropicFederationClient
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tokens == nil || c.key != key || !sameHTTPDoer(c.httpClient, httpClient) {
		federation := *f
		c.key, c.httpClient = key, httpClient
		c.tokens = &anthropicTokenCache{exchange: func() (anthropicAccessToken, error) {
			return anthropicFederationExchange(baseURL, federation, httpClient)
		}}
	}
	return c.tokens
}

// sameHTTPDoer is pi's `federationClient.fetch !== fetch` identity test. A
// client of a type == cannot compare is never the same one, rather than a
// panic.
func sameHTTPDoer(a, b ai.HTTPDoer) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	t := reflect.TypeOf(a)
	return t == reflect.TypeOf(b) && t.Comparable() && a == b
}

// appendAnthropicOAuthBeta is the SDK's prepareRequest for token auth: once
// every header is merged, the OAuth beta joins anthropic-beta — appended, as
// Headers.append joins a value, after ", " — unless it is already one of the
// header's comma-separated values, trimmed. An empty header (the one an
// empty `betas` list sends) still counts as present, so it becomes
// ", oauth-2025-04-20".
func appendAnthropicOAuthBeta(h http.Header) {
	key := http.CanonicalHeaderKey("anthropic-beta")
	values, present := h[key]
	if !present {
		h.Set(key, anthropicOAuthAPIBeta)
		return
	}
	existing := strings.Join(values, ", ")
	for _, beta := range strings.Split(existing, ",") {
		if jstext.Trim(beta) == anthropicOAuthAPIBeta {
			return
		}
	}
	h.Set(key, existing+", "+anthropicOAuthAPIBeta)
}

// SDK 0.124.0 lib/credentials/types.ts.
const (
	anthropicTokenEndpoint        = "/v1/oauth/token"
	anthropicGrantTypeJWTBearer   = "urn:ietf:params:oauth:grant-type:jwt-bearer"
	anthropicOAuthAPIBeta         = "oauth-2025-04-20"
	anthropicFederationBeta       = "oidc-federation-2026-04-01"
	anthropicAdvisoryRefreshSecs  = 120
	anthropicMandatoryRefreshSecs = 30
	anthropicAdvisoryBackoffSecs  = 5
	anthropicMaxTokenResponse     = 1 << 20
	anthropicMaxErrorBodyChars    = 2000
	// anthropicFederationUserAgent is the User-Agent of the exchange: the SDK
	// passes its client's getUserAgent() as the provider's userAgent.
	anthropicFederationUserAgent = "Anthropic/JS " + anthropicSDKVersion
	anthropicSDKVersion          = "0.124.0"
)

// anthropicNowSeconds is the SDK's nowAsSeconds, Math.floor(Date.now()/1000).
// A variable only so a test can move the clock.
var anthropicNowSeconds = func() float64 { return float64(time.Now().Unix()) }

// anthropicAccessToken is the SDK's AccessToken. token is the template
// literal's String(access_token), or ok false where String() throws.
type anthropicAccessToken struct {
	token     string
	tokenOK   bool
	expiresAt float64
}

// anthropicTokenCache is the SDK's TokenCache: two-tier proactive refresh and
// concurrent deduplication over the exchange.
//
//   - no cached token, or a forced refresh: exchange (blocking);
//   - more than 120s left: the cached token;
//   - 30-120s left (advisory): the cached token, and a background exchange
//     whose failure is swallowed, backing off 5s after one;
//   - less than 30s left: exchange (blocking), failing on failure.
//
// Concurrent callers join the exchange in flight, unless forced. pi's
// federation exchange always has an expiry, so the SDK's never-expiring
// arm is not needed.
type anthropicTokenCache struct {
	exchange func() (anthropicAccessToken, error)

	mu                sync.Mutex
	cached            *anthropicAccessToken
	pending           *anthropicTokenRefresh
	nextForce         bool
	lastAdvisoryError float64
}

type anthropicTokenRefresh struct {
	done  chan struct{}
	token anthropicAccessToken
	err   error
}

// getToken is TokenCache.getToken, plus the String() of the template literal
// authHeaders writes it into. A ctx that ends while the caller waits returns
// errRequestAborted at once; the exchange runs on and fills the cache. (The
// SDK's wait cannot be cut short, but pi's request then fails as aborted all
// the same: retryProviderRequest checks the signal first.)
func (c *anthropicTokenCache) getToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	force := c.nextForce
	c.nextForce = false
	cached := c.cached
	var refresh *anthropicTokenRefresh
	switch {
	case force || cached == nil:
		refresh = c.refreshLocked(force)
	case cached.expiresAt-anthropicNowSeconds() > anthropicAdvisoryRefreshSecs:
	case cached.expiresAt-anthropicNowSeconds() > anthropicMandatoryRefreshSecs:
		c.backgroundRefreshLocked()
	default:
		refresh = c.refreshLocked(false)
	}
	c.mu.Unlock()

	token := cached
	if refresh != nil {
		var done <-chan struct{}
		if ctx != nil {
			done = ctx.Done()
		}
		select {
		case <-refresh.done:
		case <-done:
			return "", errRequestAborted
		}
		if refresh.err != nil {
			return "", refresh.err
		}
		token = &refresh.token
	}
	if !token.tokenOK {
		return "", errors.New("Cannot convert object to primitive value")
	}
	return token.token, nil
}

// invalidate is TokenCache.invalidate, after a 401 from a request that used
// the cached token.
func (c *anthropicTokenCache) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cached = nil
	c.nextForce = true
}

func (c *anthropicTokenCache) refreshLocked(force bool) *anthropicTokenRefresh {
	if c.pending != nil && !force {
		return c.pending
	}
	return c.doRefreshLocked()
}

func (c *anthropicTokenCache) backgroundRefreshLocked() {
	if c.pending != nil {
		return
	}
	if anthropicNowSeconds()-c.lastAdvisoryError < anthropicAdvisoryBackoffSecs {
		return
	}
	refresh := c.doRefreshLocked()
	go func() {
		<-refresh.done
		if refresh.err != nil {
			c.mu.Lock()
			c.lastAdvisoryError = anthropicNowSeconds()
			c.mu.Unlock()
		}
	}()
}

// doRefreshLocked starts an exchange and makes it the one in flight. As in
// the SDK, its settling clears the in-flight slot whichever exchange holds it.
func (c *anthropicTokenCache) doRefreshLocked() *anthropicTokenRefresh {
	refresh := &anthropicTokenRefresh{done: make(chan struct{})}
	c.pending = refresh
	go func() {
		token, err := c.exchange()
		c.mu.Lock()
		if err == nil {
			c.cached = &token
		}
		c.pending = nil
		refresh.token, refresh.err = token, err
		c.mu.Unlock()
		close(refresh.done)
	}()
	return refresh
}

// anthropicFederationExchange is the SDK's oidcFederationProvider: one RFC
// 7523 jwt-bearer exchange of the identity token for an access token, posted
// to the client's base URL (trailing slashes dropped, as
// resolveCredentialsFromConfig drops them). The identity token file is read
// afresh on every exchange, so a rotated token is picked up. The exchange is
// the shared token's, not any one request's: it runs under no request's
// context, as the SDK's fetch of it carries no signal, and through the
// request's client (pi's fetch), with no timeout of the SDK's own.
func anthropicFederationExchange(baseURL string, f anthropicFederation, httpClient ai.HTTPDoer) (anthropicAccessToken, error) {
	baseURL = strings.TrimRight(baseURL, "/")
	if err := requireSecureTokenEndpoint(baseURL); err != nil {
		return anthropicAccessToken{}, err
	}
	jwt, err := readIdentityToken(f.identityTokenFile)
	if err != nil {
		return anthropicAccessToken{}, err
	}
	// The token endpoint enforces a 16 KiB assertion limit (a length in UTF-16
	// code units, as JavaScript measures it).
	if n := utf16Len(jwt); n > 16*1024 {
		return anthropicAccessToken{}, fmt.Errorf("Identity token is %d KiB, exceeds the 16 KiB assertion limit", (n+1023)/1024)
	}

	// JSON.stringify of the body object, in its insertion order.
	fields := [][2]string{
		{"grant_type", anthropicGrantTypeJWTBearer},
		{"assertion", jwt},
		{"federation_rule_id", f.ruleID},
		{"organization_id", f.organizationID},
	}
	if f.serviceAccountID != "" {
		fields = append(fields, [2]string{"service_account_id", f.serviceAccountID})
	}
	if f.workspaceID != "" {
		fields = append(fields, [2]string{"workspace_id", f.workspaceID})
	}
	var body bytes.Buffer
	body.WriteByte('{')
	for i, kv := range fields {
		if i > 0 {
			body.WriteByte(',')
		}
		k, _ := jstext.Stringify(kv[0])
		v, _ := jstext.Stringify(kv[1])
		body.WriteString(k + ":" + v)
	}
	body.WriteByte('}')

	endpoint := baseURL + anthropicTokenEndpoint
	resp, err := postTokenExchange(endpoint, body.Bytes(), httpClient)
	if err != nil {
		return anthropicAccessToken{}, fmt.Errorf("Failed to reach token endpoint %s: %s", endpoint, err)
	}
	defer resp.Body.Close()
	// Headers.get joins a repeated header's values.
	requestID := strings.Join(resp.Header.Values("Request-Id"), ", ")

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// resp.text().catch(() => ''): a body that fails to read is empty.
		raw, err := io.ReadAll(resp.Body)
		text := ""
		if err == nil {
			text = jstext.DecodeText(raw)
		}
		redacted := redactSensitiveText(text)
		// A 401 is hard to debug from the status alone, so the SDK adds
		// guidance; other statuses get none.
		hint := ""
		if resp.StatusCode == 401 {
			middle := ""
			if f.workspaceID == "" {
				middle = "If your federation rule is scoped to multiple workspaces, set the ANTHROPIC_WORKSPACE_ID environment variable, the 'workspace_id' config key, or the `workspaceId` option. "
			}
			hint = " Ensure your federation rule matches your identity token. " + middle +
				"View your authentication events in the Workload identity page of Claude Console for more details."
		}
		rid := ""
		if requestID != "" {
			rid = " (request-id " + requestID + ")"
		}
		return anthropicAccessToken{}, fmt.Errorf("Token exchange failed with status %d%s: %s%s", resp.StatusCode, rid, redacted, hint)
	}

	data, redacted, err := parseTokenResponse(resp)
	if err != nil {
		return anthropicAccessToken{}, err
	}
	expiresIn := math.NaN()
	if v, present := data["expires_in"]; present {
		var ok bool
		if expiresIn, ok = jstext.ToNumber(v); !ok {
			return anthropicAccessToken{}, errors.New("Cannot convert object to primitive value")
		}
	}
	if math.IsNaN(expiresIn) || math.IsInf(expiresIn, 0) {
		return anthropicAccessToken{}, fmt.Errorf("Token endpoint response missing required fields: %s", redacted)
	}
	token, ok := jstext.ToString(data["access_token"])
	return anthropicAccessToken{token: token, tokenOK: ok, expiresAt: anthropicNowSeconds() + expiresIn}, nil
}

// postTokenExchange sends the exchange as the SDK's fetch does: its three
// headers, and the default client's failure as undici's TypeError.
func postTokenExchange(endpoint string, body []byte, httpClient ai.HTTPDoer) (*http.Response, error) {
	target, err := requestURL(endpoint)
	if err != nil {
		return nil, fmt.Errorf("TypeError: Failed to parse URL from %s", endpoint)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-beta", anthropicOAuthAPIBeta+","+anthropicFederationBeta)
	req.Header.Set("User-Agent", anthropicFederationUserAgent)
	if httpClient != nil {
		return httpClient.Do(req)
	}
	resp, err := sharedClient(undiciHeadersTimeoutMs).Do(req)
	if err != nil {
		return nil, fmt.Errorf("TypeError: %w", undiciFetchError(err))
	}
	return resp, nil
}

// requireSecureTokenEndpoint is the SDK's: it refuses a base URL that would
// send the identity token over cleartext HTTP, loopback hosts excepted.
func requireSecureTokenEndpoint(baseURL string) error {
	if baseURL == "" {
		return nil
	}
	normalized, err := requestURL(baseURL)
	var u *url.URL
	if err == nil {
		u, err = url.Parse(normalized)
	}
	if err != nil {
		return fmt.Errorf("Invalid token endpoint base URL \"%s\": TypeError: Invalid URL", baseURL)
	}
	if u.Scheme == "https" {
		return nil
	}
	// net/url's Hostname drops an IPv6 literal's brackets, which the SDK
	// strips by hand.
	host := strings.ToLower(u.Hostname())
	if u.Scheme == "http" && (host == "localhost" || host == "127.0.0.1" || host == "::1") {
		return nil
	}
	return fmt.Errorf("Refusing to send credential over non-https token endpoint \"%s\"", baseURL)
}

// readIdentityToken is the SDK's identityTokenFromFile provider: the file
// read as UTF-8 on every call, trimmed as String.prototype.trim trims. A read
// error is quoted as node's fs error stringifies.
func readIdentityToken(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("Failed to read identity token file at %s: %s", path, nodeFSError(err, path))
	}
	token := jstext.Trim(jstext.DecodeUTF8(raw))
	if token == "" {
		return "", fmt.Errorf("Identity token file at %s is empty", path)
	}
	return token, nil
}

// nodeFSError is `${err}` for the error fs.promises.readFile rejects with,
// for the failures a token file meets; any other is Go's own text.
func nodeFSError(err error, path string) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Sprintf("Error: ENOENT: no such file or directory, open '%s'", path)
	case errors.Is(err, fs.ErrPermission):
		return fmt.Sprintf("Error: EACCES: permission denied, open '%s'", path)
	case errors.Is(err, syscall.EISDIR):
		return "Error: EISDIR: illegal operation on a directory, read"
	}
	return "Error: " + err.Error()
}

// parseTokenResponse is the SDK's: the body read up to 1 MiB and decoded as
// TextDecoder does, parsed as JSON, then access_token required and a
// token_type other than Bearer refused. data is the parsed object; a JSON
// value that is not an object has no access_token, null aside, which
// JavaScript cannot read a property of.
//
// redacted is JSON.stringify(redactSensitive(data)), which the caller's own
// error quotes too, built from an ordered parse: the SDK's keeps the body's
// key order, which data, a map, has lost.
func parseTokenResponse(resp *http.Response) (data map[string]any, redacted string, err error) {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, anthropicMaxTokenResponse))
	text := []byte(jstext.DecodeText(raw))
	parsed, err := jstext.Parse(text)
	if err != nil {
		return nil, "", fmt.Errorf("Token endpoint returned non-JSON response (status %d)", resp.StatusCode)
	}
	if parsed == nil {
		return nil, "", errors.New("Cannot read properties of null (reading 'access_token')")
	}
	ordered, _ := ai.DecodeOrderedValue(text)
	redacted = redactedValueJSON(ordered)
	data, _ = parsed.(map[string]any)
	if !jstext.Truthy(data["access_token"]) {
		return nil, "", fmt.Errorf("Token endpoint response missing access_token: %s", redacted)
	}
	if tokenType := data["token_type"]; jstext.Truthy(tokenType) {
		s, ok := tokenType.(string)
		if !ok {
			return nil, "", errors.New("data.token_type.toLowerCase is not a function")
		}
		if jstext.ToLower(s) != "bearer" {
			return nil, "", fmt.Errorf("Token endpoint response: unsupported token_type \"%s\" (want Bearer)", s)
		}
	}
	return data, redacted, nil
}

// anthropicSafeErrorKeys are the RFC 6749 §5.2 error-response fields, the only
// members of a token endpoint body the SDK lets into an error.
var anthropicSafeErrorKeys = []string{"error", "error_description", "error_uri"}

// redactSensitiveText is the SDK's redactSensitive for an error body's text:
// JSON is re-written redacted (redactedValueJSON), anything else kept, cut to
// 2000 UTF-16 code units with a count of the rest.
func redactSensitiveText(text string) string {
	if parsed, err := ai.DecodeOrderedValue([]byte(text)); err == nil {
		return redactedValueJSON(parsed)
	}
	units := utf16.Encode([]rune(text))
	if len(units) <= anthropicMaxErrorBodyChars {
		return text
	}
	return string(utf16.Decode(units[:anthropicMaxErrorBodyChars])) +
		fmt.Sprintf("... <%d more chars>", len(units)-anthropicMaxErrorBodyChars)
}

// redactedValueJSON is JSON.stringify(redactSensitive(v)) for a value
// ai.DecodeOrderedValue parsed: an object keeps only the RFC 6749 error
// fields, in its own order, a string is redacted as text, and anything else
// is null.
func redactedValueJSON(v any) string {
	switch x := v.(type) {
	case string:
		s, _ := jstext.Stringify(redactSensitiveText(x))
		return s
	case ai.OrderedObject:
		kept := ai.OrderedObject{}
		for _, field := range x {
			if isSafeErrorKey(field.Key) {
				kept = append(kept, field)
			}
		}
		s, _ := jstext.Stringify(kept)
		return s
	}
	return "null"
}

func isSafeErrorKey(k string) bool {
	for _, safe := range anthropicSafeErrorKeys {
		if k == safe {
			return true
		}
	}
	return false
}

// utf16Len is a string's JavaScript length.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}
