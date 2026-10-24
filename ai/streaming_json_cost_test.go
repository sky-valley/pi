package ai

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Upstream's "StreamingJsonParser cost" tests (streaming-json.test.ts at
// f715a15c1, #9265): a stream costs time and memory linear in its text. The
// byte bounds are upstream's, measured here as Go's allocated bytes.

// costContent is upstream's file content: 20,000 lines, each with a quoted
// string, a backslash and a newline, so its JSON text is dense with escapes.
var costContent = func() string {
	var b strings.Builder
	for line := 0; line < 20_000; line++ {
		fmt.Fprintf(&b, "const line%d = \"value %d\"; // \\ done\n", line, line)
	}
	return b.String()
}()

// writeArguments is `write` arguments with size characters of file content.
func writeArguments(size int) string {
	text, _ := json.Marshal(struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}{"/src/file.ts", costContent[:size]})
	return string(text)
}

// allocatedBytes is what run allocates.
func allocatedBytes(run func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	run()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

func streamAllocation(text string, delta int) uint64 {
	return allocatedBytes(func() {
		var args ToolCallArgumentsStream
		for i := 0; i < len(text); i += delta {
			args.Append(text[i:min(i+delta, len(text))])
		}
		args.Finish()
	})
}

func TestStreamingJSONAllocatesLinearlyInTheStreamedText(t *testing.T) {
	small, large := writeArguments(50_000), writeArguments(500_000)
	smallBytes, largeBytes := streamAllocation(small, 32), streamAllocation(large, 32)
	t.Logf("%.0f bytes per character; 500K/50K %.1fx", float64(largeBytes)/float64(len(large)), float64(largeBytes)/float64(smallBytes))
	if perChar := float64(largeBytes) / float64(len(large)); perChar >= 200 {
		t.Errorf("%.0f bytes per character, want < 200", perChar)
	}
	if ratio := float64(largeBytes) / float64(smallBytes); ratio >= 20 {
		t.Errorf("ten times the text allocates %.1fx, want < 20 (linear is about 10x)", ratio)
	}
}

// A number past float64's range must not make every later snapshot copy the
// whole tree (the tool-facing forms hold it as null): a closed member with
// many entries, then a long string streamed after it.
func TestStreamingJSONNonFiniteNumberStaysLinear(t *testing.T) {
	var done strings.Builder
	for i := 0; i < 2000; i++ {
		if i > 0 {
			done.WriteByte(',')
		}
		fmt.Fprintf(&done, `"k%d":%d`, i, i)
	}
	document := func(big string, size int) string {
		return `{"big":` + big + `,"done":{` + done.String() + `},` + writeArguments(size)[1:]
	}
	small, large := document("1e999", 50_000), document("1e999", 500_000)
	smallBytes, largeBytes := streamAllocation(small, 32), streamAllocation(large, 32)
	finiteBytes := streamAllocation(document("1e99", 500_000), 32)
	t.Logf("%.0f bytes per character (%.0f with a finite number); 500K/50K %.1fx",
		float64(largeBytes)/float64(len(large)), float64(finiteBytes)/float64(len(large)), float64(largeBytes)/float64(smallBytes))
	if ratio := float64(largeBytes) / float64(finiteBytes); ratio >= 1.5 {
		t.Errorf("the non-finite number makes the stream allocate %.1fx, want < 1.5", ratio)
	}
	if ratio := float64(largeBytes) / float64(smallBytes); ratio >= 20 {
		t.Errorf("ten times the text allocates %.1fx, want < 20 (linear is about 10x)", ratio)
	}
	var args ToolCallArgumentsStream
	got, order := args.Append(small)
	if v, ok := got["big"]; !ok || v != nil {
		t.Errorf(`partial "big" = %v, want null`, v)
	}
	if v, _ := order.Get("big"); v != nil {
		t.Errorf(`ordered partial "big" = %v, want null`, v)
	}
}

func TestStreamingJSONLargeOpenArrayLagsLinearly(t *testing.T) {
	elements := func(count int) string {
		items := make([]int, count)
		for i := range items {
			items[i] = i
		}
		text, _ := json.Marshal(map[string][]int{"items": items})
		return string(text)
	}
	small, large := elements(10_000), elements(100_000)
	smallBytes, largeBytes := streamAllocation(small, 16), streamAllocation(large, 16)
	t.Logf("%.0f bytes per character; 100K/10K %.1fx", float64(largeBytes)/float64(len(large)), float64(largeBytes)/float64(smallBytes))
	if perChar := float64(largeBytes) / float64(len(large)); perChar >= 2000 {
		t.Errorf("%.0f bytes per character, want < 2000", perChar)
	}
	if ratio := float64(largeBytes) / float64(smallBytes); ratio >= 20 {
		t.Errorf("ten times the text allocates %.1fx, want < 20", ratio)
	}
	var parser StreamingJSONParser
	shown := 0
	for i := 0; i < len(large); i += 16 {
		parser.Append(large[i:min(i+16, len(large))])
		snap, _ := parser.Snapshot()
		n := 0
		if obj, ok := snap.(OrderedObject); ok {
			if items, ok := obj.Get("items"); ok {
				n = len(items.([]any))
			}
		}
		if n < shown {
			t.Fatalf("the preview moved back from %d to %d items", shown, n)
		}
		shown = n
	}
	snap, _ := parser.Snapshot()
	items, _ := snap.(OrderedObject).Get("items")
	if len(items.([]any)) != 100_000 {
		t.Fatalf("final snapshot has %d items, want 100000", len(items.([]any)))
	}
}

func TestStreamingJSONDeepNestingIsLinear(t *testing.T) {
	const depth = 200_000
	text := strings.Repeat("[", depth) + "1" + strings.Repeat("]", depth)
	started := time.Now()
	var parser StreamingJSONParser
	for i := 0; i < len(text); i += 7 {
		parser.Append(text[i:min(i+7, len(text))])
		parser.Snapshot()
	}
	value, _ := parser.Snapshot()
	for level := 0; level < depth; level++ {
		value = value.([]any)[0]
	}
	if value != 1.0 {
		t.Fatalf("innermost value %v, want 1", value)
	}
	t.Logf("took %v", time.Since(started))
	if elapsed := time.Since(started); elapsed >= 5*time.Second && !raceEnabled {
		t.Errorf("took %v, want < 5s", elapsed)
	}
}

// Integer-like keys list first, ascending, however they arrive: descending
// keys must not cost a shift of every key read so far.
func TestStreamingJSONDescendingIndexKeysAreLinear(t *testing.T) {
	keys := func(count int) string {
		var b strings.Builder
		b.WriteByte('{')
		for i := count; i > 0; i-- {
			if i < count {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `"%d":0`, i)
		}
		return b.String() + "}"
	}
	elapsed := func(text string) time.Duration {
		started := time.Now()
		var parser StreamingJSONParser
		parser.Append(text)
		parser.Snapshot()
		return time.Since(started)
	}
	small, large := elapsed(keys(10_000)), elapsed(keys(100_000))
	t.Logf("10k keys %v, 100k keys %v", small, large)
	if ratio := float64(large) / float64(small); ratio >= 40 {
		t.Errorf("ten times the keys took %.1fx (%v vs %v), want < 40 (quadratic is about 100x)", ratio, large, small)
	}
	var parser StreamingJSONParser
	parser.Append(`{"3":0,"10":1,"b":2,"1":3,"a":4,"2":5}`)
	snap, _ := parser.Snapshot()
	var got []string
	for _, f := range snap.(OrderedObject) {
		got = append(got, f.Key)
	}
	if strings.Join(got, ",") != "1,2,3,10,b,a" {
		t.Errorf("keys %v, want 1,2,3,10,b,a", got)
	}
}

func TestStreamingJSONOneHugePieceIsLinear(t *testing.T) {
	text := writeArguments(500_000)
	bytes := allocatedBytes(func() {
		var parser StreamingJSONParser
		parser.Append(text)
		parser.Snapshot()
	})
	t.Logf("%.0f bytes per character", float64(bytes)/float64(len(text)))
	if perChar := float64(bytes) / float64(len(text)); perChar >= 100 {
		t.Errorf("%.0f bytes per character, want < 100", perChar)
	}
}
