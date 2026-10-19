package providers

import (
	"fmt"
	"math"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/internal/jstext"
)

// Header plumbing for ai.ProviderHeaders, whose nil values are deletion markers
// (see ai.ProviderHeaders). pi applies those markers two different ways, and
// the difference is observable, so the port keeps both:
//
//   - Adapters that hand the merged headers to a vendor SDK as `defaultHeaders`
//     (openai-completions, openai-responses, anthropic-messages) let the SDK
//     delete on null, which also removes a header the SDK itself would have
//     sent, its auth header included. sdkHeaders is that path: the SDK's own
//     bundles around pi's object, which headerObject.applyAsDefaultHeaders
//     writes.
//   - Adapters that build the request themselves (google-generative-ai,
//     pi-messages) run the merged headers through providerHeadersToRecord
//     (pi utils/headers.ts), which folds names case-insensitively: the last
//     slot for a name wins, and a null there deletes every earlier spelling
//     of it from the record (upstream a328aa89a). The record is then spread
//     over the adapter's own literal headers, so a null still cannot remove a
//     header the adapter set literally. headerObject.applyAsRecord (google)
//     and applyAsFetchInit (pi-messages) are that path.
//
// Both paths end in a fetch Headers object in pi — the SDK's, or the one fetch
// builds from pi-messages' plain object — and Headers.append/set normalize
// every value by stripping its leading and trailing HTTP whitespace. net/http
// does not: it fails the request on a CR or LF anywhere in a value, and its
// HTTP/2 encoder sends a value's edges verbatim. So every value is normalized
// here as it is written (setHeader), which makes a key or token read with a
// trailing newline work as it does in pi instead of failing the stream.
//
// Before it normalizes anything, a Headers object converts each name and value
// it is given to a WebIDL ByteString (see byteString): a character above U+00FF
// fails the request before it is sent, and U+0080..U+00FF go out as one Latin-1
// byte each, where net/http would send the UTF-8 bytes of any string. After it
// normalizes a value, it refuses a name that is not an HTTP token and a value
// holding a NUL, CR or LF, with a TypeError of its own (see checkHeaderToken,
// normalizeHeaderValue), where net/http fails the round trip with its text and
// the retry loop tries it again. openai's own buildHeaders tests each name
// against the token grammar before a Headers ever sees it, with a message of
// its own (openaiHeaderName). Every name and value is converted and checked
// here, in the order pi's Headers sees them, so the same input fails with the
// same message, at once, or is sent as the same bytes.
//
// Two divergences live in here and are recorded in docs/UPSTREAM.md rather than
// papered over: an empty or whitespace-only User-Agent value is dropped
// entirely where pi sends the header present-and-empty (setHeader normalizes a
// whitespace-only value to empty, and net/http omits an empty User-Agent), and
// @google/genai comma-joins a record entry onto its own User-Agent or
// x-goog-api-client default when the entry spells the name differently, where
// this package — which sends neither default — sends the record's value alone.

// headerObject models the ONE header object pi builds per provider request.
//
// It is an ordered map because pi's is one. A JS object keeps its string keys
// in insertion order, and assigning to a key it already holds updates that key
// IN PLACE instead of moving it to the end — so a later source that re-spells a
// name an earlier source already inserted does NOT get promoted past the names
// inserted in between. Two names differing only by case are two distinct keys
// there but one header on the wire, and both the vendor SDKs and net/http
// resolve that collision by taking the LAST of them; which one is last is
// exactly the slot information an http.Header cannot hold.
//
// Folding every source into this before touching the request is what keeps the
// two agreeing. Concretely, on the anthropic OAuth branch pi's object holds
// "User-Agent" (the seeded pi default) at slot 0 and "user-agent"
// (claude-cli/<v>) at a later slot, so a caller spelling the name "User-Agent"
// lands back in slot 0 and loses to the Claude Code identity — while a caller
// spelling it "user-agent" or "USER-AGENT" wins. Writing straight to an
// http.Header made the later WRITE win regardless of spelling, which is not
// what pi does.
type headerObject struct {
	// names in insertion order; every name is a key of values.
	names []string
	// values by exact (non-canonicalized) name; nil is a deletion marker.
	values map[string]*string
}

