package ai

import (
	"encoding/json"
	"fmt"
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
)

// ProviderId identifies a model provider (e.g. "anthropic", "openai"). pi
// renamed this from Provider to ProviderId in the model-registry merge
// (732bb161), freeing Provider for the runtime Provider interface (see
// models_runtime.go).
type ProviderId = string

// ThinkingLevel is a reasoning effort level understood by the unified API.
type ThinkingLevel string

const (
	ThinkingMinimal ThinkingLevel = "minimal"
	ThinkingLow     ThinkingLevel = "low"
	ThinkingMedium  ThinkingLevel = "medium"
	ThinkingHigh    ThinkingLevel = "high"
	ThinkingXHigh   ThinkingLevel = "xhigh"
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
	StopStop    StopReason = "stop"
	StopLength  StopReason = "length"
	StopToolUse StopReason = "toolUse"
	StopError   StopReason = "error"
	StopAborted StopReason = "aborted"
)

// Role identifies a message author.
type Role string

const (
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
	Thinking          string `json:"thinking"`
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
	// ThoughtSignature is a Google-specific opaque signature for reusing thought context.
	ThoughtSignature string `json:"thoughtSignature,omitempty"`
}

func (ToolCall) contentType() string { return "toolCall" }

// marshalContent serializes a content block with its "type" discriminator.
func marshalContent(c Content) ([]byte, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	// Splice the type field into the object.
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	t, _ := json.Marshal(c.contentType())
	obj["type"] = t
	return json.Marshal(obj)
}

// unmarshalContent decodes a content block based on its "type" discriminator.
func unmarshalContent(data []byte) (Content, error) {
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

// CostBreakdown holds the per-bucket dollar cost of a request.
type CostBreakdown struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
	Total      float64 `json:"total"`
}

// Usage holds token counts and cost for a request.
type Usage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cacheRead"`
	CacheWrite int `json:"cacheWrite"`
	// CacheWrite1h is the subset of CacheWrite written with 1h retention. Only
	// Anthropic reports this split (pi: Usage.cacheWrite1h, optional).
	CacheWrite1h int `json:"cacheWrite1h,omitempty"`
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
}

// ---------------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------------

// Message is a UserMessage, AssistantMessage, or ToolResultMessage.
type Message interface {
	MessageRole() Role
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
type AssistantMessage struct {
	Content       ContentList  `json:"content"` // TextContent | ThinkingContent | ToolCall
	Api           Api          `json:"api"`
	Provider      ProviderId   `json:"provider"`
	Model         string       `json:"model"`
	ResponseModel string       `json:"responseModel,omitempty"`
	ResponseID    string       `json:"responseId,omitempty"`
	Diagnostics   []Diagnostic `json:"diagnostics,omitempty"`
	Usage         Usage        `json:"usage"`
	StopReason    StopReason   `json:"stopReason"`
	ErrorMessage  string       `json:"errorMessage,omitempty"`
	Timestamp     int64        `json:"timestamp"`
}

func (AssistantMessage) MessageRole() Role { return RoleAssistant }

// MarshalJSON adds the role discriminator.
func (m AssistantMessage) MarshalJSON() ([]byte, error) {
	type alias AssistantMessage
	return json.Marshal(struct {
		Role Role `json:"role"`
		alias
	}{Role: RoleAssistant, alias: alias(m)})
}

// ToolResultMessage is the result of executing a tool call.
type ToolResultMessage struct {
	ToolCallID string      `json:"toolCallId"`
	ToolName   string      `json:"toolName"`
	Content    ContentList `json:"content"` // TextContent | ImageContent
	Details    any         `json:"details,omitempty"`
	IsError    bool        `json:"isError"`
	Timestamp  int64       `json:"timestamp"`
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

// Tool is a tool definition exposed to the model.
type Tool struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Parameters  *Schema `json:"parameters"`
}

// Context is the input to a stream call: system prompt, transcript, and tools.
type Context struct {
	SystemPrompt string    `json:"systemPrompt,omitempty"`
	Messages     []Message `json:"messages"`
	Tools        []Tool    `json:"tools,omitempty"`
}

// ---------------------------------------------------------------------------
// Models
// ---------------------------------------------------------------------------

// ModelCost holds per-million-token pricing.
type ModelCost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
}

// Model describes a concrete model in the unified model system.
type Model struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	Api              Api               `json:"api"`
	Provider         ProviderId        `json:"provider"`
	BaseURL          string            `json:"baseUrl"`
	Reasoning        bool              `json:"reasoning"`
	ThinkingLevelMap ThinkingLevelMap  `json:"thinkingLevelMap,omitempty"`
	Input            []string          `json:"input"` // "text" | "image"
	Cost             ModelCost         `json:"cost"`
	ContextWindow    int               `json:"contextWindow"`
	MaxTokens        int               `json:"maxTokens"`
	Headers          map[string]string `json:"headers,omitempty"`
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

// StreamOptions are the base options shared by all providers.
type StreamOptions struct {
	Temperature               *float64
	MaxTokens                 *int
	APIKey                    string
	Transport                 Transport
	CacheRetention            CacheRetention
	SessionID                 string
	OnPayload                 func(payload any, model *Model) (any, error)
	OnResponse                func(resp ProviderResponse, model *Model) error
	Headers                   map[string]string
	TimeoutMs                 int
	WebSocketConnectTimeoutMs int
	MaxRetries                int
	MaxRetryDelayMs           *int
	Metadata                  map[string]any
	// Env holds provider-scoped environment overrides. When set, a non-empty
	// value here takes precedence over os.Getenv for provider configuration such
	// as PI_CACHE_RETENTION and Cloudflare base-URL placeholders (pi 7f29e7a3).
	// Defaults to nil, in which case lookups fall through to the OS environment.
	Env map[string]string
}

// SimpleStreamOptions extends StreamOptions with unified reasoning controls.
type SimpleStreamOptions struct {
	StreamOptions
	Reasoning       ThinkingLevel
	ThinkingBudgets *ThinkingBudgets
}
