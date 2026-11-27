package providers

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"

	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/internal/jstext"
)

// openai-completions `reasoning_details` — OpenRouter's structured reasoning
// replay channel (pi openai-completions.ts, upstream b7bb00b93).
//
// A detail is provider-opaque: we validate its shape, never its contents, and
// carry every member — including ones pi has no name for — so the sequence
// replays whole. The whole sequence is serialized into the thinking block's
// thinkingSignature, which is also the session-format slot — see
// ai.ThinkingContent.ThinkingSignature.
//
// "Replayed unmodified" is OpenRouter's requirement, but the bytes pi replays
// are not the provider's bytes: pi parses the detail out of the SSE line and
// the SDK re-serializes the resulting object, so `1.0` becomes `1`, `é`
// becomes `é` and a repeated key collapses. Details therefore go through
// jsStringify (JSON.stringify ∘ JSON.parse) rather than json.Compact, which
// would keep the provider's spelling and diverge from pi on the wire.

// jsonStringValue decodes raw as a JSON string. Unlike json.Unmarshal into a
// string it rejects `null`, which would otherwise decode to "" and make a null
// field indistinguishable from an empty one (pi's `typeof x === "string"`).
func jsonStringValue(raw json.RawMessage) (string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '"' {
		return "", false
	}
	var s string
	if json.Unmarshal(trimmed, &s) != nil {
		return "", false
	}
	return s, true
}

// isJSONString reports whether raw is a JSON string (pi `typeof x === "string"`).
func isJSONString(raw json.RawMessage) bool {
	_, ok := jsonStringValue(raw)
	return ok
}

// isJSONNull reports whether raw is the literal `null`.
func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// isJSONNumber reports whether raw is a JSON number (pi `typeof x === "number"`).
//
// The leading-byte screen is what rejects non-numbers: decoding into
// json.Number keeps the literal instead of converting it, and encoding/json
// accepts a QUOTED number there ("1"), which is a string to `typeof`. Keeping
// the literal is the point — a magnitude json.Parse saturates to ±Infinity is
// still `typeof "number"` in JS, so `1e400` must be accepted, where decoding
// into float64 would fail with "number out of range".
func isJSONNumber(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return false
	}
	if c := trimmed[0]; c != '-' && (c < '0' || c > '9') {
		return false
	}
	var n json.Number
	return json.Unmarshal(trimmed, &n) == nil
}

// reasoningDetailFields decodes a reasoning detail's members.
//
// Port of pi isReasoningDetailObject: only a JSON object qualifies. `null` and
// arrays are rejected explicitly — `typeof null === "object"` and
// `typeof [] === "object"` in JS, and json.Unmarshal of `null` into a map
// succeeds with a nil map, so neither is caught by decoding alone.
func reasoningDetailFields(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(trimmed, &fields) != nil {
		return nil, false
	}
	return fields, true
}

// hasValidCommonReasoningDetailFields checks the members every detail shape may
// carry (pi hasValidCommonReasoningDetailFields): `id` absent, null or string;
// `format` absent or string; `index` absent or number. Note the asymmetry — a
// null `id` is accepted but a null `format` or `index` is not.
func hasValidCommonReasoningDetailFields(fields map[string]json.RawMessage) bool {
	if id, ok := fields["id"]; ok && !isJSONNull(id) && !isJSONString(id) {
		return false
	}
	if format, ok := fields["format"]; ok && !isJSONString(format) {
		return false
	}
	if index, ok := fields["index"]; ok && !isJSONNumber(index) {
		return false
	}
	return true
}