// set records an adapter-owned literal header, the way pi writes one into the
// object literal it spreads its sources into.
func (o *headerObject) set(name, value string) {
	o.setValue(name, &value)
}

// setValue records name at its existing slot, or adds a slot for a name the
// object does not hold yet where a JS object enumerates it (see insertOwnKey)
// — JS assignment semantics.
func (o *headerObject) setValue(name string, value *string) {
	if o.values == nil {
		o.values = make(map[string]*string, 8)
	}
	if _, held := o.values[name]; !held {
		o.names = insertOwnKey(o.names, name, func(name string) string { return name })
	}
	o.values[name] = value
}

// insertOwnKey adds item to items, an object's own keys in the order a JS
// object enumerates them, where that order puts a new key: an array index (see
// arrayIndex) among the leading array indexes, in ascending numeric order, and
// any other string after every key already there. key reads an item's key.
//
// So a header named "1" converts, and fails, ahead of every header named
// otherwise, whatever slot it was added in, and "9" ahead of "10". Measured on
// pi's wire: model {"X-A": U+65E5} with opts {"1": U+0100} fails on the
// U+0100.
func insertOwnKey[T any](items []T, item T, key func(T) string) []T {
	n, isIndex := arrayIndex(key(item))
	if !isIndex {
		return append(items, item)
	}
	i := 0
	for ; i < len(items); i++ {
		if m, ok := arrayIndex(key(items[i])); !ok || m > n {
			break
		}
	}
	return slices.Insert(items, i, item)
}

// arrayIndex reports whether name is an array index, and its value: the
// canonical decimal form of an integer from 0 to 2^32-2, so "01", "+1" and
// "4294967295" are not.
func arrayIndex(name string) (uint32, bool) {
	if name == "" || len(name) > 10 || (len(name) > 1 && name[0] == '0') {
		return 0, false
	}
	var n uint64
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + uint64(c-'0')
	}
	if n > math.MaxUint32-1 {
		return 0, false
	}
	return uint32(n), true
}

// merge folds one ProviderHeaders source in, the way pi spreads one into the
// object (`{...merged, ...source}`): the source overrides an earlier value by
// name, and a nil overrides just as a string does — that is what lets a
// consumer's marker cancel a model or attribution header.
//
// Across sources the order is the caller's call order, which is pi's spread
// order exactly. WITHIN one source it is sorted name order, because a Go map
// has no key order to reproduce: that is the standing tie-break for this
// divergence class (2026-08-04 ruling), shared with mergeHeaders in
// ai/models_runtime.go, and it now only decides ties between two spellings
// inside a single ai.ProviderHeaders literal. An array-index name takes the
// slot a JS object gives it whatever the order (see insertOwnKey).
func (o *headerObject) merge(source ai.ProviderHeaders) {
	for _, name := range sortedNames(source) {
		o.setValue(name, source[name])
	}
}

// mergeStrings folds in an adapter-owned bundle that carries no markers
// (attribution defaults, copilot dynamic headers). Same ordering rules as
// merge.
func (o *headerObject) mergeStrings(source map[string]string) {
	for _, name := range sortedNames(source) {
		value := source[name]
		o.setValue(name, &value)
	}
}

// applyAsDefaultHeaders writes the object onto h with a marker DELETING the
// header it names. It is the Go stand-in for passing the merged headers to an
// SDK as `defaultHeaders` (see sdkHeaders), where a null value removes the
// header an earlier bundle set — the SDK's own auth header included. Header
// names are canonicalized by net/http, so the delete matches
// case-insensitively, as it does on the wire, and slot order decides a case
// collision the way the SDKs' own buildHeaders does (later slot wins; a marker
// in the last slot deletes).
//
// It fails where the SDK's buildHeaders throws, before anything is sent: slot
// by slot, on the name (checkName, the SDK's own check of it) and then on the
// value.
func (o *headerObject) applyAsDefaultHeaders(h http.Header, checkName headerNameCheck) error {
	return o.fold(h, nil, checkName)
}

// headerNameCheck is how an SDK's buildHeaders refuses the name of an entry of
// a bundle it folds, a marker's included: nil for a name it takes.
type headerNameCheck func(name string) error

