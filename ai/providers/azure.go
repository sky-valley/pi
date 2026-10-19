package providers

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"strings"

	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/internal/jstext"
)

// azureProvider is the provider id of pi's providers/azure.ts. Upstream
// a37306d43 renamed it from azure-openai-responses when it began serving Chat
// Completions deployments beside the Responses API; the azure-openai-responses
// api id did not change.
const azureProvider ai.ProviderId = "azure"

// errAzureBaseURLRequired is resolveAzureBaseUrl's error when nothing names an
// endpoint (api/azure-openai-config.ts).
var errAzureBaseURLRequired = errors.New("Azure OpenAI base URL is required. Set AZURE_OPENAI_BASE_URL or AZURE_OPENAI_RESOURCE_NAME, or pass azureBaseUrl, azureResourceName, or model.baseUrl.")

// streamAzureCompletions is pi's azureStreams(openAICompletionsApi()).stream
// (providers/azure.ts): azure models ship without a baseUrl — one resource
// per user — so the endpoint and the deployment are resolved per request,
// before the Chat Completions stream runs. A failure to resolve ends the
// stream with an error event, as pi's lazyStream does, before any key check
// or request. The model the stream sees carries the resolved baseUrl; the
// message keeps its id.
//
// pi reaches azureStreams through the azure provider's api table; the port
// dispatches by api, so StreamOpenAICompletions routes provider azure here,
// as it is the only provider whose table wraps openai-completions this way.
func streamAzureCompletions(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *OpenAIOptions) *ai.AssistantMessageEventStream {
	baseURL, err := resolveAzureBaseURL(model, opts.Env)
	if err != nil {
		return ai.ErrorStream(model, err)
	}
	resolved := *model
	resolved.BaseURL = baseURL
	return streamOpenAICompletions(ctx, &resolved, req, withAzureDeploymentName(model, opts))
}

// withAzureDeploymentName sends the deployment name as the request's model,
// keeping model.ID as the catalog id (pi withDeploymentName): when they
// differ, the payload's "model" is replaced before a caller's OnPayload sees
// it, and the caller's replacement, if any, is what goes out.
func withAzureDeploymentName(model *ai.Model, opts *OpenAIOptions) *OpenAIOptions {
	deployment := resolveAzureDeploymentName(model, opts.Env)
	if deployment == model.ID {
		return opts
	}
	wrapped := *opts
	onPayload := opts.OnPayload
	wrapped.OnPayload = func(payload any, payloadModel *ai.Model) (any, error) {
		params := map[string]any{}
		if body, ok := payload.(map[string]any); ok {
			maps.Copy(params, body)
		}
		params["model"] = deployment
		if onPayload == nil {
			return params, nil
		}
		next, err := onPayload(params, payloadModel)
		if err != nil || next != nil {
			return next, err
		}
		return params, nil
	}
	return &wrapped
}

// resolveAzureDeploymentName is pi's resolveDeploymentName: the deployment
// AZURE_OPENAI_DEPLOYMENT_NAME_MAP names for the model id, else the id. (pi's
// azureDeploymentName option, which comes first, is the Responses adapter's;
// Scope queue row 4.)
func resolveAzureDeploymentName(model *ai.Model, env map[string]string) string {
	if deployment := parseAzureDeploymentNameMap(ai.ProviderEnvValue("AZURE_OPENAI_DEPLOYMENT_NAME_MAP", env))[model.ID]; deployment != "" {
		return deployment
	}
	return model.ID
}

// parseAzureDeploymentNameMap reads "id=deployment,id=deployment" as pi's
// parseDeploymentNameMap does: entries and both sides are JS-trimmed, an
// entry missing either side is skipped, a later entry for an id replaces an
// earlier one, and an entry is split JS-style — split("=", 2) keeps only the
// text between the first "=" and the next.
func parseAzureDeploymentNameMap(value string) map[string]string {
	deployments := map[string]string{}
	for entry := range strings.SplitSeq(value, ",") {
		trimmed := jstext.Trim(entry)
		if trimmed == "" {
			continue
		}
		parts := strings.SplitN(trimmed, "=", 3)
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
			continue
		}
		deployments[jstext.Trim(parts[0])] = jstext.Trim(parts[1])
	}
	return deployments
}

// resolveAzureBaseURL is pi's resolveAzureBaseUrl: AZURE_OPENAI_BASE_URL
// (JS-trimmed; blank counts as unset), else
// https://<AZURE_OPENAI_RESOURCE_NAME>.openai.azure.com/openai/v1, else the
// model's baseUrl, normalized (normalizeAzureBaseURL); errAzureBaseURLRequired
// when there is none. (pi's azureBaseUrl and azureResourceName options, which
// come first, are the Responses adapter's; Scope queue row 4.)
func resolveAzureBaseURL(model *ai.Model, env map[string]string) (string, error) {
	baseURL := jstext.Trim(ai.ProviderEnvValue("AZURE_OPENAI_BASE_URL", env))
	if baseURL == "" {
		if resourceName := ai.ProviderEnvValue("AZURE_OPENAI_RESOURCE_NAME", env); resourceName != "" {
			baseURL = "https://" + resourceName + ".openai.azure.com/openai/v1"
		}
	}
	if baseURL == "" {
		baseURL = model.BaseURL
	}
	if baseURL == "" {
		return "", errAzureBaseURLRequired
	}
	return normalizeAzureBaseURL(baseURL)
}

// normalizeAzureBaseURL is pi's normalizeAzureBaseUrl: the URL as the WHATWG
// parser writes it (requestURL), trailing slashes dropped, with the path of an
// Azure host (*.openai.azure.com, *.cognitiveservices.azure.com,
// *.ai.azure.com) set to /openai/v1 and its query dropped when it is the root,
// /openai or /openai/v1/responses — so the client can append its endpoint.
func normalizeAzureBaseURL(baseURL string) (string, error) {
	serialized, err := requestURL(strings.TrimRight(jstext.Trim(baseURL), "/"))
	if err != nil {
		return "", fmt.Errorf("Invalid Azure OpenAI base URL: %s", baseURL)
	}
	u, err := url.Parse(serialized)
	if err != nil {
		return "", fmt.Errorf("Invalid Azure OpenAI base URL: %s", baseURL)
	}
	host := u.Hostname()
	isAzureHost := strings.HasSuffix(host, ".openai.azure.com") ||
		strings.HasSuffix(host, ".cognitiveservices.azure.com") ||
		strings.HasSuffix(host, ".ai.azure.com")
	switch strings.TrimRight(u.EscapedPath(), "/") {
	case "", "/openai", "/openai/v1/responses":
		if isAzureHost {
			u.Path, u.RawPath = "/openai/v1", ""
			u.RawQuery, u.ForceQuery = "", false
			serialized = u.String()
		}
	}
	return strings.TrimRight(serialized, "/"), nil
}