// isOpenAIReasoningDetail reports whether raw is one of the three reasoning
// detail shapes pi accepts (openai-completions.ts isOpenAIReasoningDetail):
//
//	{"type":"reasoning.summary",   "summary": string}
//	{"type":"reasoning.encrypted", "data":    string}
//	{"type":"reasoning.text",      "text":    string, "signature"?: string|null}
//
// plus valid common fields. Any other type — and any non-object — is rejected.
func isOpenAIReasoningDetail(raw json.RawMessage) bool {
	fields, ok := reasoningDetailFields(raw)
	if !ok || !hasValidCommonReasoningDetailFields(fields) {
		return false
	}
	detailType, _ := jsonStringValue(fields["type"])
	switch detailType {
	case "reasoning.summary":
		return isJSONString(fields["summary"])
	case "reasoning.encrypted":
		return isJSONString(fields["data"])
	case "reasoning.text":
		if !isJSONString(fields["text"]) {
			return false
		}
		signature, ok := fields["signature"]
		return !ok || isJSONNull(signature) || isJSONString(signature)
	default:
		return false
	}
}

// isOpenAIReasoningDetailValue is isOpenAIReasoningDetail over a detail as
// JSON.parse made it (ai.DecodeOrderedValue's shapes), the form a streamed
// chunk arrives in: an object (not null, not an array) whose id is absent,
// null or a string, whose format is absent or a string, whose index is absent
// or a number, and whose type names one of the three shapes with its string
// member. A number JSON.parse reads as ±Infinity is still a number here.
func isOpenAIReasoningDetailValue(detail any) bool {
	if _, isObject := detail.(ai.OrderedObject); !isObject {
		return false
	}
	isString := func(v any) bool { _, ok := v.(string); return ok }
	absent := func(v any) bool { return v == jsUndefined }
	if id := jsGet(detail, "id"); !absent(id) && id != nil && !isString(id) {
		return false
	}
	if format := jsGet(detail, "format"); !absent(format) && !isString(format) {
		return false
	}
	if index := jsGet(detail, "index"); !absent(index) {
		if _, isNumber := index.(float64); !isNumber {
			return false
		}
	}
	switch jsGet(detail, "type") {
	case "reasoning.summary":
		return isString(jsGet(detail, "summary"))
	case "reasoning.encrypted":
		return isString(jsGet(detail, "data"))
	case "reasoning.text":
		signature := jsGet(detail, "signature")
		return isString(jsGet(detail, "text")) && (absent(signature) || signature == nil || isString(signature))
	}
	return false
}

// decodeDetailsSignature reads the reasoning-detail sequence serialized in a
// thinking block's signature (pi decodeDetailsSignature). It returns nil
// unless the signature is a JSON array that is non-empty and whose every entry
// is a valid detail — so an ordinary opaque signature (a reasoning field name,
// a Responses item id, a base64 blob) is simply not a sequence.
// decodeThinkingSignature only reaches it for a signature starting with `[`.
func decodeDetailsSignature(signature string) []json.RawMessage {
	var details []json.RawMessage
	// `null` decodes into a slice without error but is not an array to
	// Array.isArray; the length check below covers it along with `[]`.
	if json.Unmarshal([]byte(signature), &details) != nil || len(details) == 0 {
		return nil
	}
	for i, detail := range details {
		if !isOpenAIReasoningDetail(detail) {
			return nil
		}
		details[i] = stringifyReasoningDetail(detail)
	}
	return details
}

// decodeLegacyToolCallReasoningDetail reads the pre-b7bb00b93 session shape: a
// tool call's thoughtSignature holding ONE encrypted detail (pi
// decodeLegacyToolCallReasoningDetail). Only a signature whose first byte is
// `{` is parsed (upstream ede8bdca0), so leading whitespace JSON.parse would
// skip makes it no detail. Beyond being a valid detail it must be
// `reasoning.encrypted` with a non-empty id AND non-empty data, so the loose
// "anything that parses" signatures older code stored are not replayed.
func decodeLegacyToolCallReasoningDetail(signature string) (json.RawMessage, bool) {
	if !strings.HasPrefix(signature, "{") {
		return nil, false
	}
	raw := json.RawMessage(signature)
	if !isOpenAIReasoningDetail(raw) {
		return nil, false
	}
	fields, _ := reasoningDetailFields(raw)
	if detailType, _ := jsonStringValue(fields["type"]); detailType != "reasoning.encrypted" {
		return nil, false
	}
	if id, ok := jsonStringValue(fields["id"]); !ok || id == "" {
		return nil, false
	}
	if data, _ := jsonStringValue(fields["data"]); data == "" {
		return nil, false
	}
	return stringifyReasoningDetail(raw), true
}

