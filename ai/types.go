package ai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"time"

	"github.com/sky-valley/pi/telemetry"
)

// Api identifies a wire protocol / API shape. Known values mirror pi's KnownApi
// but any string is accepted (custom providers).
type Api = string

const (
	APIOpenAICompletions     Api = "openai-completions"
	APIMistralConversations  Api = "mistral-conversations"
	APIOpenAIResponses       Api = "openai-responses"
	APIAzureOpenAIResponses  Api = "azure-openai-responses"
	APIOpenAICodexResponses  Api = "openai-codex-responses"
	APIAnthropicMessages     Api = "anthropic-messages"
	APIBedrockConverseStream Api = "bedrock-converse-stream"
	APIGoogleGenerativeAI    Api = "google-generative-ai"
	APIGoogleVertex          Api = "google-vertex"
	APIPiMessages            Api = "pi-messages"
)

// ProviderId identifies a model provider (e.g. "anthropic", "openai"). pi
// renamed this from Provider to ProviderId in the model-registry merge
// (732bb161), freeing Provider for the runtime Provider interface (see
// models_runtime.go).
type ProviderId = string

// ToolChoice is the provider-neutral tool selection for simple requests (pi
// ToolChoice, upstream e5dde9a76). The empty value is pi's absent option; what
// that means is the adapter's business, not this type's (upstream 6b36eb592
// stopped documenting it as "auto").
type ToolChoice string

const (
	ToolChoiceAuto ToolChoice = "auto"
	ToolChoiceNone ToolChoice = "none"
)

// ThinkingLevel is a reasoning effort level understood by the unified API.
type ThinkingLevel string

const (
	ThinkingMinimal ThinkingLevel = "minimal"
	ThinkingLow     ThinkingLevel = "low"
	ThinkingMedium  ThinkingLevel = "medium"
	ThinkingHigh    ThinkingLevel = "high"
	ThinkingXHigh   ThinkingLevel = "xhigh"
	ThinkingMax     ThinkingLevel = "max"
)

// ModelThinkingLevel adds "off" to the reasoning levels.
type ModelThinkingLevel string

// ThinkingLevelMap maps pi thinking levels to provider/model-specific values.
// A nil pointer value marks a level as unsupported.
type ThinkingLevelMap map[ModelThinkingLevel]*string

// ThinkingBudgets holds token budgets per thinking level (token-based providers).
type ThinkingBudgets struct {
	Minimal *int `json:"minimal,omitempty"`
	Low     *int `json:"low,omitempty"`
	Medium  *int `json:"medium,omitempty"`
	High    *int `json:"high,omitempty"`
}

// CacheRetention is the prompt cache retention preference.
type CacheRetention string

const (
	CacheNone  CacheRetention = "none"
	CacheShort CacheRetention = "short"
	CacheLong  CacheRetention = "long"
)

// ModelPromptCache is the best-effort prompt cache lifetime in seconds for each
// retention tier a request can ask for (pi ModelPromptCache,
// `Partial<Record<Exclude<CacheRetention, "none">, number>>`). A nil tier means
// the lifetime is unknown, which is not the same as zero — pi does not warm a
// cache whose lifetime it cannot predict, so the distinction is load-bearing
// and both tiers are pointers.
type ModelPromptCache struct {
	Short *float64 `json:"short,omitempty"`
	Long  *float64 `json:"long,omitempty"`
}

// ProviderHeaders are custom HTTP headers for provider requests (pi
// types.ts ProviderHeaders = Record<string, string | null>). A header name has
// three distinct states:
//
//	absent           — send whatever the provider/API would send by default
//	present, non-nil — send that value (an empty string sends an empty header)
//	present, nil     — deletion marker: suppress the default header entirely
//
// The nil marker is how a consumer turns a provider default OFF, which no
// string value can express; Cloudflare AI Gateway uses it to stop the
// upstream provider's Authorization/x-api-key from being sent while its own
// key rides in cf-aig-authorization. Marshalling round-trips all three states:
// an explicit `null` in catalog/model JSON decodes to a nil marker, not to an
// absent key.
//
// Values are SHARED, not copied: merging headers (mergeHeaders here,
// headerObject.merge in ai/providers) copies the *string pointers, so a
// merged map — including the one handed to a TransformHeaders hook — aliases
// the *string values inside a Model.Headers catalog entry that other requests
// read concurrently. Treat a *string value as immutable: to change a header,
// replace the pointer (h["X"] = HeaderValue("v")), never write through it
// (*h["X"] = "v"), which would mutate the model for every other request.
type ProviderHeaders map[string]*string

// HeaderValue returns a present header value for a ProviderHeaders entry. Use
// a nil map entry for a deletion marker; an absent key is the third state. The
// returned pointer is fresh, so it is safe to store in a shared map — see
// ProviderHeaders on why the pointed-to string must not be written through
// once it is in one.
func HeaderValue(v string) *string { return &v }

// Transport is the preferred transport for providers that support several.
type Transport string

const (
	TransportSSE             Transport = "sse"
	TransportWebSocket       Transport = "websocket"
	TransportWebSocketCached Transport = "websocket-cached"
	TransportAuto            Transport = "auto"
)

// StopReason describes why an assistant turn ended.
type StopReason string

const (
	// StopPending marks a partial assistant message whose stream has not yet
	// reported a terminal reason. It is reserved for in-flight streaming events;
	// a provider must replace it with a completion reason before the stream ends,
	// so it never appears on a persisted (done/error) message.
	StopPending StopReason = "pending"
	StopStop    StopReason = "stop"
	StopLength  StopReason = "length"
	StopToolUse StopReason = "toolUse"
	StopError   StopReason = "error"
	StopAborted StopReason = "aborted"
	// StopDeferred marks a submission the provider accepted but has not
	// finished: the message carries a DeferredHandle instead of content, and
	// the response is redeemed later through Models.FetchDeferred (pi
	// 382aa641c). Protocol v1 cannot represent it — the server bridge refuses
	// such a message rather than putting an unknown reason on the wire.
	StopDeferred StopReason = "deferred"
)

// DeferredHandle is a durable reference to a response a provider accepted and
// is producing asynchronously (pi DeferredHandle, upstream 382aa641c). It is
// what a "deferred" assistant message carries in place of content, and what
// FetchDeferred/CancelDeferred take to redeem or drop that response.
type DeferredHandle struct {
	Provider ProviderId `json:"provider"`
	ModelID  string     `json:"modelId"`
	Api      Api        `json:"api"`
	// ID is the provider's own token: a response id, or a batch id plus row id.
	ID string `json:"id"`
	// ExpiresAt is when the provider stops honoring the handle, in Unix
	// milliseconds; zero when the provider did not say.
	ExpiresAt int64 `json:"expiresAt,omitempty"`
	// PollAfterMs is how long the provider asks callers to wait before fetching
	// again; zero when the provider did not say.
	PollAfterMs int64 `json:"pollAfterMs,omitempty"`
	// Data is provider conversion data needed to reconstruct the final
	// assistant message. pi types it as `JsonValue`, a compile-time constraint
	// with no Go counterpart; encoding/json imposes the same requirement at
	// marshal time, so this is `any` like ToolResultMessage.Details.
	Data any `json:"data,omitempty"`
}

// DeferredWindow is how long a provider should keep working on a deferred
// submission (pi SimpleStreamOptions.deferred.window).
type DeferredWindow string

const (
	DeferredWindow15m DeferredWindow = "15m"
	DeferredWindow1h  DeferredWindow = "1h"
	DeferredWindow24h DeferredWindow = "24h"
)

// DeferredRequest asks a capable provider to return a DeferredHandle and carry
// on with the request asynchronously. pi writes this as
// `deferred?: boolean | { window?: ... }`; Go collapses the union onto a
// pointer, so a non-nil DeferredRequest with an empty Window is pi's
// `deferred: true`.
type DeferredRequest struct {
	Window DeferredWindow
}

