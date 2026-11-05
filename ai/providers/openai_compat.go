package providers

import (
	"encoding/json"
	"strings"

	"github.com/sky-valley/pi/ai"
)

// Session-affinity header formats (pi SessionAffinityFormat). "openai" sends
// session_id + x-client-request-id (+ x-session-affinity) on completions, and
// session-id + x-client-request-id on Responses (upstream c5d397835);
// "openai-nosession" drops session_id / session-id; "openrouter" sends
// x-session-id.
const (
	sessionAffinityOpenAI     = "openai"
	sessionAffinityOpenRouter = "openrouter"
)

// sessionAffinityFormatFor is pi's `isOpenRouter ? "openrouter" : "openai"`
// default, shared by the completions and responses providers. It is NOT the
// anthropic-messages default: there the alternative to "openrouter" is pi's
// undefined, and "openai" would give the legacy name a meaning it lacks.
func sessionAffinityFormatFor(isOpenRouter bool) string {
	if isOpenRouter {
		return sessionAffinityOpenRouter
	}
	return sessionAffinityOpenAI
}

// vercelGatewayRouting mirrors pi's vercelGatewayRouting object shape.
type vercelGatewayRouting struct {
	Only  []string `json:"only,omitempty"`
	Order []string `json:"order,omitempty"`
}

// openAICompletionsCompat is the resolved compatibility profile for an
// OpenAI-compatible chat-completions provider (port of ResolvedOpenAICompletionsCompat).
type openAICompletionsCompat struct {
	SupportsStore            bool
	SupportsDeveloperRole    bool
	SupportsReasoningEffort  bool
	SupportsUsageInStreaming bool
	// SupportsFinishReason reports whether streamed responses include
	// finish_reason. When false, pi infers stop/toolUse at end of stream instead
	// of failing with "Stream ended without finish_reason" (upstream 2c3041242).
	// Default: true.
	SupportsFinishReason bool
	MaxTokensField       string // "max_tokens" | "max_completion_tokens"
	ThinkingFormat       string
	SupportsStrictMode   bool
	// SupportsOpenAIGrammarTools reports whether the provider accepts OpenAI
	// custom tools with Lark/regex grammar formats. When false, grammar-
	// constrained tools fall back to normal function tools. Default: false.
	SupportsOpenAIGrammarTools bool
	// SupportsMidConvoSystemMessages reports whether the exact model accepts
	// system or developer messages after the conversation has started. When
	// false, later system messages are folded into the leading system message.
	// Default: false; the generated model catalog enables it for verified
	// models (pi 9e05370b2).
	SupportsMidConvoSystemMessages bool
	// SupportsMidConvoToolAdditions reports whether system messages can
	// introduce additional tools mid-conversation, as Kimi's tool-bearing
	// system messages. Requires SupportsMidConvoSystemMessages. Default: false;
	// the generated model catalog enables it for capable models (pi 9e05370b2).
	SupportsMidConvoToolAdditions               bool
	SupportsLongCacheRetention                  bool
	RequiresReasoningContentOnAssistantMessages bool
	RequiresToolResultName                      bool
	RequiresAssistantAfterToolResult            bool
	// RequiresNonNullAssistantContent reports whether replayed assistant
	// messages with no text (tool-call-only turns) must send "" instead of
	// null content (pi compat.requiresNonNullAssistantContent, upstream
	// 14b804d80). Default: false; detectCompat never sets it.
	RequiresNonNullAssistantContent bool
	RequiresThinkingAsText          bool
	ZaiToolStream                   bool
	// ThinkingTokenBudgetField is the top-level request field used to cap
	// reasoning tokens from the caller's ThinkingBudgets (pi types.ts:596-606,
	// upstream b23741269). Reasoning and the answer share max_tokens on these
	// endpoints, so without a budget a reasoning-heavy turn can consume the
	// whole response and emit no answer. "thinking_token_budget" is vLLM,
	// "thinking_budget" is Qwen/DashScope/SGLang, "thinking_budget_tokens" is
	// llama.cpp. "" is pi's undefined (off); the generated catalog never sets it.
	ThinkingTokenBudgetField string
	// SupportsThinkingTokenBudget is pi's retained alias for
	// ThinkingTokenBudgetField: "thinking_token_budget" (vLLM). Prefer the field
	// name (pi types.ts:607-608, upstream b23741269). Default: false.
	SupportsThinkingTokenBudget bool
	// SendSessionAffinityHeaders reports whether to send session-affinity data
	// from StreamOptions.SessionID. Default: true for OpenRouter endpoints,
	// false otherwise (pi bbb61e34a).
	SendSessionAffinityHeaders bool
	// SessionAffinityFormat selects the session-affinity header shape (pi
	// SessionAffinityFormat). Auto-detected: openrouter → sessionAffinityOpenRouter,
	// else sessionAffinityOpenAI.
	SessionAffinityFormat string
	CacheControlFormat    string // "" | "anthropic"
	// PromptCacheKeyMode controls the prompt_cache_key request field (pi
	// compat.promptCacheKeyMode, upstream 0f4d5e960). "" (pi's undefined) and
	// "auto" send it to api.openai.com, or with long retention when
	// SupportsLongCacheRetention is set; "enabled" sends it whenever caching
	// is active, for compatible proxies; "disabled" never sends it. pi reads
	// the key bare, without a detected default.
	PromptCacheKeyMode string
	// OpenRouterRouting is an arbitrary provider-routing object sent as `provider`.
	OpenRouterRouting map[string]any
	// HasOpenRouterRouting records that model.compat carried a non-null
	// openRouterRouting (pi sends `provider` for any truthy object, even {}).
	HasOpenRouterRouting bool
	// VercelGatewayRouting carries only/order routing for the Vercel AI Gateway.
	VercelGatewayRouting vercelGatewayRouting
	// ChatTemplateKwargs carries the ordered kwargs sent as `chat_template_kwargs`
	// when ThinkingFormat is "chat-template" (pi: compat.chatTemplateKwargs).
	ChatTemplateKwargs []chatTemplateKwarg
	// ChatTemplateArgs carries the ordered args sent as `chat_template_args`
	// when ThinkingFormat is "baseten" (pi: compat.chatTemplateArgs).
	ChatTemplateArgs []chatTemplateKwarg
	// VLLMPriority is the vLLM scheduler priority sent as the top-level
	// `priority` request field (pi OpenAICompletionsCompat.vllmPriority,
	// upstream 256f63024). Lower values are handled earlier; the server default
	// is 0, and the field only means anything when vLLM runs with
	// `--scheduling-policy priority`. detectCompat never produces one and the
	// generated catalog never sets it, so the field is omitted unless a model's
	// compat asks for it. A pointer because 0 is a meaningful value — the
	// highest priority — not "off".
	VLLMPriority *float64
	// HasVLLMPriority records that model.compat carried a vllmPriority key at
	// all, which for this one key is not the same as carrying a value. pi reads
	// it BARE — `vllmPriority: model.compat.vllmPriority`, the only key in
	// getCompat with no `??` fallback — and gates the wire field on
	// `!== undefined`. An explicit JSON null therefore rides as `"priority":null`
	// where a `??` key would have fallen through to its default.
	HasVLLMPriority bool
}

