package delta

import (
	"encoding/json"
	"fmt"
	"testing"
)

// testdata/upstream_delta.json's "pending" section: scripts of pushes, takes
// and size reads that capture.mts ran against pi's own PendingOps — the
// upstream tests' sequences and cap cases, and a corpus of pi's tracker and
// diffRevisions batches taken at random points.

type goldenPending struct {
	Name  string `json:"name"`
	Steps []struct {
		Push     json.RawMessage `json:"push"`
		PushEach *struct {
			Gen   string `json:"gen"`
			Count int    `json:"count"`
		} `json:"pushEach"`
		Take bool `json:"take"`
		Size bool `json:"size"`
	} `json:"steps"`
	Results []struct {
		goldenBatch
		Replace bool `json:"replace"`
		Same    *int `json:"same"`
		Size    *int `json:"size"`
	} `json:"results"`
}

// pendingPush is a push step's batch: decoded ops, or distinctSets.
func pendingPush(t *testing.T, raw json.RawMessage) []Op {
	t.Helper()
	if raw[0] == '{' {
		var gen struct {
			Gen         string
			Count, From int
		}
		if err := json.Unmarshal(raw, &gen); err != nil || gen.Gen != "distinctSets" {
			t.Fatalf("push %s: not a known generator", raw)
		}
		return distinctSets(gen.Count, gen.From)
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatalf("push %s: %v", raw, err)
	}
	batch := make([]Op, len(items))
	for i, item := range items {
		op, err := UnmarshalOp(item)
		if err != nil {
			t.Fatalf("push %s: %v", raw, err)
		}
		batch[i] = op
	}
	return batch
}

// pendingBatches is capture.mts's: a pushEach step's one-commit batches.
func pendingBatches(gen string, count int) [][]Op {
	out := make([][]Op, count)
	for i := range out {
		if gen == "x" {
			out[i] = []Op{Append{Path: Path{Key("text")}, Text: "x"}}
		} else {
			out[i] = []Op{
				Append{Path: Path{Key("entries"), Index(0), Key("text")}, Text: "tok "},
				Set{Path: Path{Key("meta"), Key("tokens")}, Value: float64(i)},
			}
		}
	}
	return out
}

func TestGoldenPending(t *testing.T) {
	g := golden(t)
	for _, c := range g.Pending {
		if len(c.Steps) != len(c.Results) {
			t.Fatalf("%s: %d steps, %d results", c.Name, len(c.Steps), len(c.Results))
		}
		var queue PendingOps
		pushed := map[int][]Op{}
		for i, step := range c.Steps {
			where := fmt.Sprintf("%s step %d", c.Name, i)
			want := c.Results[i]
			switch {
			case step.PushEach != nil:
				for _, batch := range pendingBatches(step.PushEach.Gen, step.PushEach.Count) {
					queue.Push(batch)
				}
			case step.Push != nil:
				batch := pendingPush(t, step.Push)
				pushed[i] = batch
				queue.Push(batch)
			case step.Size:
				if want.Size == nil {
					t.Fatalf("%s: pi recorded no size", where)
				}
				if queue.Size() != *want.Size {
					t.Errorf("%s: Size() = %d, pi %d", where, queue.Size(), *want.Size)
				}
			case step.Take:
				got := queue.Take()
				if got.Replace != want.Replace {
					t.Errorf("%s: take replace = %v, pi %v", where, got.Replace, want.Replace)
					continue
				}
				if want.Replace {
					if got.Ops != nil {
						t.Errorf("%s: a replacement carries ops", where)
					}
					continue
				}
				checkBatch(t, where, got.Ops, want.goldenBatch)
				for j, batch := range pushed {
					if is := sameBatch(got.Ops, batch); is != (want.Same != nil && *want.Same == j) {
						t.Errorf("%s: take is push step %d's batch: %v; pi: same = %v", where, j, is, want.Same)
					}
				}
			default:
				t.Fatalf("%s: unknown step", where)
			}
		}
	}
}
