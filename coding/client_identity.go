package coding

import (
	"os"
	"strings"

	"github.com/sky-valley/pi/ai"
)

// clientIdentity is how a session's requests identify pi to providers (pi
// getClientIdentity, core/client-identity.ts, upstream adc5fa9f8): a detailed
// User-Agent with pi's version, and attribution to pi as a CLI agent while
// install telemetry is enabled. It replaces the agent loop's default identity
// on every request the session makes, compaction and summaries included, and
// is read per request, so a telemetry change applies at once.
//
// pi's version is the coding agent's package version, which its release
// script keeps equal to pi-ai's; the port has the one, ai.PiAIVersion.
func clientIdentity() *ai.ClientIdentity {
	attribution := ai.NoAttribution
	if isInstallTelemetryEnabled() {
		attribution = &ai.PiAttribution
	}
	return &ai.ClientIdentity{UserAgent: ai.UserAgentDetailed, Version: ai.PiAIVersion, Attribution: attribution}
}

// isInstallTelemetryEnabled is pi's isInstallTelemetryEnabled (telemetry.ts):
// PI_TELEMETRY when set, else the enableInstallTelemetry setting, which
// defaults to true. The port has no settings manager, so without the variable
// telemetry is on, as pi's default is.
func isInstallTelemetryEnabled() bool {
	value, ok := os.LookupEnv("PI_TELEMETRY")
	if !ok {
		return true
	}
	return isTruthyEnvFlag(value)
}

// isTruthyEnvFlag is pi's isTruthyEnvFlag: "1", or "true" or "yes" in any case.
func isTruthyEnvFlag(value string) bool {
	lower := strings.ToLower(value)
	return value == "1" || lower == "true" || lower == "yes"
}
