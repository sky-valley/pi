package coding

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// modelResolverCapture is testdata/modelresolver: pi-coding-agent 1.3.0's
// resolveModelReference and resolveCliModel (upstream d7f7e0dfe) over
// model-resolver.test.ts's catalogs and cases, plus edge cases.
type modelResolverCapture struct {
	Catalogs   map[string][]*ai.Model `json:"catalogs"`
	References []struct {
		Name    string `json:"name"`
		Request struct {
			Text           string `json:"text"`
			Provider       string `json:"provider"`
			ThinkingSuffix string `json:"thinkingSuffix"`
			Fuzzy          bool   `json:"fuzzy"`
		} `json:"request"`
		Usable []string `json:"usable"`
		Result struct {
			Status        string   `json:"status"`
			Model         string   `json:"model"`
			Candidates    []string `json:"candidates"`
			UsableCount   int      `json:"usableCount"`
			ThinkingLevel string   `json:"thinkingLevel"`
			Warning       string   `json:"warning"`
			Target        *struct {
				Provider       string   `json:"provider"`
				ProviderModels []string `json:"providerModels"`
				ModelText      string   `json:"modelText"`
				ModelID        string   `json:"modelId"`
				ThinkingLevel  string   `json:"thinkingLevel"`
			} `json:"target"`
		} `json:"result"`
	} `json:"references"`
	CLI []struct {
		Name         string   `json:"name"`
		Catalog      string   `json:"catalog"`
		Configured   []string `json:"configured"`
		CLIProvider  string   `json:"cliProvider"`
		CLIModel     string   `json:"cliModel"`
		CLIThinking  string   `json:"cliThinking"`
		InlineAPIKey bool     `json:"inlineApiKey"`
		Result       struct {
			Model *struct {
				Provider      string `json:"provider"`
				ID            string `json:"id"`
				Name          string `json:"name"`
				Reasoning     bool   `json:"reasoning"`
				BaseURL       string `json:"baseUrl"`
				ContextWindow int    `json:"contextWindow"`
			} `json:"model"`
			ThinkingLevel string `json:"thinkingLevel"`
			Warning       string `json:"warning"`
			Error         string `json:"error"`
		} `json:"result"`
	} `json:"cli"`
}

func loadModelResolverCapture(t *testing.T) modelResolverCapture {
	t.Helper()
	data, err := os.ReadFile("testdata/modelresolver/modelresolver-1.3.0.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture modelResolverCapture
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatal(err)
	}
	if len(capture.References) == 0 || len(capture.CLI) == 0 {
		t.Fatal("the model resolver capture holds no cases")
	}
	return capture
}

func modelRefs(models []*ai.Model) []string {
	refs := make([]string, len(models))
	for i, m := range models {
		refs[i] = string(m.Provider) + "/" + m.ID
	}
	slices.Sort(refs)
	return refs
}

func reversedModels(models []*ai.Model) []*ai.Model {
	out := slices.Clone(models)
	slices.Reverse(out)
	return out
}

var thinkingSuffixModes = map[string]thinkingSuffixMode{
	"none": thinkingSuffixNone, "strict": thinkingSuffixStrict, "lenient": thinkingSuffixLenient,
}