// DeferredFetchOptions are the options for redeeming a DeferredHandle
// (pi DeferredFetchOptions). Redeeming re-reads a response the provider is
// already producing, so it carries only ProviderRequestOptions: the sampling,
// transport and metadata fields that shaped the submission have nothing left
// to act on and are no longer accepted here (upstream 686f193e5).
type DeferredFetchOptions struct {
	ProviderRequestOptions
	// Wait bounds the provider's long poll for a terminal response. Zero checks
	// once; nil leaves the wait to the provider (pi's `wait?: number` in
	// milliseconds).
	Wait *time.Duration
}

// DeferredCancelOptions are the options for best-effort cancellation of a
// deferred response (pi DeferredCancelOptions). Cancelling shapes no request
// body at all, so it is ProviderRequestOptions exactly — an alias, as upstream
// writes it, so an auth-applied ProviderRequestOptions is one already.
type DeferredCancelOptions = ProviderRequestOptions

// Role identifies a message author.
type Role string

const (
	RoleSystem     Role = "system"
	RoleUser       Role = "user"
	RoleAssistant  Role = "assistant"
	RoleToolResult Role = "toolResult"
)

// ---------------------------------------------------------------------------
// Content blocks
// ---------------------------------------------------------------------------

// Content is a single content block within a message. Implemented by
// TextContent, ThinkingContent, ImageContent and ToolCall.
type Content interface {
	contentType() string
}

// TextContent is a text block.
type TextContent struct {
	Text string `json:"text"`
	// TextSignature carries provider message metadata (e.g. OpenAI responses).
	TextSignature string `json:"textSignature,omitempty"`
}

func (TextContent) contentType() string { return "text" }

// ThinkingContent is a reasoning/thinking block.
type ThinkingContent struct {
	Thinking string `json:"thinking"`
	// ThinkingSignature carries provider-specific opaque or serialized reasoning
	// replay data: an OpenAI Responses reasoning item id, the openai-completions
	// reasoning field name, or a serialized JSON array of that API's
	// reasoning_details. Persisted in the session, so treat it as opaque and
	// replay it unmodified.
	ThinkingSignature string `json:"thinkingSignature,omitempty"`
	// Redacted marks thinking content removed by safety filters; the opaque
	// encrypted payload is kept in ThinkingSignature for multi-turn continuity.
	Redacted bool `json:"redacted,omitempty"`
}

func (ThinkingContent) contentType() string { return "thinking" }

// ImageContent is a base64-encoded image block.
type ImageContent struct {
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
}

func (ImageContent) contentType() string { return "image" }

// ToolCall is a tool invocation requested by the assistant.
type ToolCall struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
	// ArgumentsOrder is Arguments in the order pi's JS object lists its keys,
	// recorded when the arguments were decoded (from a stream or from a stored
	// session): keys that are array indices first, ascending, then the rest in
	// the order they first appeared (OrdinaryOwnPropertyKeys, as JSON.parse's
	// object has them). A Go map keeps no order, and the order is
	// model-visible wherever the arguments are replayed into a request.
	// Arguments stays authoritative: ArgumentsOrder is only used for
	// serialization while the two still agree, so leaving it behind when
	// Arguments is replaced costs the order, never the values.
	// It is never persisted — a stored session records the order as the key
	// order of the "arguments" object itself, exactly as pi writes it.
	ArgumentsOrder OrderedObject `json:"-"`
	// ThoughtSignature is a Google-specific opaque signature for reusing thought context.
	ThoughtSignature string `json:"thoughtSignature,omitempty"`
	// Namespace is the OpenAI Responses namespace for calls to dynamically
	// loaded or namespaced tools (pi 02bd2d1c6 `namespace?`). It is echoed back
	// when the call is replayed, but only for a call the current model could
	// have made — see canReplayNamespace in ai/providers/openai_responses.go.
	// Plain string like ThoughtSignature: pi's guard is `!== undefined`, so a
	// provider-sent empty string would replay there and is dropped here.
	Namespace string `json:"namespace,omitempty"`
	// ArgumentsError is set when the model's complete arguments text was not
	// a valid JSON object (pi 87f9a3641 `argumentsError?`); Arguments is then
	// empty. It names the problem and quotes the text the model sent, and
	// ValidateToolArguments fails with it, so the model sees what it sent.
	ArgumentsError string `json:"argumentsError,omitempty"`
}

func (ToolCall) contentType() string { return "toolCall" }

// OrderedArguments returns the arguments to serialize: the recorded ordered
// form when it still matches Arguments, and Arguments itself otherwise (a nil
// Arguments stays nil, and so still marshals to null).
func (t ToolCall) OrderedArguments() any {
	if t.ArgumentsOrder != nil && reflect.DeepEqual(t.ArgumentsOrder.Plain(), t.Arguments) {
		return t.ArgumentsOrder
	}
	return t.Arguments
}

// MarshalJSON writes arguments in the order pi's JS object lists their keys
// (see ArgumentsOrder).
func (t ToolCall) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ID               string `json:"id"`
		Name             string `json:"name"`
		Arguments        any    `json:"arguments"`
		ThoughtSignature string `json:"thoughtSignature,omitempty"`
		Namespace        string `json:"namespace,omitempty"`
		ArgumentsError   string `json:"argumentsError,omitempty"`
	}{
		ID:               t.ID,
		Name:             t.Name,
		Arguments:        t.OrderedArguments(),
		ThoughtSignature: t.ThoughtSignature,
		Namespace:        t.Namespace,
		ArgumentsError:   t.ArgumentsError,
	})
}

