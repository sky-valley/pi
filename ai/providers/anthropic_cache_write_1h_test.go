package providers

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// cacheWrite1hCaptureFile is written by
// testdata/cache-write-1h/capture-cache-write-1h.mts, which streams pi's
// anthropic-messages adapter from source at the named sha.
const cacheWrite1hCaptureFile = "testdata/cache-write-1h/cache-write-1h-cae8d69e5.json"

// TestAnthropic1hCacheWriteMatchesPi replays each captured stream at the
// captured rates and requires pi's usage exactly — token counts, cacheWrite1h
// and every cost double. The rows are the edges of where a 1h breakdown is
// read from (upstream 667fc3dd3): a message_delta's breakdown with and without
// the total beside it, one without the 1h key, a null cache_creation, a null
// count, and an explicit 0 — and a second message_start, which resets the
// value whatever its breakdown says.
func TestAnthropic1hCacheWriteMatchesPi(t *testing.T) {
	data, err := os.ReadFile(cacheWrite1hCaptureFile)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate it with testdata/cache-write-1h/capture-cache-write-1h.mts)", cacheWrite1hCaptureFile, err)
	}
	var capture struct {
		Rows []struct {
			Name  string       `json:"name"`
			Model string       `json:"model"`
			Rates ai.ModelCost `json:"rates"`
			SSE   string       `json:"sse"`
			Usage ai.Usage     `json:"usage"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("decode %s: %v", cacheWrite1hCaptureFile, err)
	}
	if len(capture.Rows) == 0 {
		t.Fatalf("%s has no rows", cacheWrite1hCaptureFile)
	}
	for _, row := range capture.Rows {
		t.Run(row.Name, func(t *testing.T) {
			provider, id, _ := strings.Cut(row.Model, "/")
			base := ai.GetModel(provider, id)
			if base == nil {
				t.Fatalf("catalog has no %s", row.Model)
			}
			model := *base
			model.Cost = row.Rates
			final := streamAnthropicSSE(t, &model, row.SSE)
			if final.StopReason != ai.StopStop {
				t.Fatalf("stop %s (%s)", final.StopReason, final.ErrorMessage)
			}
			if !reflect.DeepEqual(final.Usage, row.Usage) {
				t.Errorf("usage differs from pi:\ngot  %+v\nwant %+v", final.Usage, row.Usage)
			}
		})
	}
}