// undiciDeleteHeaderName is @anthropic-ai/sdk 0.129.0's name check (and openai
// 6's): buildHeaders deletes a name from its Headers before it first appends
// one, and a marker is that delete alone, so undici's Headers.delete converts
// the name and refuses one that is not a token.
func undiciDeleteHeaderName(name string) error { return checkHeaderName("Headers.delete", name) }

// openaiHeaderName is openai 7.19.0's name check: its buildHeaders tests every
// name against the token grammar itself, before anything converts it, so a
// name with a character above U+00FF fails with this text too, not with the
// ByteString conversion's.
func openaiHeaderName(name string) error {
	if !isHTTPToken(name) {
		return fmt.Errorf("Header name must be a valid HTTP token [\"%s\"]", name)
	}
	return nil
}

// fold is buildHeaders' loop over one plain-object bundle (see
// applyAsDefaultHeaders), each name checked by checkName. When nulls is not
// nil it also keeps the Headers' `nulls`: the names a marker left deleted, by
// canonical key, which a later fold over the result deletes again (see
// builtHeaders).
func (o *headerObject) fold(h http.Header, nulls map[string]bool, checkName headerNameCheck) error {
	for _, name := range o.names {
		if err := checkName(name); err != nil {
			return err
		}
		key := http.CanonicalHeaderKey(name)
		value := o.values[name]
		if value == nil {
			h.Del(name)
			if nulls != nil {
				nulls[key] = true
			}
			continue
		}
		if err := setHeader(h, name, *value); err != nil {
			return err
		}
		delete(nulls, key)
	}
	return nil
}

// defaultHeaders is a client's `defaultHeaders` option as its buildHeaders
// reads it: pi's header object, or the Headers a client built from it at
// construction (builtHeaders).
type defaultHeaders interface {
	applyAsDefaultHeaders(h http.Header, checkName headerNameCheck) error
}

// builtHeaders is what buildHeaders returns: a Headers object, every value in
// it converted and checked already, and the names a marker left deleted.
// openai's constructor keeps one as its defaultHeaders when
// OPENAI_CUSTOM_HEADERS is set (see openAIClientHeaders), so writing it at
// request time cannot fail.
type builtHeaders struct {
	header http.Header
	nulls  map[string]bool
}

// buildDefaultHeaders is buildHeaders over plain-object bundles, in order,
// each name checked by checkName.
func buildDefaultHeaders(checkName headerNameCheck, bundles ...*headerObject) (builtHeaders, error) {
	b := builtHeaders{header: http.Header{}, nulls: map[string]bool{}}
	for _, o := range bundles {
		if err := o.fold(b.header, b.nulls, checkName); err != nil {
			return builtHeaders{}, err
		}
	}
	return b, nil
}

// applyAsDefaultHeaders writes the built Headers onto h; every name in it was
// checked when it was built.
func (b builtHeaders) applyAsDefaultHeaders(h http.Header, _ headerNameCheck) error {
	for key, values := range b.header {
		h[key] = slices.Clone(values)
	}
	for key := range b.nulls {
		h.Del(key)
	}
	return nil
}

// sdkHeaders is one request's headers as a vendor SDK's buildHeaders folds
// them together (openai 7.19.0 and @anthropic-ai/sdk 0.129.0,
// client.buildHeaders), bundle by bundle in this order:
//
//	[own, auth, defaults, body, request]
//
// A bundle replaces any header an earlier one set under the same name,
// whatever its spelling, and a marker in defaults deletes one. pi's header
// object is the defaults bundle alone: the headers the SDK owns sit in bundles
// of their own around it, so no spelling in pi's object can take their slot,
// and body and request come after it, where no default can change them.
// Measured on pi's wire with both SDKs: {"Content-Type": "text/plain"} and
// {"content-type": null} still send `content-type: application/json`, and
// model {"Authorization": "a"} under opts {"authorization": "b"} sends `b`.
//
// The values are converted in the order the SDK builds them, which is not the
// fold order: request first (the resource method builds it before calling the
// client), then auth (authHeaders builds its own Headers before the fold
// starts), then own, defaults and body as the fold reaches them.
type sdkHeaders struct {
	// own is the bundle the SDK writes for itself (Accept, anthropic-version).
	// Its User-Agent is left out, as pi's object always replaces or deletes
	// it, and so is its X-Stainless-* telemetry, which describes the JS
	// runtime.
	own []recordEntry
	// auth is the header the SDK builds from its apiKey or authToken option.
	auth []recordEntry
	// defaults is the client's `defaultHeaders`: pi's header object, or the
	// Headers openai's constructor built from it (see openAIClientHeaders).
	defaults defaultHeaders
	// body is the bundle the SDK adds for a JSON body.
	body []recordEntry
	// request is the per-request `headers` option: the anthropic-beta header
	// the beta namespace lifts out of the params.
	request []recordEntry
	// checkName is the SDK's check of a name in defaults: the bundles it
	// writes itself carry names it takes.
	checkName headerNameCheck
}