// UnmarshalJSON recovers the argument key order from the source bytes, as
// JSON.parse's object lists them, so a tool call reloaded from a session
// replays in the order pi's would.
func (t *ToolCall) UnmarshalJSON(data []byte) error {
	var raw struct {
		ID               string          `json:"id"`
		Name             string          `json:"name"`
		Arguments        json.RawMessage `json:"arguments"`
		ThoughtSignature string          `json:"thoughtSignature,omitempty"`
		Namespace        string          `json:"namespace,omitempty"`
		ArgumentsError   string          `json:"argumentsError,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*t = ToolCall{ID: raw.ID, Name: raw.Name, ThoughtSignature: raw.ThoughtSignature, Namespace: raw.Namespace, ArgumentsError: raw.ArgumentsError}
	if len(raw.Arguments) == 0 {
		return nil
	}
	args, order, err := DecodeOrderedObject(raw.Arguments)
	if err != nil {
		// Not an object (a null, most likely): decode it the plain way and let
		// json report anything genuinely malformed.
		return json.Unmarshal(raw.Arguments, &t.Arguments)
	}
	t.Arguments, t.ArgumentsOrder = args, order
	return nil
}

// marshalContent serializes a content block with its "type" discriminator
// first and the block's own fields after it in declaration order — pi's literal
// shape (`{type: "text", text}`), and so the key order a pi session file
// carries. The block types hold no "type" field of their own.
//
// A nil block is a hole in the content array (a pi-messages backend that
// starts a block past the end of it), which JSON.stringify writes null.
func marshalContent(c Content) ([]byte, error) {
	if c == nil {
		return []byte("null"), nil
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) < 2 || raw[0] != '{' || raw[len(raw)-1] != '}' {
		return nil, fmt.Errorf("ai: %s content block did not serialize to a JSON object: %s", c.contentType(), raw)
	}
	t, _ := json.Marshal(c.contentType())
	var buf bytes.Buffer
	buf.WriteString(`{"type":`)
	buf.Write(t)
	if fields := bytes.TrimSpace(raw[1 : len(raw)-1]); len(fields) > 0 {
		buf.WriteByte(',')
		buf.Write(fields)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// unmarshalContent decodes a content block based on its "type" discriminator.
// A null is the hole marshalContent writes, which JSON.parse reads back as a
// null element; it decodes to a nil block.
func unmarshalContent(data []byte) (Content, error) {
	if string(bytes.TrimSpace(data)) == "null" {
		return nil, nil
	}
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, err
	}
	switch head.Type {
	case "text":
		var c TextContent
		err := json.Unmarshal(data, &c)
		return c, err
	case "thinking":
		var c ThinkingContent
		err := json.Unmarshal(data, &c)
		return c, err
	case "image":
		var c ImageContent
		err := json.Unmarshal(data, &c)
		return c, err
	case "toolCall":
		var c ToolCall
		err := json.Unmarshal(data, &c)
		return c, err
	default:
		return nil, fmt.Errorf("unknown content type: %q", head.Type)
	}
}

// ContentList is a slice of heterogeneous content blocks with discriminated JSON.
type ContentList []Content

// MarshalJSON encodes each block with a "type" discriminator.
func (cl ContentList) MarshalJSON() ([]byte, error) {
	if cl == nil {
		return []byte("[]"), nil
	}
	parts := make([]json.RawMessage, len(cl))
	for i, c := range cl {
		raw, err := marshalContent(c)
		if err != nil {
			return nil, err
		}
		parts[i] = raw
	}
	return json.Marshal(parts)
}

// UnmarshalJSON decodes a discriminated content array.
func (cl *ContentList) UnmarshalJSON(data []byte) error {
	var raws []json.RawMessage
	if err := json.Unmarshal(data, &raws); err != nil {
		return err
	}
	out := make(ContentList, 0, len(raws))
	for _, raw := range raws {
		c, err := unmarshalContent(raw)
		if err != nil {
			return err
		}
		out = append(out, c)
	}
	*cl = out
	return nil
}

// ---------------------------------------------------------------------------
// Usage
// ---------------------------------------------------------------------------

// CostBreakdown holds the per-bucket dollar cost of a request. The four
// buckets are catalog-derived; Total is the best available total (upstream
// cae8d69e5): the provider's billed cost when it reports one, otherwise the
// catalog estimate.
type CostBreakdown struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
	Total      float64 `json:"total"`
	// TotalFromProvider is true when Total is the billed cost the provider
	// reported, false when it is a catalog estimate; unset also means false
	// (pi `totalFromProvider?`, upstream 7c22a93d0). CalculateCost sets it
	// false, so every cost it computes carries the key, while a cost nothing
	// computed (a zero literal) does not.
	TotalFromProvider OptBool `json:"totalFromProvider,omitzero"`
}

// OptBool is an optional JavaScript boolean property: unset, false or true.
// Its zero value is unset, which omitzero leaves out of the JSON. It is a
// value rather than a *bool so a struct holding it, such as Usage, still
// compares by value with ==.
type OptBool uint8

const (
	OptBoolUnset OptBool = iota
	OptBoolFalse
	OptBoolTrue
)

// OptBoolOf is the set OptBool holding v.
func OptBoolOf(v bool) OptBool {
	if v {
		return OptBoolTrue
	}
	return OptBoolFalse
}

// True is `=== true`: unset and false are both false.
func (b OptBool) True() bool { return b == OptBoolTrue }

// MarshalJSON writes true or false; omitzero keeps an unset OptBool out.
func (b OptBool) MarshalJSON() ([]byte, error) {
	if b == OptBoolUnset {
		return []byte("null"), nil
	}
	return json.Marshal(b == OptBoolTrue)
}

// UnmarshalJSON reads true or false; null leaves it unset.
func (b *OptBool) UnmarshalJSON(data []byte) error {
	if string(bytes.TrimSpace(data)) == "null" {
		*b = OptBoolUnset
		return nil
	}
	var v bool
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*b = OptBoolOf(v)
	return nil
}

// Usage holds token counts and cost for a request.
type Usage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cacheRead"`
	CacheWrite int `json:"cacheWrite"`
	// Reasoning is the count of reasoning/thinking tokens, when the provider
	// reports them. This is a subset of Output: Output already includes these
	// tokens. Set to a number (possibly 0) by providers that expose a reasoning
	// breakdown; left unset by providers that don't (pi: Usage.reasoning, optional).
	//
	// Faithfulness note on omitempty: pi leaves reasoning `undefined` when a
	// provider doesn't report it, but the OpenAI completions/responses and Google
	// paths set `reasoning: ... || 0` unconditionally, so those providers always
	// emit reasoning (0 when absent). With omitempty a 0 is dropped from the JSON,
	// which differs from pi emitting `reasoning: 0` for those providers. We accept
	// this divergence to keep session goldens byte-identical when reasoning is
	// 0/unset; the in-memory value (0) is identical either way.
	Reasoning   int           `json:"reasoning,omitempty"`
	TotalTokens int           `json:"totalTokens"`
	Cost        CostBreakdown `json:"cost"`
	// CacheWrite1h is the part of the cache write done with 1h retention, meant
	// as a subset of CacheWrite. Only Anthropic reports this split (pi:
	// Usage.cacheWrite1h, optional). It is not guaranteed to be at most
	// CacheWrite: a message_delta may carry the breakdown without the total
	// (upstream 667fc3dd3), and the two are then taken as reported. pi assigns
	// it after the usage object exists, so it is the last key.
	CacheWrite1h int `json:"cacheWrite1h,omitempty"`
}

// ---------------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------------

// Message is a SystemMessage, UserMessage, AssistantMessage, or
// ToolResultMessage.
type Message interface {
	MessageRole() Role
}

// SystemMessage carries system instructions and tool declarations at one point
// in the transcript (pi SystemMessage, upstream 9e05370b2).
//
// The leading system message is the system prompt. Later system messages change
// it: Content adds instructions from that point on, Sections replace or remove
// named prompt sections, and ToolsAdded/ToolsRemoved change the tool set.
// Replaying every system message in order yields the current prompt and tools.
// Providers that accept system messages mid-conversation send each one in
// place; other providers rebuild the leading system message from the replayed
// state.
//
// JSON: pi's content is `string | TextContent[]`. Content decoded from the
// string form, or built with NewSystemText, is re-emitted as a string; so is a
// nil Content, which is how pi's own producers spell an empty prompt (`""`).
// ToolsAdded, ToolsRemoved and Sections are omitted when nil and emitted when
// non-nil, even empty, as a JS object carrying `toolsAdded: []` would be.
//
// Key order follows the producer, because pi serializes a plain object and the
// bytes are shared with pi on the pi-messages wire and in session files: a
// message decoded from JSON keeps its document order, WithToolChanges appends
// the tool keys after everything else (agent-loop.ts `withToolChanges`), and
// every other message uses role, content, sections, toolsAdded, toolsRemoved,
// timestamp — the order of pi's createInitialSystemMessage and
// getCurrentSystemMessage literals. Decoded and WithToolChanges messages are JS
// objects with an own-key order, so a key a caller later sets that the message
// did not carry is appended after the recorded ones, as a JS property
// assignment appends — even when the recorded keys happen to be in default
// order. Several such keys follow in default order (Go cannot see the order
// they were assigned in). A constructed message has no recorded order: its
// keys take their default slots whenever they are set.
type SystemMessage struct {
	// Content is instruction text (TextContent blocks). On the leading message
	// this is the base prompt; later, additional instructions.
	Content ContentList
	// Sections are named, ordered prompt sections rendered verbatim after
	// Content. The leading message declares them; later messages replace
	// sections by name, and a nil value removes one. Nil means absent.
	Sections SystemSections
	// ToolsAdded holds complete definitions of tools that become available at
	// this point.
	ToolsAdded []Tool
	// ToolsRemoved names tools that stop being available at this point.
	ToolsRemoved []ToolReference
	// Timestamp is the Unix timestamp in milliseconds.
	Timestamp int64

	// contentWasString records that Content is pi's string form (decoded from
	// a JSON string, or built by NewSystemText).
	contentWasString bool
	// keyOrder is the JSON key order recorded by decoding or WithToolChanges;
	// nil is the default literal order. See the type comment.
	keyOrder []string
}

func (SystemMessage) MessageRole() Role { return RoleSystem }

// NewSystemText builds a system message whose content is the plain-string form
// pi's producers use (`content: systemPrompt ?? ""`).
func NewSystemText(text string, timestamp int64) SystemMessage {
	return SystemMessage{Content: ContentList{TextContent{Text: text}}, Timestamp: timestamp, contentWasString: true}
}

// StringContent reports whether the message's content is the plain-string form,
// returning that string. A nil Content is the empty string.
func (m SystemMessage) StringContent() (string, bool) {
	if m.Content == nil {
		return "", true
	}
	if m.contentWasString && len(m.Content) == 1 {
		if t, ok := m.Content[0].(TextContent); ok {
			return t.Text, true
		}
	}
	return "", false
}

// systemMessageKeys is the default key order: pi's createInitialSystemMessage
// and getCurrentSystemMessage literals, which every other literal producer in
// range agrees with.
var systemMessageKeys = []string{"role", "content", "sections", "toolsAdded", "toolsRemoved", "timestamp"}

// hasKey reports whether key is one of the message's own JSON properties.
func (m SystemMessage) hasKey(key string) bool {
	switch key {
	case "role", "content", "timestamp":
		return true
	case "sections":
		return m.Sections != nil
	case "toolsAdded":
		return m.ToolsAdded != nil
	case "toolsRemoved":
		return m.ToolsRemoved != nil
	}
	return false
}

// layout is the message's JSON key order: the recorded order, then any key it
// does not name in default order (a JS property assignment appends).
func (m SystemMessage) layout() []string {
	if m.keyOrder == nil {
		return systemMessageKeys
	}
	out := append([]string(nil), m.keyOrder...)
	for _, key := range systemMessageKeys {
		if !slices.Contains(m.keyOrder, key) {
			out = append(out, key)
		}
	}
	return out
}

// withKeyOrder records order as the message's own-key order. It is kept even
// when it matches the default order: a key set later appends after it.
func (m SystemMessage) withKeyOrder(order []string) SystemMessage {
	m.keyOrder = append(make([]string, 0, len(order)), order...)
	return m
}

// WithToolChanges returns a copy of m whose tool fields are replaced by changes,
// omitting an empty list. It is the Go home of agent-loop.ts `withToolChanges`,
// `{...rest, toolsAdded?, toolsRemoved?}`: the spread keeps every other own key
// in place and the tool keys follow them, after timestamp.
func (m SystemMessage) WithToolChanges(changes ToolStateChanges) SystemMessage {
	var order []string
	for _, key := range m.layout() {
		if key != "toolsAdded" && key != "toolsRemoved" && m.hasKey(key) {
			order = append(order, key)
		}
	}
	m.ToolsAdded, m.ToolsRemoved = nil, nil
	if len(changes.ToolsAdded) > 0 {
		m.ToolsAdded = changes.ToolsAdded
		order = append(order, "toolsAdded")
	}
	if len(changes.ToolsRemoved) > 0 {
		m.ToolsRemoved = changes.ToolsRemoved
		order = append(order, "toolsRemoved")
	}
	return m.withKeyOrder(order)
}

// MarshalJSON writes the role discriminator and the producer's key order.
func (m SystemMessage) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	first := true
	for _, key := range m.layout() {
		if !m.hasKey(key) {
			continue
		}
		var value any
		switch key {
		case "role":
			value = RoleSystem
		case "content":
			if s, ok := m.StringContent(); ok {
				value = s
			} else {
				value = m.Content
			}
		case "sections":
			value = m.Sections
		case "toolsAdded":
			value = m.ToolsAdded
		case "toolsRemoved":
			value = m.ToolsRemoved
		case "timestamp":
			value = m.Timestamp
		}
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		if !first {
			buf.WriteByte(',')
		}
		first = false
		buf.WriteString(`"` + key + `":`)
		buf.Write(raw)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// UnmarshalJSON accepts content as a string, a text-block array, or null
// (read as the empty string), and records the document's key order. Keys the
// type does not model are dropped.
func (m *SystemMessage) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil {
		return err
	} else if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return fmt.Errorf("ai: system message must be a JSON object, got %s", bytes.TrimSpace(data))
	}
	out := SystemMessage{}
	var order []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := tok.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
		}
		switch key {
		case "role":
		case "content":
			out.Content, out.contentWasString = nil, false
			switch {
			case string(raw) == "null":
			case raw[0] == '"':
				var s string
				if err := json.Unmarshal(raw, &s); err != nil {
					return err
				}
				out.Content, out.contentWasString = ContentList{TextContent{Text: s}}, true
			default:
				if err := json.Unmarshal(raw, &out.Content); err != nil {
					return fmt.Errorf("ai: system message content: %w", err)
				}
			}
		case "sections":
			if err := json.Unmarshal(raw, &out.Sections); err != nil {
				return err
			}
		case "toolsAdded":
			out.ToolsAdded = nil
			if err := json.Unmarshal(raw, &out.ToolsAdded); err != nil {
				return fmt.Errorf("ai: system message toolsAdded: %w", err)
			}
		case "toolsRemoved":
			out.ToolsRemoved = nil
			if err := json.Unmarshal(raw, &out.ToolsRemoved); err != nil {
				return fmt.Errorf("ai: system message toolsRemoved: %w", err)
			}
		case "timestamp":
			if err := json.Unmarshal(raw, &out.Timestamp); err != nil {
				return fmt.Errorf("ai: system message timestamp: %w", err)
			}
		default:
			continue
		}
		if !slices.Contains(order, key) {
			order = append(order, key)
		}
	}
	if _, err := dec.Token(); err != nil {
		return err
	}
	*m = out.withKeyOrder(order)
	return nil
}