// Every reference resolves as pi's resolveModelReference does, over the
// catalog in order and reversed: catalog order never decides.
func TestResolveModelReferenceMatchesPi(t *testing.T) {
	capture := loadModelResolverCapture(t)
	catalog := capture.Catalogs["reference"]
	for _, tc := range capture.References {
		t.Run(tc.Name, func(t *testing.T) {
			mode, ok := thinkingSuffixModes[tc.Request.ThinkingSuffix]
			if !ok {
				t.Fatalf("unknown thinkingSuffix %q", tc.Request.ThinkingSuffix)
			}
			request := modelReferenceRequest{text: tc.Request.Text, provider: tc.Request.Provider, thinkingSuffix: mode, fuzzy: tc.Request.Fuzzy}
			usable := func(provider string) bool { return slices.Contains(tc.Usable, provider) }
			want := tc.Result
			for _, models := range [][]*ai.Model{catalog, reversedModels(catalog)} {
				got := resolveModelReference(request, models, usable)
				if got.status.String() != want.Status {
					t.Fatalf("status = %s, pi %s (%+v)", got.status, want.Status, got)
				}
				if got.thinkingLevel != want.ThinkingLevel || got.warning != want.Warning {
					t.Fatalf("thinking level %q warning %q, pi %q %q", got.thinkingLevel, got.warning, want.ThinkingLevel, want.Warning)
				}
				switch got.status {
				case referenceResolved:
					if ref := string(got.model.Provider) + "/" + got.model.ID; ref != want.Model {
						t.Fatalf("model = %s, pi %s", ref, want.Model)
					}
				case referenceAmbiguous:
					if refs := modelRefs(got.candidates); !slices.Equal(refs, want.Candidates) || got.usableCount != want.UsableCount {
						t.Fatalf("ambiguous %q (%d usable), pi %q (%d)", refs, got.usableCount, want.Candidates, want.UsableCount)
					}
				case referenceNotFound:
					if (got.target == nil) != (want.Target == nil) {
						t.Fatalf("target = %+v, pi %+v", got.target, want.Target)
					}
					if got.target != nil {
						target := *want.Target
						if got.target.provider != target.Provider || !slices.Equal(modelRefs(got.target.providerModels), target.ProviderModels) ||
							got.target.modelText != target.ModelText || got.target.modelID != target.ModelID ||
							got.target.thinkingLevel != target.ThinkingLevel {
							t.Fatalf("target = %+v, pi %+v", *got.target, target)
						}
						if got.target.providerModels[0].Provider != got.target.provider {
							t.Fatalf("target's first model is %s/%s, not the target provider's", got.target.providerModels[0].Provider, got.target.providerModels[0].ID)
						}
					}
				}
			}
		})
	}
}

