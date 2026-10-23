package providers

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// openRouterCostCaptureFile is written by testdata/openrouter-cost/capture.mts,
// which runs upstream's own packages/ai/src/utils/openrouter-cost.ts under node.
const openRouterCostCaptureFile = "testdata/openrouter-cost/openrouter-cost-cae8d69e5.json"

type openRouterCostCapture struct {
	SHA   string `json:"sha"`
	Apply []struct {
		Covers            string           `json:"covers"`
		Reported          string           `json:"reported"`
		TotalFromProvider *bool            `json:"totalFromProvider"`
		Threw             string           `json:"threw"`
		Cost              *json.RawMessage `json:"cost"`
	} `json:"apply"`
	IsOpenRouterModel []struct {
		Provider   string `json:"provider"`
		BaseURL    string `json:"baseUrl"`
		OpenRouter bool   `json:"openRouter"`
	} `json:"isOpenRouterModel"`
}

func loadOpenRouterCostCapture(t *testing.T) openRouterCostCapture {
	t.Helper()
	data, err := os.ReadFile(openRouterCostCaptureFile)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate it with testdata/openrouter-cost/capture.mts)", openRouterCostCaptureFile, err)
	}
	var capture openRouterCostCapture
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("decode %s: %v", openRouterCostCaptureFile, err)
	}
	if len(capture.Apply) == 0 || len(capture.IsOpenRouterModel) == 0 {
		t.Fatalf("%s holds no rows", openRouterCostCaptureFile)
	}
	return capture
}

// TestApplyOpenRouterReportedCostMatchesPi replays every accounting pi
// classified onto openrouter-cost.test.ts's usage and requires pi's cost
// exactly, totalFromProvider's presence included.
func TestApplyOpenRouterReportedCostMatchesPi(t *testing.T) {
	capture := loadOpenRouterCostCapture(t)
	for i, row := range capture.Apply {
		if row.Threw != "" {
			// pi reads properties of a null report and throws; neither caller
			// can pass one (both hold a usage object), and the port's no-op
			// reads the same as never being called.
			continue
		}
		usage := ai.Usage{Input: 1, Output: 1, TotalTokens: 2,
			Cost: ai.CostBreakdown{Input: 0.25, Output: 0.75, Total: 1}}
		if row.TotalFromProvider != nil {
			usage.Cost.TotalFromProvider = ai.OptBoolOf(*row.TotalFromProvider)
		}
		reported, err := ai.DecodeOrderedValue([]byte(row.Reported))
		if err != nil {
			t.Fatalf("row %d: %v", i, err)
		}
		applyOpenRouterReportedCost(&usage, reported)
		var want ai.CostBreakdown
		if err := json.Unmarshal(*row.Cost, &want); err != nil {
			t.Fatalf("row %d: %v", i, err)
		}
		if usage.Cost != want {
			t.Errorf("row %d (%s): reported %s gives cost %+v, pi at %s %+v", i, row.Covers, row.Reported, usage.Cost, capture.SHA, want)
		}
	}
}

func TestIsOpenRouterModelMatchesPi(t *testing.T) {
	capture := loadOpenRouterCostCapture(t)
	for _, row := range capture.IsOpenRouterModel {
		if got := isOpenRouterModel(&ai.Model{Provider: row.Provider, BaseURL: row.BaseURL}); got != row.OpenRouter {
			t.Errorf("isOpenRouterModel(%q, %q) = %v, pi at %s %v", row.Provider, row.BaseURL, got, capture.SHA, row.OpenRouter)
		}
	}
}