// The values of compat.promptCacheKeyMode (pi 0f4d5e960).
const (
	promptCacheKeyAuto     = "auto"
	promptCacheKeyEnabled  = "enabled"
	promptCacheKeyDisabled = "disabled"
)

// sendsPromptCacheKey ports the openai-completions gate on prompt_cache_key:
// caching must be active, and the mode either "enabled" or "auto" (pi's
// `promptCacheKeyMode ?? "auto"`) with the native-OpenAI or long-retention
// condition. Any other mode string, "disabled" included, sends none.
func (c openAICompletionsCompat) sendsPromptCacheKey(model *ai.Model, retention ai.CacheRetention) bool {
	if retention == ai.CacheNone {
		return false
	}
	switch c.PromptCacheKeyMode {
	case promptCacheKeyEnabled:
		return true
	case "", promptCacheKeyAuto:
		return strings.Contains(model.BaseURL, "api.openai.com") ||
			(retention == ai.CacheLong && c.SupportsLongCacheRetention)
	}
	return false
}

// detectOpenAICompat infers compatibility settings from provider + baseUrl,
// matching pi's detectCompat. Provider takes precedence over URL detection.
func detectOpenAICompat(model *ai.Model) openAICompletionsCompat {
	provider := model.Provider
	baseURL := model.BaseURL
	has := func(s string) bool { return strings.Contains(baseURL, s) }

	isZai := provider == "zai" || provider == "zai-coding-cn" || has("api.z.ai") || has("open.bigmodel.cn")
	isTogether := provider == "together" || has("api.together.ai") || has("api.together.xyz")
	isMoonshot := provider == "moonshotai" || provider == "moonshotai-cn" || has("api.moonshot.")
	isOpenRouter := provider == "openrouter" || has("openrouter.ai")
	isCloudflareWorkersAI := provider == "cloudflare-workers-ai" || has("api.cloudflare.com")
	isCloudflareAiGateway := provider == "cloudflare-ai-gateway" || has("gateway.ai.cloudflare.com")
	isNvidia := provider == "nvidia" || has("integrate.api.nvidia.com")
	isAntLing := provider == "ant-ling" || has("api.ant-ling.com")
	// pi af7359b90 hoisted the cerebras probe so isNonStandard and the strict-mode
	// resolution read from one place.
	isCerebras := provider == "cerebras" || has("cerebras.ai")
	// pi b647d1879: the DeepSeek probe alone folds case, and it is hoisted above
	// isNonStandard so both disjunctions read from it. Every other probe here
	// stays case-sensitive, exactly as upstream leaves them.
	isDeepSeek := provider == "deepseek" || strings.Contains(strings.ToLower(baseURL), "deepseek.com")

	isNonStandard := isNvidia || isCerebras ||
		provider == "xai" || has("api.x.ai") || isTogether || has("chutes.ai") ||
		isDeepSeek || isZai || isMoonshot || provider == "opencode" ||
		has("opencode.ai") || isCloudflareWorkersAI || isCloudflareAiGateway || isAntLing
	useMaxTokens := has("chutes.ai") || isDeepSeek || isMoonshot || isCloudflareAiGateway || isTogether || isNvidia || isAntLing || isZai

	isGrok := provider == "xai" || has("api.x.ai")
	isOpenRouterDeveloperRoleModel := isOpenRouter && (strings.HasPrefix(model.ID, "anthropic/") || strings.HasPrefix(model.ID, "openai/"))
	cacheControlFormat := ""
	if provider == "openrouter" && strings.HasPrefix(model.ID, "anthropic/") {
		cacheControlFormat = "anthropic"
	}

	thinkingFormat := "openai"
	switch {
	case isDeepSeek:
		thinkingFormat = "deepseek"
	case isZai:
		thinkingFormat = "zai"
	case isTogether:
		thinkingFormat = "together"
	case isAntLing:
		thinkingFormat = "ant-ling"
	case isOpenRouter:
		thinkingFormat = "openrouter"
	}

	maxTokensField := "max_completion_tokens"
	if useMaxTokens {
		maxTokensField = "max_tokens"
	}

	// SupportsStrictMode is false for every provider (upstream 890f92088):
	// OpenAI compatibility alone does not imply strict JSON-schema tool support,
	// and the generated catalog enables it explicitly on capable models.
	return openAICompletionsCompat{
		SupportsStore:                               !isNonStandard,
		SupportsDeveloperRole:                       isOpenRouterDeveloperRoleModel || (!isNonStandard && !isOpenRouter),
		SupportsReasoningEffort:                     !isGrok && !isZai && !isMoonshot && !isTogether && !isCloudflareAiGateway && !isNvidia && !isAntLing,
		SupportsUsageInStreaming:                    true,
		SupportsFinishReason:                        true,
		MaxTokensField:                              maxTokensField,
		ThinkingFormat:                              thinkingFormat,
		SupportsStrictMode:                          false,
		SupportsOpenAIGrammarTools:                  false,
		SupportsMidConvoSystemMessages:              false,
		SupportsMidConvoToolAdditions:               false,
		SupportsLongCacheRetention:                  !(isTogether || isCloudflareWorkersAI || isCloudflareAiGateway || isNvidia || isAntLing),
		RequiresReasoningContentOnAssistantMessages: isDeepSeek,
		RequiresToolResultName:                      false,
		RequiresAssistantAfterToolResult:            false,
		RequiresNonNullAssistantContent:             false,
		RequiresThinkingAsText:                      false,
		ZaiToolStream:                               false,
		ThinkingTokenBudgetField:                    "",
		SupportsThinkingTokenBudget:                 false,
		SendSessionAffinityHeaders:                  isOpenRouter,
		SessionAffinityFormat:                       sessionAffinityFormatFor(isOpenRouter),
		CacheControlFormat:                          cacheControlFormat,
		// pi defaults these routing objects to {} (no routing emitted).
		OpenRouterRouting:    nil,
		VercelGatewayRouting: vercelGatewayRouting{},
	}
}