// jsonBody is the SDKs' body bundle for the JSON params every adapter sends.
var jsonBody = []recordEntry{{"content-type", "application/json"}}

// apply writes the headers onto h, failing where the SDK throws, before
// anything is sent.
func (s sdkHeaders) apply(h http.Header) error {
	request, err := headerValues(s.request)
	if err != nil {
		return err
	}
	auth, err := headerValues(s.auth)
	if err != nil {
		return err
	}
	own, err := headerValues(s.own)
	if err != nil {
		return err
	}
	for _, bundle := range [][]recordEntry{own, auth} {
		for _, e := range bundle {
			h.Set(e.name, e.value)
		}
	}
	if err := s.defaults.applyAsDefaultHeaders(h, s.checkName); err != nil {
		return err
	}
	body, err := headerValues(s.body)
	if err != nil {
		return err
	}
	for _, bundle := range [][]recordEntry{body, request} {
		for _, e := range bundle {
			h.Set(e.name, e.value)
		}
	}
	return nil
}

// headerValues converts each entry's value as a Headers object stores it (see
// headerValue), in order, and fails on the first that it refuses.
func headerValues(entries []recordEntry) ([]recordEntry, error) {
	out := make([]recordEntry, len(entries))
	for i, e := range entries {
		wire, err := headerValue(e.value)
		if err != nil {
			return nil, err
		}
		out[i] = recordEntry{name: e.name, value: wire}
	}
	return out, nil
}

// openAIClientHeaders is the headers openai's client sends with every request
// pi makes through it (see sdkHeaders), given pi's object o and the api key.
// pi builds the client once per stream, in createClient, before the params and
// onPayload, and openai 7.19.0's constructor reads the environment then:
//
//   - OPENAI_ORG_ID and OPENAI_PROJECT_ID become the OpenAI-Organization and
//     OpenAI-Project headers of its own bundle. pi passes neither option, so
//     every request through the client sends them, whatever the provider.
//   - OPENAI_CUSTOM_HEADERS is folded, below pi's object, into the Headers the
//     client keeps as its defaultHeaders (buildHeaders([parsed, o])). Every
//     name and value in both is converted and checked there, so one that fails
//     fails the stream before the params are built, and ahead of the api key.
func openAIClientHeaders(o *headerObject, apiKey string) (sdkHeaders, error) {
	s := sdkHeaders{
		// Accept is application/json whether or not the request streams.
		own:       []recordEntry{{"accept", "application/json"}},
		auth:      []recordEntry{{"authorization", "Bearer " + apiKey}},
		defaults:  o,
		body:      jsonBody,
		checkName: openaiHeaderName,
	}
	if org, ok := sdkEnv("OPENAI_ORG_ID"); ok {
		s.own = append(s.own, recordEntry{"openai-organization", org})
	}
	if project, ok := sdkEnv("OPENAI_PROJECT_ID"); ok {
		s.own = append(s.own, recordEntry{"openai-project", project})
	}
	if parsed, ok := sdkCustomHeaders("OPENAI_CUSTOM_HEADERS"); ok {
		built, err := buildDefaultHeaders(openaiHeaderName, parsed, o)
		if err != nil {
			return sdkHeaders{}, err
		}
		s.defaults = built
	}
	return s, nil
}