// marshalOpenAIReasoningDetails serializes a detail sequence back into a
// thinking signature. Entries are already JSON.stringify-shaped, and
// stringifying an array is its elements joined by commas, so a plain join IS
// pi's JSON.stringify of the array it just parsed and pushed onto.
func marshalOpenAIReasoningDetails(details []json.RawMessage) string {
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, detail := range details {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(detail)
	}
	buf.WriteByte(']')
	return buf.String()
}

// stringifyReasoningDetail renders a detail the way pi's SDK renders the object
// it parsed out of the SSE line: `JSON.stringify(JSON.parse(detail))`. Unknown
// members and their nesting survive whole, which is the property OpenRouter
// needs; what does NOT survive is the provider's own spelling of a value pi
// never had either — redundant number forms, `\uXXXX` escapes of characters
// JSON.stringify emits literally, and a repeated key.
//
// Invalid JSON is returned unchanged; every caller here has already validated
// its input, so the fallback never fires in practice.
func stringifyReasoningDetail(raw json.RawMessage) json.RawMessage {
	rendered, ok := jsStringify(raw)
	if !ok {
		return raw
	}
	return json.RawMessage(rendered)
}

// appendOpenAIReasoningDetail appends detail to a sequence, merging it into the
// previous entry when both are text or both are summary (pi
// appendOpenAIReasoningDetail, upstream c5ad7c1b0). OpenRouter streams
// reasoning_details as DELTAS: consecutive text/summary deltas are one logical
// entry, while encrypted entries stay opaque and discrete. Only ADJACENT
// entries merge, so an encrypted entry between two text deltas breaks the run
// and the next text delta starts a fresh entry.
func appendOpenAIReasoningDetail(details []json.RawMessage, detail json.RawMessage) []json.RawMessage {
	// pi's `details.push({ ...detail })` is a shallow copy, so later merges
	// mutate the STORED entry and never the arriving one; rendering the detail
	// into bytes of its own is that copy.
	normalized := stringifyReasoningDetail(detail)
	if len(details) > 0 {
		if merged, ok := mergeOpenAIReasoningDetail(details[len(details)-1], normalized); ok {
			details[len(details)-1] = merged
			return details
		}
	}
	return append(details, normalized)
}

