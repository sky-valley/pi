package providers

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// googlePromptBlockCaptureFile is what testdata/google-prompt-block/capture.mts
// recorded from pi's source at 117eac3a0 (#12330): how google-generative-ai
// ends a stream with no finish reason — a blocked prompt reports the block
// (formatGooglePromptBlockError: reason, message, safety ratings) and is not
// retryable; any other such stream names the chunks it received and stays
// retryable. Its first two rows are upstream's google-stream-errors.test.ts;
// the rest probe the JavaScript reading of mistyped feedback. Control: the
// script at the parent 69a699348 records "Google stream ended without a finish
// reason" (retryable) for every row but the one with a finish reason.
const googlePromptBlockCaptureFile = "testdata/google-prompt-block/google-prompt-block-117eac3a0.json"

func TestGooglePromptBlockVersusIncompleteStream(t *testing.T) {
	data, err := os.ReadFile(googlePromptBlockCaptureFile)
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Rows []struct {
			Name         string  `json:"name"`
			SSE          string  `json:"sse"`
			StopReason   string  `json:"stopReason"`
			ErrorMessage *string `json:"errorMessage"`
			Retryable    bool    `json:"retryable"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("%s: %v", googlePromptBlockCaptureFile, err)
	}
	if len(capture.Rows) == 0 {
		t.Fatalf("%s holds no rows; rerun capture.mts", googlePromptBlockCaptureFile)
	}
	for _, row := range capture.Rows {
		t.Run(row.Name, func(t *testing.T) {
			final := googleServe(t, "gemini-2.5-flash", row.SSE).Result()
			if string(final.StopReason) != row.StopReason {
				t.Fatalf("stop reason %s (%s), pi %s", final.StopReason, final.ErrorMessage, row.StopReason)
			}
			want := ""
			if row.ErrorMessage != nil {
				want = *row.ErrorMessage
			}
			if final.ErrorMessage != want {
				t.Fatalf("error message\n got %q\n  pi %q", final.ErrorMessage, want)
			}
			if got := ai.IsRetryableAssistantError(*final); got != row.Retryable {
				t.Fatalf("retryable = %v, pi %v", got, row.Retryable)
			}
		})
	}
}