// UserMessage is a message authored by the user.
type UserMessage struct {
	Content   ContentList `json:"content"` // TextContent | ImageContent
	Timestamp int64       `json:"timestamp"`
	// contentWasString records that the source JSON carried content as a plain
	// string (pi: content is string | array and is passed through untouched), so
	// MarshalJSON can re-emit the string form on round-trip.
	contentWasString bool
}

func (UserMessage) MessageRole() Role { return RoleUser }

// MarshalJSON adds the role discriminator. Content that was decoded from the
// string form is re-emitted as a string (pi leaves string content untouched).
func (m UserMessage) MarshalJSON() ([]byte, error) {
	if m.contentWasString && len(m.Content) == 1 {
		if t, ok := m.Content[0].(TextContent); ok {
			return json.Marshal(struct {
				Role      Role   `json:"role"`
				Content   string `json:"content"`
				Timestamp int64  `json:"timestamp"`
			}{Role: RoleUser, Content: t.Text, Timestamp: m.Timestamp})
		}
	}
	type alias UserMessage
	return json.Marshal(struct {
		Role Role `json:"role"`
		alias
	}{Role: RoleUser, alias: alias(m)})
}

// UnmarshalJSON accepts content as either a string or a discriminated array.
// A missing or null content key yields empty content (JSON.parse tolerance),
// not an error.
func (m *UserMessage) UnmarshalJSON(data []byte) error {
	var probe struct {
		Content   json.RawMessage `json:"content"`
		Timestamp int64           `json:"timestamp"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}
	m.Timestamp = probe.Timestamp
	m.contentWasString = false
	if len(probe.Content) == 0 || string(probe.Content) == "null" {
		m.Content = nil
		return nil
	}
	if probe.Content[0] == '"' {
		var s string
		if err := json.Unmarshal(probe.Content, &s); err != nil {
			return err
		}
		m.Content = ContentList{TextContent{Text: s}}
		m.contentWasString = true
		return nil
	}
	return json.Unmarshal(probe.Content, &m.Content)
}

// StringContent reports whether the message's content was the plain-string
// form (pi: content is string | array), returning that string. Providers use
// this to mirror pi's string-vs-parts request shapes.
func (m UserMessage) StringContent() (string, bool) {
	if m.contentWasString && len(m.Content) == 1 {
		if t, ok := m.Content[0].(TextContent); ok {
			return t.Text, true
		}
	}
	return "", false
}

// NewUserText builds a user message from plain text. The content is marked as
// string-form, matching pi where prompt-created user messages carry `content`
// as a plain string (on the wire and in session files).
func NewUserText(text string, timestamp int64) UserMessage {
	return UserMessage{Content: ContentList{TextContent{Text: text}}, Timestamp: timestamp, contentWasString: true}
}

// AssistantMessage is a message authored by the model.
//
// It is written with its keys in the order pi's JSON.stringify writes them,
// which is not this struct's field order (see MarshalJSON).
type AssistantMessage struct {
	Content  ContentList `json:"content"` // TextContent | ThinkingContent | ToolCall
	Api      Api         `json:"api"`
	Provider ProviderId  `json:"provider"`
	Model    string      `json:"model"`
	// ResponseModel is the concrete model reported by the provider, when it
	// reports one. It may equal the requested Model, which stays the requested
	// id (upstream 14c811841).
	ResponseModel string `json:"responseModel,omitempty"`
	// ResponseID is the provider's own response/message identifier, when its API
	// exposes one.
	ResponseID string `json:"responseId,omitempty"`
	// ProviderThinkingLevel is the exact provider-native effort level this
	// response was produced under (pi `providerThinkingLevel?: string`, upstream
	// 4e69b0c28). It is what lets a later turn replay the SAME effort for a
	// historical assistant message instead of re-running it at the current one —
	// see the Anthropic managed-effort path, which reads it back off the
	// transcript. Absent (empty) on legacy transcripts and on every provider that
	// does not manage effort, which is exactly pi's `undefined`.
	ProviderThinkingLevel string `json:"providerThinkingLevel,omitempty"`
	// ThinkingLevel is the pi thinking level the agent loop requested for this
	// response (pi `thinkingLevel?: ModelThinkingLevel`, upstream 540e174c7):
	// "off" when it requested none. Empty outside the agent loop and on legacy
	// transcripts, which is pi's `undefined`.
	ThinkingLevel ModelThinkingLevel `json:"thinkingLevel,omitempty"`
	Diagnostics   []Diagnostic       `json:"diagnostics,omitempty"`
	Usage         Usage              `json:"usage"`
	StopReason    StopReason         `json:"stopReason"`
	// Deferred is the handle to redeem when StopReason is StopDeferred.
	Deferred     *DeferredHandle `json:"deferred,omitempty"`
	ErrorMessage string          `json:"errorMessage,omitempty"`
	// RawStopReason preserves the provider's own stop/finish reason verbatim,
	// before it was mapped onto StopReason (pi d7b02636 `rawStopReason?`).
	RawStopReason string `json:"rawStopReason,omitempty"`
	// EndTurn is the provider's own indication of whether the model explicitly
	// ended its turn. Preserved for debugging; it does not affect control flow
	// here any more than it does in pi (pi c3e7bc60a `endTurn?`). A pointer
	// because pi distinguishes an explicit false from an absent field.
	//
	// Nothing in this port writes it: the only provider that does is pi's
	// openai-codex-responses, which is deliberately not ported. It is carried
	// anyway because session files and the server wire are shared with pi
	// proper — a session pi wrote must round-trip through here without losing
	// the field — the same reason ToolResultMessage.Usage is carried.
	EndTurn *bool `json:"endTurn,omitempty"`
	// Timestamp is the Unix time in milliseconds when the request started.
	Timestamp int64 `json:"timestamp"`
	// DurationMs is the milliseconds from Timestamp until the response ended,
	// measured with the monotonic clock (pi `durationMs?`, upstream 36a686ee8).
	// AssistantMessageEventStream sets it on the final message of a response it
	// saw start; nil for legacy messages and for deferred results fetched
	// later.
	DurationMs *int64 `json:"durationMs,omitempty"`

	// errorLiteral marks a message built as one object literal naming its
	// errorMessage (NewErrorMessage, NewAssistantMessageLiteral), whose
	// errorMessage precedes its timestamp.
	errorLiteral bool
	// responseIDLiteral marks a message built as one object literal naming
	// its responseId (NewAssistantMessageLiteral), whose responseId precedes
	// its timestamp.
	responseIDLiteral bool
}

func (AssistantMessage) MessageRole() Role { return RoleAssistant }

// NewErrorMessage returns the assistant message pi builds as one object
// literal for a failure outside a response's stream — lazyStream's setup
// error, pi-messages' createErrorEvent, faux's createErrorMessage, the agent's
// run failure: no content, zero usage, the stop reason, the error message and
// the time, in that order. Unlike a message a stream fails part-way, whose
// errorMessage is assigned after everything the stream learned, its
// errorMessage is written before its timestamp.
func NewErrorMessage(api Api, provider ProviderId, modelID string, stopReason StopReason, errorMessage string) *AssistantMessage {
	return &AssistantMessage{
		Content:      ContentList{},
		Api:          api,
		Provider:     provider,
		Model:        modelID,
		StopReason:   stopReason,
		ErrorMessage: errorMessage,
		Timestamp:    nowMillis(),
		errorLiteral: true,
	}
}

// NewAssistantMessageLiteral returns a copy of m that serializes as one
// object literal naming its ErrorMessage and ResponseID, when set, ahead of
// its timestamp — the order pi's fauxAssistantMessage writes its options in.
// A message whose fields are filled in as a response streams writes them
// after its timestamp instead (see MarshalJSON).
func NewAssistantMessageLiteral(m AssistantMessage) *AssistantMessage {
	m.errorLiteral = m.ErrorMessage != ""
	m.responseIDLiteral = m.ResponseID != ""
	return &m
}

// MarshalJSON writes the message with the role discriminator and its keys in
// the order pi's JSON.stringify writes them: the order pi's adapters build the
// object in — the literal {role, content, api, provider, model, usage,
// stopReason, timestamp}, then what the stream learned as it learned it
// (providerThinkingLevel from the built params, responseId and responseModel
// at the start, rawStopReason at the end, diagnostics, errorMessage when it
// fails), durationMs when the stream ends, and thinkingLevel, which the agent
// loop assigns to the stream's result (agent-loop.ts Object.assign). Some
// literals differ: the Responses adapter's carries providerThinkingLevel right
// after model, the error literal of NewErrorMessage carries errorMessage
// before timestamp, and faux's (NewAssistantMessageLiteral) carries
// errorMessage and responseId there. A deferred handle follows stopReason, as
// faux's literal has it. Absent fields are left out, as pi's undefined is.
func (m AssistantMessage) MarshalJSON() ([]byte, error) {
	o := make(OrderedObject, 0, 20)
	add := func(key string, value any, present bool) {
		if present {
			o = append(o, OrderedField{Key: key, Value: value})
		}
	}
	add("role", RoleAssistant, true)
	add("content", m.Content, true)
	add("api", m.Api, true)
	add("provider", m.Provider, true)
	add("model", m.Model, true)
	levelFirst := m.Api == APIOpenAIResponses
	add("providerThinkingLevel", m.ProviderThinkingLevel, levelFirst && m.ProviderThinkingLevel != "")
	add("usage", m.Usage, true)
	add("stopReason", m.StopReason, true)
	add("deferred", m.Deferred, m.Deferred != nil)
	add("errorMessage", m.ErrorMessage, m.errorLiteral && m.ErrorMessage != "")
	add("responseId", m.ResponseID, m.responseIDLiteral && m.ResponseID != "")
	add("timestamp", m.Timestamp, true)
	add("providerThinkingLevel", m.ProviderThinkingLevel, !levelFirst && m.ProviderThinkingLevel != "")
	add("responseId", m.ResponseID, !m.responseIDLiteral && m.ResponseID != "")
	add("responseModel", m.ResponseModel, m.ResponseModel != "")
	add("rawStopReason", m.RawStopReason, m.RawStopReason != "")
	add("endTurn", m.EndTurn, m.EndTurn != nil)
	add("diagnostics", m.Diagnostics, len(m.Diagnostics) > 0)
	add("errorMessage", m.ErrorMessage, !m.errorLiteral && m.ErrorMessage != "")
	add("durationMs", m.DurationMs, m.DurationMs != nil)
	add("thinkingLevel", m.ThinkingLevel, m.ThinkingLevel != "")
	return json.Marshal(o)
}

// ToolResultMessage is the result of executing a tool call.
type ToolResultMessage struct {
	ToolCallID string      `json:"toolCallId"`
	ToolName   string      `json:"toolName"`
	Content    ContentList `json:"content"` // TextContent | ImageContent
	Details    any         `json:"details,omitempty"`
	// Usage is what executing the tool cost, when the tool accounted for that
	// (pi: ToolResultMessage.usage, optional). The agent loop sets it from the
	// tool's AgentToolResult.Usage, as pi's has since 2fd386840, and the server
	// bridge puts it on the wire when it is there.
	Usage   *Usage `json:"usage,omitempty"`
	IsError bool   `json:"isError"`
	// DurationMs is the milliseconds the tool's execution took, measured with
	// the monotonic clock (pi `durationMs?`, upstream 36a686ee8); nil for
	// legacy results and for calls that never ran. pi's agent loop writes it
	// between isError and timestamp.
	DurationMs *int64 `json:"durationMs,omitempty"`
	// Timestamp is the Unix time in milliseconds when the result was created.
	Timestamp int64 `json:"timestamp"`
	// NestedCalls records the calls this tool made to other tools, a codemode
	// script's for example (pi ToolResultMessage.nestedCalls, upstream
	// 8562bcf66). It is kept for the session record and not sent to the model.
	// pi attaches it at message_start, after the message was built, so it is
	// the last key.
	NestedCalls *NestedToolCalls `json:"nestedCalls,omitempty"`
}

// NestedToolCalls is the bounded record of the calls a tool made to other
// tools (upstream 8562bcf66). Results are not recorded.
type NestedToolCalls struct {
	Calls []NestedToolCallRecord `json:"calls"`
	// Complete is false when calls were dropped, arguments omitted, or calls
	// had not finished.
	Complete bool `json:"complete"`
}

// NestedToolCallStatus is how a nested tool call ended.
type NestedToolCallStatus string

const (
	NestedToolCallOK    NestedToolCallStatus = "ok"
	NestedToolCallError NestedToolCallStatus = "error"
	// NestedToolCallUnfinished marks a call still running when the calling
	// tool finished.
	NestedToolCallUnfinished NestedToolCallStatus = "unfinished"
)

// NestedToolCallRecord is one call a tool made to another tool (upstream
// 8562bcf66). Its fields are in the order pi's recorder writes them.
type NestedToolCallRecord struct {
	ID     string               `json:"id"`
	Name   string               `json:"name"`
	Status NestedToolCallStatus `json:"status"`
	// Arguments are the call's arguments as JSON, exactly as recorded. pi
	// records JSON.parse(JSON.stringify(args)) before validating them, so they
	// need not be an object — a script's tools.bash("ls") records "ls" — and
	// raw bytes keep every shape, a null, and pi's key order through a session
	// round trip. Nil (omitted) when over the size limits; ArgumentsBytes then
	// gives their UTF-8 size as JSON.
	Arguments      json.RawMessage `json:"arguments,omitzero"`
	ArgumentsBytes int             `json:"argumentsBytes,omitempty"`
	// DurationMs is unset while the call is unfinished; a call can take 0ms.
	DurationMs *int64 `json:"durationMs,omitempty"`
	// Error is the call's error text, truncated.
	Error string `json:"error,omitempty"`
}

func (ToolResultMessage) MessageRole() Role { return RoleToolResult }

// MarshalJSON adds the role discriminator.
func (m ToolResultMessage) MarshalJSON() ([]byte, error) {
	type alias ToolResultMessage
	return json.Marshal(struct {
		Role Role `json:"role"`
		alias
	}{Role: RoleToolResult, alias: alias(m)})
}

// UnmarshalMessage decodes a Message from JSON based on its "role".
func UnmarshalMessage(data []byte) (Message, error) {
	var head struct {
		Role Role `json:"role"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, err
	}
	switch head.Role {
	case RoleSystem:
		var m SystemMessage
		err := json.Unmarshal(data, &m)
		return m, err
	case RoleUser:
		var m UserMessage
		err := json.Unmarshal(data, &m)
		return m, err
	case RoleAssistant:
		var m AssistantMessage
		err := json.Unmarshal(data, &m)
		return m, err
	case RoleToolResult:
		var m ToolResultMessage
		err := json.Unmarshal(data, &m)
		return m, err
	default:
		return nil, fmt.Errorf("unknown message role: %q", head.Role)
	}
}