// mergeOpenAIReasoningDetail folds a text or summary delta into the preceding
// entry of the same type, reporting false when the two do not merge and the
// delta has to be appended on its own.
//
// Both arguments are stringifyReasoningDetail output, so the merge is a
// rendering-level edit of the object pi mutates in place. Since upstream
// 7aab6c26e pi no longer re-parses the sequence out of the signature per
// arriving detail: it holds live JS objects and stringifies once at block end.
// Rendering per arrival is still equivalent for every value that survives a
// JSON round trip, which is every value a provider can send but one — a
// magnitude JSON.parse saturates to Infinity. That renders to `null` here on
// arrival, and the nullish fill below then reads it as absent and drops the
// key, where pi's live Infinity is not nullish, survives the fill, and
// serializes as `null`. So `{"index": 1e400}` followed by an ADJACENT same-type
// delta yields no `index` here and `"index":null` in pi; unmerged, both render
// `"index":null` and agree. Recorded, not fixed — see docs/UPSTREAM.md — since
// reaching it needs a provider to send `index: 1e400` in the first place.
func mergeOpenAIReasoningDetail(last, detail json.RawMessage) (json.RawMessage, bool) {
	target, ok := decodeReasoningDetailMembers(last)
	if !ok {
		return nil, false
	}
	source, ok := decodeReasoningDetailMembers(detail)
	if !ok {
		return nil, false
	}
	detailType := source.stringMember("type")
	if target.stringMember("type") != detailType {
		return nil, false
	}
	switch detailType {
	case "reasoning.text":
		target.concat("text", source)
		// `signature ||= detail.signature` is a FALSY fill, unlike the nullish
		// fills below: an empty signature is overwritten, a null one too.
		if isFalsyJSMember(target.get("signature")) {
			target.assign("signature", source)
		}
	case "reasoning.summary":
		target.concat("summary", source)
	default:
		return nil, false
	}
	// fillMissingCommonReasoningDetailFields, inlined. The assignment order is
	// observable: a key the target lacks is created at the END of the object,
	// so id, format and index land in that order behind what is already there.
	if isNullishJSMember(target.get("id")) {
		target.assign("id", source)
	}
	if isFalsyJSMember(target.get("format")) {
		target.assign("format", source)
	}
	// `index ??=`, NOT `||=`: an index of 0 is falsy but PRESENT, and pi keeps it.
	if isNullishJSMember(target.get("index")) {
		target.assign("index", source)
	}
	return target.render(), true
}

// reasoningDetailMember is one member of a detail object: its key and its
// already-rendered JSON value.
type reasoningDetailMember struct {
	key   string
	value string
}

// reasoningDetailMembers is a detail broken into members in JSON.stringify
// order. A merge needs that order because pi merges by MUTATING the stored
// object: overwriting an existing key keeps it in place, while assigning a key
// the object lacks appends it — so the merged entry's byte order is the order
// of neither input on its own.
type reasoningDetailMembers []reasoningDetailMember

// decodeReasoningDetailMembers breaks a detail rendered by
// stringifyReasoningDetail into its members. The input is already in
// JSON.stringify shape — duplicate keys collapsed, array-index keys hoisted,
// values normalized — so walking it in document order reproduces the parsed
// object's own-property order, and re-rendering each value is the identity.
func decodeReasoningDetailMembers(raw json.RawMessage) (reasoningDetailMembers, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if tok, err := dec.Token(); err != nil || tok != json.Token(json.Delim('{')) {
		return nil, false
	}
	var members reasoningDetailMembers
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return nil, false
		}
		name, ok := key.(string)
		if !ok {
			return nil, false
		}
		var value strings.Builder
		if err := jsStringifyValue(dec, &value); err != nil {
			return nil, false
		}
		members = append(members, reasoningDetailMember{key: name, value: value.String()})
	}
	return members, true
}

// get returns a member's rendered value, and whether the object has the key at
// all — the distinction `??=` and `||=` both start from.
func (m reasoningDetailMembers) get(key string) (string, bool) {
	for _, member := range m {
		if member.key == key {
			return member.value, true
		}
	}
	return "", false
}

// stringMember returns a member known to hold a JSON string, decoded. An absent
// or non-string member reads as "", which no caller here can reach: the
// validator has already pinned `type`, `text` and `summary` as strings.
func (m reasoningDetailMembers) stringMember(key string) string {
	value, _ := m.get(key)
	s, _ := jsonStringValue(json.RawMessage(value))
	return s
}

// set overwrites a member in place, or appends it when the object lacks the key
// — JS property assignment, whose ordering rule this whole type exists to keep.
func (m *reasoningDetailMembers) set(key, value string) {
	for i := range *m {
		if (*m)[i].key == key {
			(*m)[i].value = value
			return
		}
	}
	*m = append(*m, reasoningDetailMember{key: key, value: value})
}

// assign renders `target[key] = source[key]`. Assigning an ABSENT source member
// assigns undefined, and JSON.stringify omits an undefined value, so the key
// leaves the rendering entirely — which is what removing it here reproduces.
func (m *reasoningDetailMembers) assign(key string, source reasoningDetailMembers) {
	if value, ok := source.get(key); ok {
		m.set(key, value)
		return
	}
	for i := range *m {
		if (*m)[i].key == key {
			*m = append((*m)[:i], (*m)[i+1:]...)
			return
		}
	}
}