// sdkEnv reads an environment variable as both SDKs' readEnv does: from the
// process environment, which is where they look (pi's options.env never
// reaches them), trimmed as String.prototype.trim trims, and unset when that
// leaves nothing.
func sdkEnv(name string) (string, bool) {
	value := jstext.Trim(os.Getenv(name))
	return value, value != ""
}

// sdkCustomHeaders parses the <SDK>_CUSTOM_HEADERS variable env as both SDKs'
// constructors do, into the plain object they fold into defaultHeaders: one
// `Name: value` per LF-separated line, split at its first colon, both sides
// trimmed. A line with no colon is skipped, and a line naming a key an earlier
// one named assigns over it in place. ok is false when env is unset, which is
// the only case the constructor skips: a value with no header line still
// makes openai build its defaultHeaders.
func sdkCustomHeaders(env string) (parsed *headerObject, ok bool) {
	value, ok := sdkEnv(env)
	if !ok {
		return nil, false
	}
	parsed = &headerObject{}
	for _, line := range strings.Split(value, "\n") {
		if name, value, found := strings.Cut(line, ":"); found {
			parsed.set(jstext.Trim(name), jstext.Trim(value))
		}
	}
	return parsed, true
}

// recordEntry is one header of pi's providerHeadersToRecord result: the
// spelling that survived for its case-folded name, and its value.
type recordEntry struct{ name, value string }

// record folds the object the way pi's providerHeadersToRecord does since
// upstream a328aa89a. Names fold case-insensitively in slot order: each slot
// deletes whatever an earlier slot left under its folded name, then stores its
// own spelling and value — unless its value is a marker, which leaves the
// folded name deleted. So the LAST slot for a name decides it, a marker there
// removes every earlier spelling instead of merely being skipped, and a
// re-stored name moves to the end, as a JS Map's delete-then-set does. An
// array-index name has no other spelling, so it keeps the leading slot the
// object gave it, where pi's Object.fromEntries enumerates it too.
//
// The fold is JavaScript's toLowerCase (jstext.ToLower), and it runs on every
// slot before any name is converted: on a marker's name, which never is, and on
// a name that fails conversion later. So it decides which spellings survive to
// be converted, and strings.ToLower would decide differently: {"X-\u0130":
// "v", "x-i": nil} keeps "X-\u0130" in pi, which then fails the request.
func (o *headerObject) record() []recordEntry {
	var out []recordEntry
	for _, name := range o.names {
		folded := jstext.ToLower(name)
		out = slices.DeleteFunc(out, func(e recordEntry) bool { return jstext.ToLower(e.name) == folded })
		if value := o.values[name]; value != nil {
			out = append(out, recordEntry{name: name, value: *value})
		}
	}
	return out
}

// applyAsRecord writes pi's providerHeadersToRecord result onto h, for the
// adapters that build the request themselves. The record is folded before it
// reaches h (see record), so a marker cancels every spelling an earlier slot
// gave its name.
//
// pi spreads the record over the adapter's literal headers — pi-messages'
// `{authorization, accept, "content-type", ...record}`, or @google/genai's own
// defaults — and the transport builds its Headers from that plain object with
// Headers.append. literals are those headers, spelled exactly as pi's object
// spells them, and this replays both steps: a record entry whose name matches
// a literal EXACTLY replaces its value in place (JS spread), while one that
// matches only case-insensitively is a second key there, and Headers.append
// joins the two with ", " in slot order — literal first. Measured with node
// fetch: opts {"Authorization": "x"} on pi-messages sends
// `authorization: Bearer <key>, x`, where {"authorization": "x"} sends `x`.
//
// It never deletes from h: a marker is not part of the record, so it cannot
// remove a literal. Nor does it touch a header the adapter sets on h after
// calling this (google's x-goog-api-key, which genai appends only when the
// Headers holds none yet).
//
// This is google's order: genai appends the object to its Headers entry by
// entry, so each entry is converted (see byteString) and then checked before
// the next is looked at, and the first entry that fails fails the request
// before it is sent. pi-messages hands the object to fetch instead, which
// orders the two steps differently: see applyAsFetchInit.
func (o *headerObject) applyAsRecord(h http.Header, literals ...recordEntry) error {
	a := recordAppender{h: h, appended: make(map[string]bool, len(literals))}
	for _, e := range o.spread(literals) {
		c, err := convertEntry(e)
		if err != nil {
			return err
		}
		if err := a.append(c); err != nil {
			return err
		}
	}
	return nil
}