// ---------------------------------------------------------------------------
// Tools and context
// ---------------------------------------------------------------------------

// ConstrainedSamplingType is the discriminant of ConstrainedSamplingConfig.
type ConstrainedSamplingType string

// Constrained-sampling kinds (ConstrainedSamplingConfig.Type).
const (
	// ConstrainedSamplingJSONSchema asks the provider to constrain sampling to
	// the tool's JSON schema — the concept most APIs expose as `strict`.
	ConstrainedSamplingJSONSchema ConstrainedSamplingType = "json_schema"
	// ConstrainedSamplingGrammar asks the provider to constrain sampling to a
	// grammar supplied in one of the provider-specific Variants.
	ConstrainedSamplingGrammar ConstrainedSamplingType = "grammar"
)

// ConstrainedSamplingStrictness is how hard a json_schema config insists.
type ConstrainedSamplingStrictness string

// Constrained-sampling strictness levels (ConstrainedSamplingConfig.Strict).
const (
	// ConstrainedSamplingPrefer uses strict sampling where available and falls
	// back to unconstrained sampling elsewhere.
	ConstrainedSamplingPrefer ConstrainedSamplingStrictness = "prefer"
	// ConstrainedSamplingRequire fails the request when the model cannot
	// constrain sampling to the schema.
	ConstrainedSamplingRequire ConstrainedSamplingStrictness = "require"
)