// resolveCliModel's model or error, thinking level and warning are pi's.
func TestResolveCliModelMatchesPi(t *testing.T) {
	capture := loadModelResolverCapture(t)
	for _, tc := range capture.CLI {
		t.Run(tc.Name, func(t *testing.T) {
			models, ok := capture.Catalogs[tc.Catalog]
			if !ok {
				t.Fatalf("no catalog %q", tc.Catalog)
			}
			got, err := resolveCliModel(cliModelOptions{
				provider: tc.CLIProvider, model: tc.CLIModel, thinking: tc.CLIThinking, inlineAPIKey: tc.InlineAPIKey,
				models:            models,
				hasConfiguredAuth: func(provider string) bool { return slices.Contains(tc.Configured, provider) },
			})
			want := tc.Result
			if want.Error != "" {
				if err == nil || err.Error() != want.Error {
					t.Fatalf("error = %v, pi %q", err, want.Error)
				}
				if got.Model != nil {
					t.Fatalf("model = %+v with an error", got.Model)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.ThinkingLevel != want.ThinkingLevel || got.Warning != want.Warning {
				t.Fatalf("thinking level %q warning %q, pi %q %q", got.ThinkingLevel, got.Warning, want.ThinkingLevel, want.Warning)
			}
			if want.Model == nil {
				if got.Model != nil {
					t.Fatalf("model = %s/%s, pi none", got.Model.Provider, got.Model.ID)
				}
				return
			}
			if got.Model == nil {
				t.Fatalf("no model, pi %+v", *want.Model)
			}
			gotModel := *want.Model
			gotModel.Provider, gotModel.ID, gotModel.Name = string(got.Model.Provider), got.Model.ID, got.Model.Name
			gotModel.Reasoning, gotModel.BaseURL, gotModel.ContextWindow = got.Model.Reasoning, got.Model.BaseURL, got.Model.ContextWindow
			if !reflect.DeepEqual(gotModel, *want.Model) {
				t.Fatalf("model = %+v, pi %+v", gotModel, *want.Model)
			}
		})
	}
}

// A fallback model is a copy: building one never touches the catalog entry
// it is templated from.
func TestResolveCliModelFallbackDoesNotMutateTheCatalog(t *testing.T) {
	template := &ai.Model{Provider: "mistral", ID: "codestral-latest", Name: "Codestral"}
	got, err := resolveCliModel(cliModelOptions{model: "mistral/custom:high", models: []*ai.Model{template}, hasConfiguredAuth: func(string) bool { return false }})
	if err != nil || got.Model == template || template.ID != "codestral-latest" || template.Reasoning {
		t.Fatalf("fallback %+v (err %v) aliased or changed the template %+v", got.Model, err, template)
	}
}

// A provider id may itself contain "/" (a URL-derived id): its references
// resolve by the longest known provider id, not at the first slash. These are
// model-resolver.test.ts's referenceCases from upstream 984cf6643, over its
// reference catalog with the model that commit adds; the 1.3.0 capture
// predates both.
func TestResolveModelReferenceProviderIDWithSlash(t *testing.T) {
	catalog := slices.Clone(loadModelResolverCapture(t).Catalogs["reference"])
	catalog = append(catalog, &ai.Model{ID: "unsloth/qwen-gguf:BF16", Name: "unsloth/qwen-gguf:BF16", Provider: "llama=http://host:10800"})
	for _, tc := range []struct{ name, text, model, thinkingLevel string }{
		{"provider id containing a slash", "llama=http://host:10800/unsloth/qwen-gguf:BF16", "llama=http://host:10800/unsloth/qwen-gguf:BF16", ""},
		{"provider id containing a slash, with thinking suffix", "llama=http://host:10800/unsloth/qwen-gguf:BF16:high", "llama=http://host:10800/unsloth/qwen-gguf:BF16", "high"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := modelReferenceRequest{text: tc.text, thinkingSuffix: thinkingSuffixStrict, fuzzy: true}
			for _, models := range [][]*ai.Model{catalog, reversedModels(catalog)} {
				got := resolveModelReference(request, models, func(string) bool { return false })
				if got.status != referenceResolved {
					t.Fatalf("status = %s, pi resolved (%+v)", got.status, got)
				}
				if ref := string(got.model.Provider) + "/" + got.model.ID; ref != tc.model || got.thinkingLevel != tc.thinkingLevel || got.warning != "" {
					t.Fatalf("model %s thinking level %q warning %q, pi %s %q none", ref, got.thinkingLevel, got.warning, tc.model, tc.thinkingLevel)
				}
			}
		})
	}
}

// splitProviderPrefix is pi's, JS string semantics included: provider ids
// match longest first, case and surrounding whitespace aside, and the model
// text is cut from the original text at the UTF-16 index of the slash in its
// toLowerCase, so a character that lowers to two code units ("İ" to "i̇")
// shifts the cut. Rows are node's answers for upstream 984cf6643's function.
func TestSplitProviderPrefixMatchesPi(t *testing.T) {
	for _, tc := range []struct {
		text                string
		providers           []string
		provider, modelText string // provider "" is pi's undefined
	}{
		{"llama=http://host:10800/unsloth/qwen-gguf:BF16", []string{"llama=http://host:10800", "llama=http:"}, "llama=http://host:10800", "unsloth/qwen-gguf:BF16"},
		{"İ/model", []string{"i̇"}, "i̇", "odel"},
		{"İİ/ab", []string{"i̇i̇"}, "", ""},
		{"İ/", []string{"i̇"}, "", ""},
		{" Open/x", []string{"open"}, "open", "x"},
		{"a/b/c", []string{"a", "a/b"}, "a/b", "c"},
		{"a/b/c", []string{"a/b", "a"}, "a/b", "c"},
		{"a /b", []string{"A "}, "A ", "b"},
		{"a/b", []string{"a", "A"}, "a", "b"},
		{"😀a/b", []string{"😀a"}, "😀a", "b"},
		{"a//b", []string{"a"}, "a", "/b"},
		{"a/ ", []string{"a"}, "", ""},
	} {
		models := make([]*ai.Model, len(tc.providers))
		for i, p := range tc.providers {
			models[i] = &ai.Model{Provider: ai.ProviderId(p)}
		}
		provider, modelText, ok := splitProviderPrefix(tc.text, models)
		if ok != (tc.provider != "") || provider != tc.provider || modelText != tc.modelText {
			t.Errorf("splitProviderPrefix(%q, %q) = %q, %q, %v; pi %q, %q", tc.text, tc.providers, provider, modelText, ok, tc.provider, tc.modelText)
		}
	}
}