// getOpenAICompat applies explicit model.compat overrides over the detected
// profile, one key at a time — as pi's `model.compat.<key> ?? detected.<key>`
// does. See compatOverrides for why the blob is not decoded in one shot.
func getOpenAICompat(model *ai.Model) openAICompletionsCompat {
	c := detectOpenAICompat(model)
	o := newCompatOverrides(model.Compat)
	applyCompat(o, "supportsStore", &c.SupportsStore)
	applyCompat(o, "supportsDeveloperRole", &c.SupportsDeveloperRole)
	applyCompat(o, "supportsReasoningEffort", &c.SupportsReasoningEffort)
	applyCompat(o, "supportsUsageInStreaming", &c.SupportsUsageInStreaming)
	applyCompat(o, "supportsFinishReason", &c.SupportsFinishReason)
	applyCompat(o, "maxTokensField", &c.MaxTokensField)
	applyCompat(o, "thinkingFormat", &c.ThinkingFormat)
	applyCompat(o, "supportsStrictMode", &c.SupportsStrictMode)
	applyCompat(o, "supportsOpenAIGrammarTools", &c.SupportsOpenAIGrammarTools)
	applyCompat(o, "supportsMidConvoSystemMessages", &c.SupportsMidConvoSystemMessages)
	applyCompat(o, "supportsMidConvoToolAdditions", &c.SupportsMidConvoToolAdditions)
	applyCompat(o, "supportsLongCacheRetention", &c.SupportsLongCacheRetention)
	applyCompat(o, "requiresReasoningContentOnAssistantMessages", &c.RequiresReasoningContentOnAssistantMessages)
	applyCompat(o, "requiresToolResultName", &c.RequiresToolResultName)
	applyCompat(o, "requiresAssistantAfterToolResult", &c.RequiresAssistantAfterToolResult)
	applyCompat(o, "requiresNonNullAssistantContent", &c.RequiresNonNullAssistantContent)
	applyCompat(o, "requiresThinkingAsText", &c.RequiresThinkingAsText)
	applyCompat(o, "zaiToolStream", &c.ZaiToolStream)
	applyCompat(o, "thinkingTokenBudgetField", &c.ThinkingTokenBudgetField)
	applyCompat(o, "supportsThinkingTokenBudget", &c.SupportsThinkingTokenBudget)
	applyCompat(o, "sendSessionAffinityHeaders", &c.SendSessionAffinityHeaders)
	applyCompat(o, "sessionAffinityFormat", &c.SessionAffinityFormat)
	applyCompat(o, "cacheControlFormat", &c.CacheControlFormat)
	applyCompat(o, "promptCacheKeyMode", &c.PromptCacheKeyMode)
	applyCompat(o, "vercelGatewayRouting", &c.VercelGatewayRouting)
	// pi: openRouterRouting falls back to {} (an override always replaces). An
	// explicit {} in model.compat is truthy in JS, so record its presence.
	c.HasOpenRouterRouting = applyCompat(o, "openRouterRouting", &c.OpenRouterRouting)
	// pi: chatTemplateKwargs/chatTemplateArgs overrides always replace the
	// detected defaults ({}); they are ordered maps, decoded by their own parser.
	if raw, ok := o.value("chatTemplateKwargs"); ok {
		c.ChatTemplateKwargs = parseChatTemplateValues(raw)
	}
	if raw, ok := o.value("chatTemplateArgs"); ok {
		c.ChatTemplateArgs = parseChatTemplateValues(raw)
	}
	// vllmPriority is read bare, not through applyCompat: applyCompat implements
	// `??`, which folds an explicit null into "absent". That is right for every
	// other key here and wrong for this one — pi has no `??` on it and sends
	// whatever is not undefined — so read the key itself and keep present-null
	// distinguishable from missing. A value of the wrong type still leaves both
	// fields unset, per D3.
	if raw, present := o["vllmPriority"]; present {
		if isJSONNull(raw) {
			c.HasVLLMPriority = true
		} else if v := new(float64); json.Unmarshal(raw, v) == nil {
			c.VLLMPriority, c.HasVLLMPriority = v, true
		}
	}
	return c
}