// GrammarVariants holds provider-specific encodings of the same intended
// grammar. An empty field means the caller supplied no such variant.
type GrammarVariants struct {
	OpenAILark  string `json:"openai_lark,omitempty"`
	OpenAIRegex string `json:"openai_regex,omitempty"`
}

// ConstrainedSamplingConfig is an optional provider-side constrained-sampling
// config for a tool.
//
// Type ConstrainedSamplingJSONSchema roughly maps to the concept of `strict` in
// APIs that implement it as JSON-schema constrained sampling, and reads Strict.
// Type ConstrainedSamplingGrammar reads Variants. The zero value marshals to
// (and unmarshals from) JSON `false`, pi's "explicitly unconstrained" spelling,
// and is treated exactly like no config at all.
type ConstrainedSamplingConfig struct {
	Type     ConstrainedSamplingType       `json:"type"`
	Strict   ConstrainedSamplingStrictness `json:"strict"`
	Variants GrammarVariants               `json:"variants"`
}

// MarshalJSON emits only the fields belonging to the configured Type, matching
// pi's discriminated union (and `false` for the disabled zero value). An
// unrecognized Type is an error rather than a silent downgrade to `false`,
// which would quietly drop the caller's constrained-sampling request.
func (c ConstrainedSamplingConfig) MarshalJSON() ([]byte, error) {
	switch c.Type {
	case ConstrainedSamplingJSONSchema:
		strict := c.Strict
		if strict == "" {
			// pi's union admits only "prefer"|"require"; "" is not a value a
			// pi consumer would accept, and prefer is this field's default.
			strict = ConstrainedSamplingPrefer
		}
		return json.Marshal(struct {
			Type   ConstrainedSamplingType       `json:"type"`
			Strict ConstrainedSamplingStrictness `json:"strict"`
		}{c.Type, strict})
	case ConstrainedSamplingGrammar:
		return json.Marshal(struct {
			Type     ConstrainedSamplingType `json:"type"`
			Variants GrammarVariants         `json:"variants"`
		}{c.Type, c.Variants})
	case "":
		return []byte("false"), nil
	default:
		return nil, fmt.Errorf("ai: unknown constrained sampling type %q, want %q or %q",
			c.Type, ConstrainedSamplingJSONSchema, ConstrainedSamplingGrammar)
	}
}

