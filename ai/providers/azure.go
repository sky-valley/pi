package providers

import (
	"fmt"

	"github.com/sky-valley/pi/ai"
)

// azureProvider is the provider id of pi's providers/azure.ts. Upstream
// a37306d43 renamed it from azure-openai-responses when it began serving Chat
// Completions deployments beside the Responses API; the azure-openai-responses
// api id did not change.
const azureProvider ai.ProviderId = "azure"

// azureChatCompletionsError refuses an azure model on openai-completions.
//
// pi dispatches those through azureStreams (providers/azure.ts): the catalog
// ships every azure model with an empty baseUrl, and the wrapper resolves the
// endpoint (azureBaseUrl, AZURE_OPENAI_BASE_URL, AZURE_OPENAI_RESOURCE_NAME,
// model.baseUrl) and the deployment name (AZURE_OPENAI_DEPLOYMENT_NAME_MAP)
// per request, before the shared Chat Completions stream runs. That wrapper
// and api/azure-openai-config.ts are Scope queue row 4, with the Responses
// adapter that shares the config. Without them the empty baseUrl falls through
// to the OpenAI SDK default, api.openai.com, with the Azure key; so the stream
// fails instead, before any request leaves. This goes when row 4 lands.
func azureChatCompletionsError(model *ai.Model) error {
	return fmt.Errorf("Model %s/%s needs pi's Azure endpoint and deployment resolution, which this port does not have yet, so it cannot be streamed. "+
		"Use a model from another provider, or register the deployment as an %s model under a provider id other than %s, with your resource's endpoint "+
		"(https://<resource>.openai.azure.com/openai/v1) as its baseUrl and the deployment name as its id.",
		model.Provider, model.ID, ai.APIOpenAICompletions, azureProvider)
}