// applyAsFetchInit is applyAsRecord for pi-messages, which passes the object to
// fetch as `headers`. fetch converts the whole init, every name and value in
// object order, before its Headers appends any entry, so an entry that does not
// convert fails the request ahead of an earlier entry that converts but is
// refused. Measured with node fetch: {"X-B": "a\nb", "X-C": U+65E5} fails on
// X-C's conversion here, and on X-B's LF on every other adapter.
func (o *headerObject) applyAsFetchInit(h http.Header, literals ...recordEntry) error {
	object := o.spread(literals)
	converted := make([]convertedEntry, len(object))
	for i, e := range object {
		c, err := convertEntry(e)
		if err != nil {
			return err
		}
		converted[i] = c
	}
	a := recordAppender{h: h, appended: make(map[string]bool, len(object))}
	for _, c := range converted {
		if err := a.append(c); err != nil {
			return err
		}
	}
	return nil
}

// convertedEntry is an entry of the object pi hands to a Headers, once its
// name and value converted to ByteStrings.
type convertedEntry struct {
	recordEntry
	// wire is the value's ByteString, the bytes the wire carries.
	wire string
}

// convertEntry converts e's name, then its value (see byteString).
func convertEntry(e recordEntry) (convertedEntry, error) {
	if _, err := byteString(e.name); err != nil {
		return convertedEntry{}, err
	}
	wire, err := byteString(e.value)
	if err != nil {
		return convertedEntry{}, err
	}
	return convertedEntry{recordEntry: e, wire: wire}, nil
}

// recordAppender writes converted entries onto h as Headers.append does with
// arguments it has converted: it normalizes the value, checks the name and
// then the value, and joins a value onto one the Headers already holds under
// the name.
type recordAppender struct {
	h        http.Header
	appended map[string]bool
}

func (a *recordAppender) append(e convertedEntry) error {
	if err := checkHeaderToken("Headers.append", e.name); err != nil {
		return err
	}
	value, err := normalizeHeaderValue(e.value, e.wire)
	if err != nil {
		return err
	}
	key := http.CanonicalHeaderKey(e.name)
	if a.appended[key] {
		// Headers.append normalizes the value it appends and joins it onto the
		// one already held with ", ". An empty second value leaves the
		// separator's space at the end: genai re-normalizes the joined value and
		// sends none, and pi-messages' HTTP/1.1 wire carries it as whitespace
		// outside the field value. HTTP/2 forbids a value ending in whitespace,
		// so the joined value is normalized as genai's is.
		a.h[key] = []string{trimHTTPWhitespace(a.h[key][0] + ", " + value)}
		return nil
	}
	a.h.Set(e.name, value)
	a.appended[key] = true
	return nil
}

// spread is the plain object pi hands to its Headers: the literals, with the
// record spread over them. A record entry whose name matches a literal exactly
// replaces its value in place; any other entry is added where the object
// enumerates it (see insertOwnKey), so an array-index name goes ahead of the
// literals.
func (o *headerObject) spread(literals []recordEntry) []recordEntry {
	object := slices.Clone(literals)
	for _, e := range o.record() {
		if i := slices.IndexFunc(object, func(l recordEntry) bool { return l.name == e.name }); i >= 0 {
			object[i].value = e.value
		} else {
			object = insertOwnKey(object, e, func(e recordEntry) string { return e.name })
		}
	}
	return object
}

// setHeader writes one header value the way a fetch Headers object stores it
// (see headerValue), failing as Headers.append throws. Every header value an
// adapter writes goes through here or through headerValue, because in pi every
// one goes through a Headers object. A whitespace-only User-Agent normalizes to
// empty, which net/http then omits: docs/UPSTREAM.md D11, which pi sends
// present and empty.
func setHeader(h http.Header, name, value string) error {
	wire, err := headerValue(value)
	if err != nil {
		return err
	}
	h.Set(name, wire)
	return nil
}