// UnmarshalJSON accepts pi's `false` spelling for "no constrained sampling".
func (c *ConstrainedSamplingConfig) UnmarshalJSON(data []byte) error {
	if s := string(bytes.TrimSpace(data)); s == "false" || s == "null" {
		*c = ConstrainedSamplingConfig{}
		return nil
	}
	// Reject a non-object outright: wrapping json's error here would surface
	// the private shim type ("cannot unmarshal bool into ... ai.alias"), which
	// tells a caller nothing about the spellings this field accepts.
	if s := bytes.TrimSpace(data); len(s) == 0 || s[0] != '{' {
		return fmt.Errorf("ai: constrainedSampling: want false or an object with type %q or %q",
			ConstrainedSamplingJSONSchema, ConstrainedSamplingGrammar)
	}
	type alias ConstrainedSamplingConfig
	if err := json.Unmarshal(data, (*alias)(c)); err != nil {
		return fmt.Errorf("ai: constrainedSampling: %w", err)
	}
	switch c.Type {
	case ConstrainedSamplingJSONSchema, ConstrainedSamplingGrammar:
		return nil
	default:
		return fmt.Errorf("ai: unknown constrained sampling type %q, want %q or %q",
			c.Type, ConstrainedSamplingJSONSchema, ConstrainedSamplingGrammar)
	}
}

// Tool is a tool definition exposed to the model.
type Tool struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Parameters  *Schema `json:"parameters"`
	// ConstrainedSampling optionally asks the provider to constrain sampling of
	// this tool's input. Nil (and the zero config) leave sampling unconstrained.
	ConstrainedSampling *ConstrainedSamplingConfig `json:"constrainedSampling,omitempty"`
}

// ToolReference names a tool without its definition (pi ToolReference).
type ToolReference struct {
	Name string `json:"name"`
}

// Context is the request input the public stream entry points accept
// (Stream, StreamSimple, Models.Stream, ...). SystemPrompt and Tools are
// shorthand for a leading system message; NormalizeContext folds them into one
// before the request reaches a provider.
type Context struct {
	SystemPrompt string    `json:"systemPrompt,omitempty"`
	Messages     []Message `json:"messages"`
	Tools        []Tool    `json:"tools,omitempty"`
}

// TranscriptContext is the normalized request context passed to providers and
// API implementations: the prompt and tool declarations are carried by the
// transcript's system messages. Only NormalizeContext produces one — pi brands
// the type so a raw Context cannot reach provider code by accident; Go cannot
// brand a struct, so the contract is this comment and the distinct type.
type TranscriptContext struct {
	Messages []Message `json:"messages"`
}

// MarshalJSON writes `{"messages":[...]}`, with a nil transcript as the empty
// array a pi TranscriptContext always carries.
func (c TranscriptContext) MarshalJSON() ([]byte, error) {
	messages := c.Messages
	if messages == nil {
		messages = []Message{}
	}
	return json.Marshal(struct {
		Messages []Message `json:"messages"`
	}{messages})
}

// ---------------------------------------------------------------------------
// Models
// ---------------------------------------------------------------------------

// ModelCost holds per-million-token pricing, plus optional request-wide tiers.
type ModelCost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
	// Tiers are request-wide pricing overrides (pi ModelCost.tiers). The highest
	// matching input threshold applies to the full request.
	Tiers []ModelCostTier `json:"tiers,omitempty"`
}

// ModelCostTier overrides the base per-million-token rates for a request whose
// total input usage exceeds InputTokensAbove (pi ModelCostTier).
type ModelCostTier struct {
	Input            float64 `json:"input"`
	Output           float64 `json:"output"`
	CacheRead        float64 `json:"cacheRead"`
	CacheWrite       float64 `json:"cacheWrite"`
	InputTokensAbove int     `json:"inputTokensAbove"`
}

// ModelImageResizeOptions is the cache-safe resize profile applied to an image
// before it enters conversation history (pi ModelImageResizeOptions). A nil
// field keeps the pipeline's own default, so a profile can narrow one limit
// without restating the rest.
type ModelImageResizeOptions struct {
	MaxWidth  *int `json:"maxWidth,omitempty"`
	MaxHeight *int `json:"maxHeight,omitempty"`
	// MaxBytes is the maximum base64-encoded payload size in bytes.
	MaxBytes    *int `json:"maxBytes,omitempty"`
	JPEGQuality *int `json:"jpegQuality,omitempty"`
}

// ModelImageInputLimits describes what a provider accepts for image input (pi
// ModelImageInputLimits).
type ModelImageInputLimits struct {
	// Resize is the profile applied before a new image enters conversation
	// history. It is cache-safe: an image already in the transcript is never
	// re-processed, because that would invalidate the prompt cache.
	Resize *ModelImageResizeOptions `json:"resize,omitempty"`
	// MaxPerMessage is the most images accepted in one provider message.
	MaxPerMessage *int `json:"maxPerMessage,omitempty"`
	// MaxPerRequest is the most images accepted across one provider request.
	MaxPerRequest *int `json:"maxPerRequest,omitempty"`
}

// ModelInputLimits carries a provider's input limits and the cache-safe
// preprocessing metadata derived from them (pi ModelInputLimits). MaxPerMessage,
// MaxPerRequest and MaxRequestBytes have no consumer in pi either — they are
// catalog data describing the provider, carried so a host can act on them.
type ModelInputLimits struct {
	// MaxRequestBytes is the maximum serialized provider request size in bytes.
	MaxRequestBytes *int                   `json:"maxRequestBytes,omitempty"`
	Images          *ModelImageInputLimits `json:"images,omitempty"`
}

// Model describes a concrete model in the unified model system. One struct
// stands in for pi's AnyModel: Type says which operation the model serves.
type Model struct {
	// Type is the model's type (pi `type`, upstream a328aa89a). Empty and
	// "chat" both mean a chat model — GetModelType reads them alike, as pi's
	// `model.type ?? "chat"` does for an absent or null type. One input tells
	// them apart in pi and not here: an explicit JSON "type": "" is an unknown
	// type there (not a chat model, and dropped from stored and fetched
	// catalogs) but reads as chat here, because a string field cannot tell ""
	// from absent. pi's own catalog generator and model-data validation never
	// emit it.
	Type             ModelType        `json:"type,omitempty"`
	ID               string           `json:"id"`
	Name             string           `json:"name"`
	Api              Api              `json:"api"`
	Provider         ProviderId       `json:"provider"`
	BaseURL          string           `json:"baseUrl"`
	Reasoning        bool             `json:"reasoning"`
	ThinkingLevelMap ThinkingLevelMap `json:"thinkingLevelMap,omitempty"`
	Input            []string         `json:"input"` // "text" | "image"
	// InputLimits carries the provider's input limits and the cache-safe image
	// preprocessing profile.
	InputLimits *ModelInputLimits `json:"inputLimits,omitempty"`
	Cost        ModelCost         `json:"cost"`
	// PromptCache carries the prompt cache lifetimes per retention tier. Unset
	// when the provider's cache behavior is unknown.
	PromptCache   *ModelPromptCache `json:"promptCache,omitempty"`
	ContextWindow int               `json:"contextWindow"`
	MaxTokens     int               `json:"maxTokens"`
	// SamplingParams are this model's default sampling parameters. See
	// StreamOptions.SamplingParams; per-request keys override these.
	SamplingParams map[string]any `json:"samplingParams,omitempty"`
	// SamplingParamsByThinkingLevel overrides SamplingParams for the
	// effective thinking level (ResolveSamplingParams).
	SamplingParamsByThinkingLevel map[ModelThinkingLevel]map[string]any `json:"samplingParamsByThinkingLevel,omitempty"`
	Headers                       ProviderHeaders                       `json:"headers,omitempty"`
	// Compat carries API-specific compatibility overrides (decoded per-api).
	Compat json.RawMessage `json:"compat,omitempty"`
}

