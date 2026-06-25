package ai

import "testing"

func TestIsRetryableAssistantError(t *testing.T) {
	cases := []struct {
		name string
		msg  AssistantMessage
		want bool
	}{
		{
			name: "non-error stop reason is not retryable",
			msg:  AssistantMessage{StopReason: StopStop, ErrorMessage: "overloaded"},
			want: false,
		},
		{
			name: "error stop reason with empty error message is not retryable",
			msg:  AssistantMessage{StopReason: StopError, ErrorMessage: ""},
			want: false,
		},
		{
			name: "insufficient_quota is non-retryable",
			msg:  AssistantMessage{StopReason: StopError, ErrorMessage: "insufficient_quota"},
			want: false,
		},
		{
			name: "monthly usage limit is non-retryable",
			msg:  AssistantMessage{StopReason: StopError, ErrorMessage: "Monthly usage limit reached"},
			want: false,
		},
		{
			name: "new #6019: you can retry your request",
			msg:  AssistantMessage{StopReason: StopError, ErrorMessage: "the model is busy; you can retry your request"},
			want: true,
		},
		{
			name: "new #6019: try your request again",
			msg:  AssistantMessage{StopReason: StopError, ErrorMessage: "please try your request again shortly"},
			want: true,
		},
		{
			name: "new #6019: please retry your request",
			msg:  AssistantMessage{StopReason: StopError, ErrorMessage: "transient failure, please retry your request"},
			want: true,
		},
		{
			name: "overloaded is retryable",
			msg:  AssistantMessage{StopReason: StopError, ErrorMessage: "Overloaded"},
			want: true,
		},
		{
			name: "429 is retryable",
			msg:  AssistantMessage{StopReason: StopError, ErrorMessage: "received HTTP 429 from provider"},
			want: true,
		},
		{
			name: "non-matching error message is not retryable",
			msg:  AssistantMessage{StopReason: StopError, ErrorMessage: "model refused to answer"},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRetryableAssistantError(tc.msg); got != tc.want {
				t.Errorf("IsRetryableAssistantError(%q) = %v, want %v", tc.msg.ErrorMessage, got, tc.want)
			}
		})
	}
}