// effortValue maps a unified reasoning level to the provider-specific wire value
// via the model's thinkingLevelMap, falling back to the level itself.
func effortValue(model *ai.Model, level string) string {
	if model.ThinkingLevelMap != nil {
		if v, ok := model.ThinkingLevelMap[ai.ModelThinkingLevel(level)]; ok && v != nil {
			return *v
		}
	}
	return level
}

// offEffortValue returns the model's mapped "off" reasoning value, if it is a
// concrete string (some providers need "none"/"minimal" rather than omission).
func offEffortValue(model *ai.Model) (string, bool) {
	if model.ThinkingLevelMap != nil {
		if v, ok := model.ThinkingLevelMap["off"]; ok && v != nil {
			return *v, true
		}
	}
	return "", false
}

// offEffortOrDefault ports pi's `thinkingLevelMap?.off !== null` branch used by
// the openrouter / string-thinking formats. It distinguishes:
//   - off present and null   -> omit reasoning entirely (send=false)
//   - off present and string -> send that string
//   - off absent (undefined) -> send the provided default ("none")
func offEffortOrDefault(model *ai.Model, def string) (value string, send bool) {
	if model.ThinkingLevelMap != nil {
		if v, ok := model.ThinkingLevelMap["off"]; ok {
			if v == nil {
				return "", false // present-null: pi omits reasoning
			}
			return *v, true
		}
	}
	return def, true // absent: pi falls back to default
}