// ---------------------------------------------------------------------------
// Stream options
// ---------------------------------------------------------------------------

// ProviderResponse is the HTTP response summary passed to OnResponse.
type ProviderResponse struct {
	Status  int
	Headers map[string]string
}

// HTTPDoer performs a provider HTTP request. It is the Go stand-in for pi's
// injectable `fetch`: *http.Client satisfies it, so callers usually pass one.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// ProviderRequestOptions are the authentication, HTTP transport and lifecycle
// options every provider request carries, whatever kind of request it is (pi
// ProviderRequestOptions). Streaming adds sampling and transport preferences on
// top of these in StreamOptions; a deferred fetch or cancel carries these
// alone, because there is no request body left to shape.
//
// pi's `signal` has no counterpart here: cancellation travels on the
// context.Context each entry point takes.
//
// pi parameterises the model type its callbacks see (ProviderRequestOptions
// <TModel>) so image requests can hand back an ImageModel. One Model struct
// serves every model type here, so the callbacks name *Model directly.
type ProviderRequestOptions struct {
	// Telemetry is the explicit parent span for telemetry produced by this
	// logical request (pi telemetry, renamed from telemetryContext by
	// upstream 2ec55d808). Models.Stream, StreamSimple and StreamDeferred
	// report the request under it, with its metric capability (telemetry.go);
	// nil reports to telemetry.NoopContext. Providers receive it unchanged.
	Telemetry telemetry.Span
	APIKey    string
	// OnPayload inspects or replaces a provider payload before it is sent.
	// Returning a nil payload keeps it unchanged.
	OnPayload func(payload any, model *Model) (any, error)
	// OnResponse is invoked after an HTTP response is received.
	OnResponse func(resp ProviderResponse, model *Model) error
	// Headers are custom HTTP headers merged into the provider request, with
	// caller values overriding provider defaults. A nil value suppresses a
	// provider/API default header of the same name (see ProviderHeaders).
	Headers ProviderHeaders
	// TimeoutMs bounds the wait for a response's headers on anthropic-messages,
	// openai-completions and openai-responses, as pi's timeoutMs does through
	// their SDKs' timeout option; zero means the SDKs' default, 10 minutes.
	// google-generative-ai and pi-messages ignore it, as pi's do: neither gives
	// its request a timeout. Every adapter's own client also gives up at
	// undici's headersTimeout, 300 seconds, as pi's fetch does whatever
	// timeoutMs says, so a zero TimeoutMs, or one above 300000, waits 300
	// seconds, and the openai loops then fail with openai 7.19.0's text for
	// undici's timeout rather than "Request timed out.". Only a custom
	// HTTPClient waits longer, and TimeoutMs does not reach one (see
	// HTTPClient); google-generative-ai, which refuses one, cannot.
	TimeoutMs int
	// MaxRetries caps client-side retry attempts for providers that support them.
	MaxRetries int
	// MaxRetryDelayMs caps the delay honored when a server asks for a long
	// wait; a longer requested delay fails the request instead. Nil takes the
	// 60s default, and a zero value disables the cap.
	MaxRetryDelayMs *int
	// HTTPClient overrides the client used for provider HTTP requests (pi
	// StreamOptions.fetch). Nil keeps each provider's default client, and so
	// does http.DefaultClient: it is the Go stand-in for the globalThis.fetch
	// that pi treats as equivalent to unset. The google-generative-ai adapter
	// rejects any other value, mirroring pi's `options.fetch !== globalThis.fetch`
	// guard, because @google/genai cannot take a custom fetch.
	//
	// An override owns its own transport, so neither bound the default client
	// puts on the wait for headers reaches it: undici's 300-second
	// headersTimeout, the default fetch's own, and TimeoutMs, which pi keeps
	// under a custom fetch because its SDKs apply it outside fetch — a
	// deliberate divergence (docs/UPSTREAM.md D13) whose cap is the caller's
	// to reproduce. It does not affect WebSocket transports.
	//
	// The openai adapters read an override's streamed body as openai 7.19.0
	// does, racing each read against the request's context: Read is called on
	// a goroutine of its own, and once the context is done a pending Read gets
	// 100ms (docs/UPSTREAM.md D88). An error it returns by then, other than
	// context.Canceled, fails the stream with that error; anything else, or
	// nothing, ends the stream as an abort. The adapter then closes the body,
	// possibly while that Read is still running, so Close must be safe to call
	// during a Read, and a Read that ignores both the context and Close holds
	// its goroutine for good. An *http.Client's body meets all of this: its
	// Read returns as soon as the request's context ends.
	HTTPClient HTTPDoer
	// Env holds provider-scoped environment overrides. When set, a non-empty
	// value here takes precedence over os.Getenv for provider configuration such
	// as PI_CACHE_RETENTION (pi 7f29e7a3). Defaults to nil, in which case
	// lookups fall through to the OS environment. A Cloudflare model's base-URL
	// placeholders are filled from Env alone (pi cloudflareStreams): through
	// Models it carries the account and gateway ids Cloudflare auth resolved.
	Env map[string]string
}

// StreamOptions are the options shared by all streaming provider requests: the
// request options every provider request carries, plus the fields that shape
// the request body and its transport.
type StreamOptions struct {
	ProviderRequestOptions
	// OnProviderStreamEvent observes each parsed provider stream event before
	// pi normalizes it (pi StreamOptions.onProviderStreamEvent, upstream
	// 002fc8385). data is the parsed value the adapter works from: for a
	// JSON-wire adapter, DecodeOrderedValue of the event's data, so an object
	// arrives as an OrderedObject in the order JSON.parse's object lists its
	// keys, and a number past float64's range as ±Inf (OrderedObject writes
	// it null, as JSON.stringify does); an adapter that pi runs through a
	// vendor SDK passes that SDK's shape. It is adapter-owned and must be
	// treated as read-only.
	// Adapter support is explicit: adapters that do not support it never call
	// it. Calls are synchronous and in stream order, and a non-nil error fails
	// the stream with err.Error() as its error message.
	OnProviderStreamEvent func(data any, model *Model) error
	Temperature           *float64
	// SamplingParams are arbitrary sampling parameters merged into the request
	// body as-is, after the named request fields, so keys here override them.
	// They let custom OpenAI-compatible servers (llama.cpp, vLLM, SGLang, …)
	// receive parameters pi does not model, e.g. top_p, top_k, min_p,
	// repetition_penalty. The adapter merges them over Model.SamplingParams per
	// key. Only the OpenAI-compatible adapters (completions, responses) apply
	// them; other APIs ignore them.
	SamplingParams            map[string]any
	MaxTokens                 *int
	Transport                 Transport
	CacheRetention            CacheRetention
	SessionID                 string
	WebSocketConnectTimeoutMs int
	Metadata                  map[string]any
}

// SimpleStreamOptions extends StreamOptions with unified reasoning controls.
type SimpleStreamOptions struct {
	StreamOptions
	Reasoning ThinkingLevel
	// ToolChoice selects whether the model may call tools. Empty is pi's absent
	// option; when omitted, adapters use provider-specific behavior (upstream
	// 6b36eb592).
	ToolChoice ToolChoice
	// Deferred asks a capable provider to return a DeferredHandle and continue
	// the request asynchronously; nil is pi's absent `deferred`. Providers that
	// do not support deferral ignore it.
	Deferred        *DeferredRequest
	ThinkingBudgets *ThinkingBudgets
}
