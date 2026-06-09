package coding

import "github.com/sky-valley/pi/ai"

// Exact summary-wrapper text from pi (core/messages.ts). These must match
// byte-for-byte so reconstructed context is identical to pi's.
const (
	compactionSummaryPrefix = "The conversation history before this point was compacted into the following summary:\n\n<summary>\n"
	compactionSummarySuffix = "\n</summary>"
	branchSummaryPrefix     = "The following is a summary of a branch that this conversation came back from:\n\n<summary>\n"
	branchSummarySuffix     = "</summary>"
)

// compactionSummaryMessage builds the user message pi's convertToLlm produces for
// a compactionSummary entry/message.
func compactionSummaryMessage(summary string, timestamp int64) ai.UserMessage {
	return ai.UserMessage{
		Content:   ai.ContentList{ai.TextContent{Text: compactionSummaryPrefix + summary + compactionSummarySuffix}},
		Timestamp: timestamp,
	}
}

// branchSummaryMessage builds the user message for a branchSummary entry/message.
func branchSummaryMessage(summary string, timestamp int64) ai.UserMessage {
	return ai.UserMessage{
		Content:   ai.ContentList{ai.TextContent{Text: branchSummaryPrefix + summary + branchSummarySuffix}},
		Timestamp: timestamp,
	}
}
