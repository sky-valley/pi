package ai

import (
	"encoding/json"
	"math"
	"os"
	"testing"
)

// Mirrors pi packages/ai/test/models-runtime.test.ts (upstream a9ecf301): a
// request-wide pricing tier applies only when total input usage strictly
// exceeds the tier threshold.
func TestCalculateCostAppliesInputTiers(t *testing.T) {
	model := &Model{Cost: ModelCost{
		Input: 5, Output: 30, CacheRead: 0.5, CacheWrite: 6.25,
		Tiers: []ModelCostTier{{
			InputTokensAbove: 272_000,
			Input:            10,
			Output:           45,
			CacheRead:        1,
			CacheWrite:       12.5,
		}},
	}}
	usage := func(cacheWrite int) *Usage {
		return &Usage{Input: 200_000, Output: 100_000, CacheRead: 72_000, CacheWrite: cacheWrite}
	}
	approx := func(name string, got, want float64) {
		if math.Abs(got-want) > 1e-12 {
			t.Fatalf("%s = %v, want %v", name, got, want)
		}
	}

	// inputTokens = 200000 + 72000 + 0 = 272000, NOT > 272000 -> base rates.
	short := CalculateCost(model, usage(0))
	approx("short.Input", short.Input, 1)
	approx("short.Output", short.Output, 3)
	approx("short.CacheRead", short.CacheRead, 0.036)
	approx("short.CacheWrite", short.CacheWrite, 0)

	// inputTokens = 272001 > 272000 -> tier rates.
	long := CalculateCost(model, usage(1))
	approx("long.Input", long.Input, 2)
	approx("long.Output", long.Output, 4.5)
	approx("long.CacheRead", long.CacheRead, 0.072)
	approx("long.CacheWrite", long.CacheWrite, 0.0000125)
}

// calculateCostCaptureFile is written by testdata/cost/capture.mjs from the
// published pi-ai build's calculateCost and catalog.
const calculateCostCaptureFile = "testdata/cost/calculate-cost-0.87.1.json"

// TestCalculateCostMatchesPi prices every captured usage at the captured rates
// and requires pi's exact doubles, not a tolerance: usage.cost is persisted in
// sessions and sent over the protocol, so the last bit is visible. The rows sit
// at and just above each catalog pricing tier with 1h cache writes, where a
// fused multiply-add (one rounding where V8 rounds twice) moves the result,
// plus rows for models without tiers (amazon.nova-2-lite-v1), the only ones
// that lock the cacheRead and short cache-write conversions. Each float64()
// conversion in CalculateCost has at least one row that fails when it is
// removed, so no row may be pruned as redundant. Only a target the compiler
// fuses on can show that (arm64; amd64 at GOAMD64=v3); baseline amd64 never
// fuses, so there this pins the rest.
func TestCalculateCostMatchesPi(t *testing.T) {
	data, err := os.ReadFile(calculateCostCaptureFile)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate it with testdata/cost/capture.mjs)", calculateCostCaptureFile, err)
	}
	var capture struct {
		Rows []struct {
			Model string        `json:"model"`
			Rates ModelCost     `json:"rates"`
			Usage Usage         `json:"usage"`
			Cost  CostBreakdown `json:"cost"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("decode %s: %v", calculateCostCaptureFile, err)
	}
	if len(capture.Rows) == 0 {
		t.Fatalf("%s has no rows", calculateCostCaptureFile)
	}
	for i, row := range capture.Rows {
		usage := row.Usage
		got := CalculateCost(&Model{Cost: row.Rates}, &usage)
		for _, f := range []struct {
			name      string
			got, want float64
		}{
			{"input", got.Input, row.Cost.Input},
			{"output", got.Output, row.Cost.Output},
			{"cacheRead", got.CacheRead, row.Cost.CacheRead},
			{"cacheWrite", got.CacheWrite, row.Cost.CacheWrite},
			{"total", got.Total, row.Cost.Total},
		} {
			if math.Float64bits(f.got) != math.Float64bits(f.want) {
				u := row.Usage
				t.Errorf("row %d %s (input %d, output %d, cacheRead %d, cacheWrite %d, cacheWrite1h %d): cost.%s = %v, pi %v",
					i, row.Model, u.Input, u.Output, u.CacheRead, u.CacheWrite, u.CacheWrite1h, f.name, f.got, f.want)
			}
		}
		if usage.Cost != got {
			t.Errorf("row %d %s: usage.Cost = %+v, want the returned %+v", i, row.Model, usage.Cost, got)
		}
	}
}

// upstream models-runtime.test.ts "marks catalog-derived totals as not
// provider-reported" (7c22a93d0): a total CalculateCost computes is the
// catalog estimate, whatever the usage carried before.
func TestCalculateCostMarksTheTotalAsCatalogDerived(t *testing.T) {
	for _, before := range []OptBool{OptBoolUnset, OptBoolFalse, OptBoolTrue} {
		usage := Usage{Input: 1_000_000, TotalTokens: 1_000_000,
			Cost: CostBreakdown{Total: 9, TotalFromProvider: before}}
		CalculateCost(&Model{Cost: ModelCost{Input: 2}}, &usage)
		if usage.Cost.TotalFromProvider != OptBoolFalse || usage.Cost.Total != 2 {
			t.Fatalf("from %v: cost = %+v, want total 2, totalFromProvider false", before, usage.Cost)
		}
	}
}

// totalFromProvider is pi's optional boolean: absent from the JSON until set,
// then written as itself, and read back the same way.
func TestCostTotalFromProviderJSON(t *testing.T) {
	for _, tc := range []struct {
		value OptBool
		json  string
	}{
		{OptBoolUnset, `{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":1}`},
		{OptBoolFalse, `{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":1,"totalFromProvider":false}`},
		{OptBoolTrue, `{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":1,"totalFromProvider":true}`},
	} {
		got, err := json.Marshal(CostBreakdown{Total: 1, TotalFromProvider: tc.value})
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != tc.json {
			t.Errorf("marshal %v = %s, want %s", tc.value, got, tc.json)
		}
		var back CostBreakdown
		if err := json.Unmarshal([]byte(tc.json), &back); err != nil {
			t.Fatal(err)
		}
		if back.TotalFromProvider != tc.value {
			t.Errorf("unmarshal %s = %v, want %v", tc.json, back.TotalFromProvider, tc.value)
		}
	}
	var back CostBreakdown
	if err := json.Unmarshal([]byte(`{"total":1,"totalFromProvider":null}`), &back); err != nil || back.TotalFromProvider != OptBoolUnset {
		t.Errorf("null reads as %v (%v), want unset", back.TotalFromProvider, err)
	}
	if err := json.Unmarshal([]byte(`{"total":1,"totalFromProvider":"yes"}`), &back); err == nil {
		t.Errorf("a string totalFromProvider decoded without error")
	}
}

// Or reads an unset property as its default and a set one as itself.
func TestOptBoolOr(t *testing.T) {
	for _, tc := range []struct {
		b         OptBool
		def, want bool
	}{
		{OptBoolUnset, false, false}, {OptBoolUnset, true, true},
		{OptBoolFalse, true, false}, {OptBoolFalse, false, false},
		{OptBoolTrue, false, true}, {OptBoolTrue, true, true},
	} {
		if got := tc.b.Or(tc.def); got != tc.want {
			t.Errorf("%v.Or(%v) = %v, want %v", tc.b, tc.def, got, tc.want)
		}
	}
}
