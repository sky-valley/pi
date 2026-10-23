package providers

import (
	"slices"

	"github.com/sky-valley/pi/ai"
)

// identityHeaderOrder is the order pi's getIdentityHeaders writes its object
// in: the User-Agent first, then a target's attribution headers as
// getAttributionHeaders lists them. Within one object the order decides
// nothing on the wire (the names never collide), but writing it keeps each
// slot where pi's is, which a later source's case variant reads.
var identityHeaderOrder = []string{
	"User-Agent",
	"HTTP-Referer", "X-OpenRouter-Title", "X-OpenRouter-Categories",
	"X-BILLING-INVOKE-ORIGIN",
	"x-opencode-client",
}

// setIdentityHeaders writes pi's getIdentityHeaders(model, identity) into o
// (upstream adc5fa9f8): the identity's User-Agent and provider attribution,
// the first source of every adapter's header merge, so model, provider and
// request headers all outrank it and a request's deletion marker removes it.
func setIdentityHeaders(o *headerObject, model *ai.Model, identity *ai.ClientIdentity) {
	headers := ai.GetIdentityHeaders(model.IdentityTarget(), identity)
	for _, name := range identityHeaderOrder {
		if value, ok := headers[name]; ok {
			o.set(name, value)
			delete(headers, name)
		}
	}
	// Anything getIdentityHeaders adds beyond the names above follows them.
	rest := make([]string, 0, len(headers))
	for name := range headers {
		rest = append(rest, name)
	}
	slices.Sort(rest)
	for _, name := range rest {
		o.set(name, headers[name])
	}
}