// headerValue is what a fetch Headers object makes of a value it is given: the
// value converted to a ByteString, whose bytes are what the wire carries, then
// normalized and checked (see normalizeHeaderValue).
func headerValue(value string) (string, error) {
	wire, err := byteString(value)
	if err != nil {
		return "", err
	}
	return normalizeHeaderValue(value, wire)
}

// normalizeHeaderValue finishes what Headers.append does to a value once it
// converted to wire (see byteString): it normalizes it (see
// trimHTTPWhitespace), then refuses it if a NUL, CR or LF is left inside, with
// the TypeError text undici throws. That text quotes the normalized value as
// the caller spelled it, which is value trimmed.
func normalizeHeaderValue(value, wire string) (string, error) {
	wire = trimHTTPWhitespace(wire)
	if strings.ContainsAny(wire, "\x00\r\n") {
		return "", fmt.Errorf("Headers.append: \"%s\" is an invalid header value.", trimHTTPWhitespace(value))
	}
	return wire, nil
}

// checkHeaderName fails on a header name a fetch Headers object refuses: one it
// cannot convert (see byteString), or one that is not a token (see
// checkHeaderToken). op is the Headers method that sees the name first.
func checkHeaderName(op, name string) error {
	if _, err := byteString(name); err != nil {
		return err
	}
	return checkHeaderToken(op, name)
}

// checkHeaderToken refuses a converted name that is not an HTTP token with the
// TypeError text undici's Headers throws, which names the method, op, that
// refused it.
func checkHeaderToken(op, name string) error {
	if !isHTTPToken(name) {
		return fmt.Errorf("%s: \"%s\" is an invalid header name.", op, name)
	}
	return nil
}

// isHTTPToken reports whether name is a non-empty run of tchar (RFC 9110
// section 5.6.2): a visible ASCII character other than a delimiter. A
// character above U+007F is not one, whatever its encoding.
func isHTTPToken(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		if c := name[i]; c <= ' ' || c >= 0x7f || strings.IndexByte(`"(),/:;<=>?@[\]{}`, c) >= 0 {
			return false
		}
	}
	return true
}

// byteString converts s the way WebIDL's ByteString conversion converts a
// Headers argument, which is the only form a header name or value takes in pi.
// Each UTF-16 code unit becomes one byte: a string of code units up to 0xFF
// returns their bytes, so U+0080..U+00FF become single Latin-1 bytes where the
// Go string holds two UTF-8 bytes. The first code unit above 0xFF fails the
// conversion with the TypeError text node's fetch throws, which pi's adapters
// report as the stream's error message; its index and value are counted in
// UTF-16 code units, so a character outside the BMP reports its high
// surrogate. A Go string is text here, as a JS string is: an invalid UTF-8
// byte reads as U+FFFD, and fails.
func byteString(s string) (string, error) {
	var latin1 []byte // nil until the first byte that is not ASCII
	for i, r := range s {
		if r < utf8.RuneSelf {
			if latin1 != nil {
				latin1 = append(latin1, byte(r))
			}
			continue
		}
		if r > 0xFF {
			// Every character before i was one code unit (at most 0xFF).
			unit := utf8.RuneCountInString(s[:i])
			value := r
			if r > 0xFFFF {
				value, _ = utf16.EncodeRune(r)
			}
			return "", fmt.Errorf("Cannot convert argument to a ByteString because the character at index %d has a value of %d which is greater than 255.", unit, value)
		}
		if latin1 == nil {
			latin1 = append(make([]byte, 0, len(s)), s[:i]...)
		}
		latin1 = append(latin1, byte(r))
	}
	if latin1 == nil {
		return s, nil
	}
	return string(latin1), nil
}

// trimHTTPWhitespace strips what the Fetch standard calls HTTP whitespace —
// space, tab, CR and LF — from both ends, as Headers.append and Headers.set do
// to a value (the Fetch standard's "normalize"). A CR or LF left inside is
// refused afterwards (see normalizeHeaderValue).
func trimHTTPWhitespace(v string) string { return strings.Trim(v, " \t\r\n") }

// sortedNames returns m's keys in sorted order, the tie-break every source-local
// header ordering in this package uses.
func sortedNames[V any](m map[string]V) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