// concat renders pi's `target[key] += source[key]` on two string members.
func (m *reasoningDetailMembers) concat(key string, source reasoningDetailMembers) {
	m.set(key, jsQuote(m.stringMember(key)+source.stringMember(key)))
}

// render serializes the members back into a detail, JSON.stringify-shaped.
func (m reasoningDetailMembers) render() json.RawMessage {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, member := range m {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString(jsQuote(member.key))
		buf.WriteByte(':')
		buf.WriteString(member.value)
	}
	buf.WriteByte('}')
	return json.RawMessage(buf.String())
}

// isNullishJSMember reports whether a member is nullish in JS — the state `??=`
// fills. That is absent, or the literal null.
func isNullishJSMember(value string, present bool) bool {
	return !present || value == "null"
}

// isFalsyJSMember reports whether a member is falsy in JS — the state `||=`
// fills. Beyond nullish that is false, zero and the EMPTY STRING; the empty
// string is what makes `||=` on `format` and `signature` behave differently
// from `??=` on `id` and `index` in the very same function.
func isFalsyJSMember(value string, present bool) bool {
	if isNullishJSMember(value, present) {
		return true
	}
	switch value {
	case "false", "0", `""`:
		return true
	}
	return false
}

// isOpenAICompletionsReasoningField reports whether a thinking signature names
// a raw reasoning field on the assistant message (pi
// isOpenAICompletionsReasoningField). Signatures outside this set are replay
// data — an OpenAI Responses item id, an anthropic blob, a serialized detail
// sequence — and must never be used as a request key.
func isOpenAICompletionsReasoningField(field string) bool {
	return slices.Contains(openAICompletionsReasoningFields[:], field)
}

// openAICompletionsReasoningFields are the assistant message fields that carry
// plain reasoning text, in the order the stream prefers them (pi
// OPENAI_COMPLETIONS_REASONING_FIELDS).
var openAICompletionsReasoningFields = [...]string{"reasoning_content", "reasoning", "reasoning_text"}

// completionsSignatureKind names the replay data a thinkingSignature holds.
type completionsSignatureKind int

const (
	// signatureField: the signature is a reasoning field name, e.g.
	// reasoning_content; the text is replayed in that field.
	signatureField completionsSignatureKind = iota + 1
	// signatureDetails: a JSON array of OpenRouter reasoning_details, replayed
	// as is.
	signatureDetails
	// signatureOpaque: a JSON object {field?, reasoning_opaque} carrying GitHub
	// Copilot's opaque reasoning state, replayed next to the text (upstream
	// ede8bdca0). No field means the opaque value came without text.
	signatureOpaque
)

// completionsThinkingSignature is the replay data stored in a thinking block's
// thinkingSignature (pi CompletionsThinkingSignature). Sessions keep every
// format ever written, so each must stay decodable.
type completionsThinkingSignature struct {
	kind completionsSignatureKind
	// field is the reasoning field: always set for signatureField, optional
	// ("" = absent) for signatureOpaque.
	field   string
	details []json.RawMessage
	opaque  string
}

// decodeThinkingSignature decodes a stored thinkingSignature (pi
// decodeThinkingSignature); ok is false when it is empty or not one of the
// known formats. It runs for every thinking block of every request, so only a
// signature whose first byte opens one of the JSON forms is parsed.
func decodeThinkingSignature(signature string) (completionsThinkingSignature, bool) {
	if signature == "" {
		return completionsThinkingSignature{}, false
	}
	if isOpenAICompletionsReasoningField(signature) {
		return completionsThinkingSignature{kind: signatureField, field: signature}, true
	}
	switch signature[0] {
	case '[':
		if details := decodeDetailsSignature(signature); details != nil {
			return completionsThinkingSignature{kind: signatureDetails, details: details}, true
		}
	case '{':
		return decodeOpaqueSignature(signature)
	}
	return completionsThinkingSignature{}, false
}

