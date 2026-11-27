package coding

import "github.com/sky-valley/pi/ai"

// clientIdentity is how a session's requests identify pi to providers (pi
// getClientIdentity, core/client-identity.ts, upstream adc5fa9f8, 484da3a86):
// a detailed User-Agent with pi's version, and attribution to pi as a CLI
// agent. It replaces the agent loop's default identity on every request the
// session makes, compaction and summaries included.
//
// pi's version is the coding agent's package version, which its release
// script keeps equal to pi-ai's; the port has the one, ai.PiAIVersion.
func clientIdentity() *ai.ClientIdentity {
	return &ai.ClientIdentity{UserAgent: ai.UserAgentDetailed, Version: ai.PiAIVersion, Attribution: &ai.PiAttribution}
}
