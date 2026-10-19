package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// azureCompletionsCaptureFile is what testdata/azure-completions/capture.mjs
// recorded from pi-ai 1.3.0: the request compat.js's stream()/streamSimple()
// sends for the catalog's azure/deepseek-v4-pro and for an azure model
// registered with a baseUrl, under AZURE_OPENAI_* settings, and how each
// stream ended.
const azureCompletionsCaptureFile = "testdata/azure-completions/azure-completions-1.3.0.json"

// azureRecordingDoer records the request a stream hands its custom client —
// URL, Authorization and body — and answers 400 so nothing streams.
type azureRecordingDoer struct{ sent *[]azureRequest }

type azureRequest struct {
	URL           string `json:"url"`
	Authorization string `json:"authorization"`
	Body          any    `json:"body"`
}

func (d azureRecordingDoer) Do(req *http.Request) (*http.Response, error) {
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	var body any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	*d.sent = append(*d.sent, azureRequest{URL: req.URL.String(), Authorization: req.Header.Get("Authorization"), Body: body})
	return &http.Response{
		StatusCode: http.StatusBadRequest,
		Status:     "400 Bad Request",
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"stop here"}}`)),
		Request:    req,
	}, nil
}

// An azure model on openai-completions is served as pi's azureStreams serves
// it (providers/azure.ts, api/azure-openai-config.ts, upstream a37306d43):
// the endpoint is AZURE_OPENAI_BASE_URL, else AZURE_OPENAI_RESOURCE_NAME's
// https://<name>.openai.azure.com/openai/v1, else the model's baseUrl — an
// Azure host's root, /openai or /openai/v1/responses becoming /openai/v1 —
// and the stream fails with pi's message when there is none, before any
// request or key check; the request's model is the deployment name
// AZURE_OPENAI_DEPLOYMENT_NAME_MAP gives the model id, which a caller's
// onPayload sees, while the message keeps the catalog id. Runs through
// ai.Stream / ai.StreamSimple, the compat entry points a session uses.
func TestAzureChatCompletionsMatchesPi(t *testing.T) {
	for _, name := range []string{"AZURE_OPENAI_API_KEY", "AZURE_OPENAI_BASE_URL", "AZURE_OPENAI_RESOURCE_NAME", "AZURE_OPENAI_DEPLOYMENT_NAME_MAP", "AZURE_OPENAI_API_VERSION"} {
		t.Setenv(name, "")
	}
	data, err := os.ReadFile(azureCompletionsCaptureFile)
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Registered ai.Model `json:"registered"`
		Rows       []struct {
			Name      string            `json:"name"`
			Model     string            `json:"model"`
			Env       map[string]string `json:"env"`
			NoKey     bool              `json:"noKey"`
			Simple    bool              `json:"simple"`
			Reasoning ai.ThinkingLevel  `json:"reasoning"`
			OnPayload bool              `json:"onPayload"`
			Request   *azureRequest     `json:"request"`
			SeenModel any               `json:"seenModel"`
			Result    struct {
				Model        string  `json:"model"`
				StopReason   string  `json:"stopReason"`
				ErrorMessage *string `json:"errorMessage"`
			} `json:"result"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("%s: %v; rerun capture.mjs", azureCompletionsCaptureFile, err)
	}
	if len(capture.Rows) == 0 {
		t.Fatalf("%s holds no rows; rerun capture.mjs", azureCompletionsCaptureFile)
	}
	catalog := ai.GetModel(azureProvider, "deepseek-v4-pro")
	if catalog == nil || catalog.Api != ai.APIOpenAICompletions {
		t.Fatalf("the catalog has no azure/deepseek-v4-pro on openai-completions: %+v", catalog)
	}
	for _, row := range capture.Rows {
		t.Run(row.Name, func(t *testing.T) {
			model := catalog
			if row.Model == "registered" {
				registered := capture.Registered
				model = &registered
			}
			var sent []azureRequest
			var seenModel any
			opts := ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{
				APIKey:     "azure-key",
				HTTPClient: azureRecordingDoer{&sent},
				Env:        row.Env,
			}}
			if row.NoKey {
				opts.APIKey = ""
			}
			if row.OnPayload {
				opts.OnPayload = func(payload any, _ *ai.Model) (any, error) {
					params := payload.(map[string]any)
					seenModel = params["model"]
					next := map[string]any{}
					for k, v := range params {
						next[k] = v
					}
					next["temperature"] = 0.1
					return next, nil
				}
			}
			req := ai.Context{SystemPrompt: "sys", Messages: []ai.Message{ai.NewUserText("hi", 0)}}
			var result *ai.AssistantMessage
			if row.Simple {
				result = ai.CompleteSimple(context.Background(), model, req, &ai.SimpleStreamOptions{StreamOptions: opts, Reasoning: row.Reasoning})
			} else {
				result = ai.Complete(context.Background(), model, req, &opts)
			}
			wantErr := ""
			if row.Result.ErrorMessage != nil {
				wantErr = *row.Result.ErrorMessage
			}
			if result.Model != row.Result.Model || string(result.StopReason) != row.Result.StopReason || result.ErrorMessage != wantErr {
				t.Errorf("ended %s %s %q\npi    %s %s %q", result.Model, result.StopReason, result.ErrorMessage, row.Result.Model, row.Result.StopReason, wantErr)
			}
			switch {
			case row.Request == nil && len(sent) != 0:
				t.Errorf("sent %+v; pi sends nothing", sent)
			case row.Request != nil && len(sent) != 1:
				t.Errorf("sent %d requests; pi sends one", len(sent))
			case row.Request != nil && !reflect.DeepEqual(sent[0], *row.Request):
				got, _ := json.Marshal(sent[0])
				want, _ := json.Marshal(row.Request)
				t.Errorf("request\n%s\npi\n%s", got, want)
			}
			if !reflect.DeepEqual(seenModel, row.SeenModel) {
				t.Errorf("onPayload saw model %v, pi %v", seenModel, row.SeenModel)
			}
		})
	}
}