// decodeOpaqueSignature reads `{"field"?:…,"reasoning_opaque":…}` (pi
// decodeOpaqueSignature). The opaque value must be a non-empty string; field
// must be absent or one of the reasoning fields — a `null` field is neither,
// so the whole signature is rejected. The object is read as JSON.parse reads
// it, so a repeated key takes its last value.
func decodeOpaqueSignature(signature string) (completionsThinkingSignature, bool) {
	parsed, err := ai.DecodeOrderedValue([]byte(signature))
	if err != nil {
		return completionsThinkingSignature{}, false
	}
	if _, isObject := parsed.(ai.OrderedObject); !isObject {
		return completionsThinkingSignature{}, false
	}
	opaque, isString := jsGet(parsed, "reasoning_opaque").(string)
	if !isString || opaque == "" {
		return completionsThinkingSignature{}, false
	}
	field := jsGet(parsed, "field")
	if field == jsUndefined {
		return completionsThinkingSignature{kind: signatureOpaque, opaque: opaque}, true
	}
	if name, isString := field.(string); isString && isOpenAICompletionsReasoningField(name) {
		return completionsThinkingSignature{kind: signatureOpaque, field: name, opaque: opaque}, true
	}
	return completionsThinkingSignature{}, false
}

// encode renders the replay data in the format decodeThinkingSignature reads
// (pi encodeThinkingSignature). The opaque form is JSON.stringify of
// `{ field, reasoning_opaque }`: field first, and left out entirely when
// absent, as JSON.stringify drops an undefined member.
func (s completionsThinkingSignature) encode() string {
	switch s.kind {
	case signatureField:
		return s.field
	case signatureDetails:
		return marshalOpenAIReasoningDetails(s.details)
	case signatureOpaque:
		var b strings.Builder
		b.WriteByte('{')
		if s.field != "" {
			b.WriteString(`"field":`)
			b.WriteString(jsQuote(s.field))
			b.WriteByte(',')
		}
		b.WriteString(`"reasoning_opaque":`)
		b.WriteString(jsQuote(s.opaque))
		b.WriteByte('}')
		return b.String()
	}
	return ""
}

// resolveReasoningReplay picks the reasoning replay data of one assistant
// message (pi resolveReasoningReplay): the first reasoning_details sequence a
// thinking block carries (or, for older sessions, the encrypted details on its
// tool calls), else the LATEST Copilot reasoning_opaque, else the reasoning
// field of the first thinking block with text. The zero value (no kind) means
// there is none.
func resolveReasoningReplay(thinkingBlocks []ai.ThinkingContent, toolCalls []ai.ToolCall) completionsThinkingSignature {
	signatures := make([]completionsThinkingSignature, len(thinkingBlocks))
	for i, b := range thinkingBlocks {
		signatures[i], _ = decodeThinkingSignature(b.ThinkingSignature)
	}
	for _, s := range signatures {
		if s.kind == signatureDetails {
			return s
		}
	}
	var legacy []json.RawMessage
	for _, tc := range toolCalls {
		if detail, ok := decodeLegacyToolCallReasoningDetail(tc.ThoughtSignature); ok {
			legacy = append(legacy, detail)
		}
	}
	if len(legacy) > 0 {
		return completionsThinkingSignature{kind: signatureDetails, details: legacy}
	}
	for _, s := range slices.Backward(signatures) {
		if s.kind == signatureOpaque {
			return s
		}
	}
	for i, b := range thinkingBlocks {
		if jstext.Trim(b.Thinking) != "" {
			if signatures[i].kind == signatureField {
				return signatures[i]
			}
			break
		}
	}
	return completionsThinkingSignature{}
}
