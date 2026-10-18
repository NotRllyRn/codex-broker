package responsesproxy

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestStreamNoticeObservesTerminalUsage(t *testing.T) {
	for _, status := range []string{"completed", "incomplete", "failed"} {
		t.Run(status, func(t *testing.T) {
			stream := "data: {\"type\":\"response.created\",\"response\":{}}\n\n" +
				fmt.Sprintf("data: {\"type\":\"response.%s\",\"response\":{\"id\":\"resp\",\"usage\":{\"input_tokens\":3,\"input_tokens_details\":{\"cached_tokens\":2},\"output_tokens\":5,\"output_tokens_details\":{\"reasoning_tokens\":1},\"total_tokens\":8}}}\n\n", status)
			var output bytes.Buffer
			var observed TerminalResponse
			_, err := streamNotice(&output, strings.NewReader(stream), "", 10, func(value TerminalResponse) { observed = value })
			if err != nil {
				t.Fatal(err)
			}
			if observed.Status != status || observed.ID != "resp" || observed.FirstEventAtMS != 10 || observed.TerminalEventAtMS < 10 ||
				observed.InputTokens == nil || *observed.InputTokens != 3 || observed.CachedInputTokens == nil || *observed.CachedInputTokens != 2 ||
				observed.OutputTokens == nil || *observed.OutputTokens != 5 || observed.ReasoningOutputTokens == nil || *observed.ReasoningOutputTokens != 1 ||
				observed.TotalTokens == nil || *observed.TotalTokens != 8 {
				t.Fatalf("observation=%+v", observed)
			}
		})
	}
}

func TestTerminalResponseIgnoresInvalidUsageCounts(t *testing.T) {
	observed := terminalResponse(map[string]any{
		"type": "response.completed",
		"response": map[string]any{"usage": map[string]any{
			"input_tokens": -1.0, "output_tokens": 1.5, "total_tokens": "8",
		}},
	}, 1, 2)
	if observed.InputTokens != nil || observed.OutputTokens != nil || observed.TotalTokens != nil || len(observed.UsageJSON) == 0 {
		t.Fatalf("observation=%+v", observed)
	}
}

func TestTerminalResponseClassifiesOnlyAccountFailures(t *testing.T) {
	for _, test := range []struct {
		error any
		want  string
	}{
		{map[string]any{"code": "usage_limit_reached"}, "quota"},
		{map[string]any{"message": "Rate limit exceeded"}, "quota"},
		{map[string]any{"message": "Access token expired"}, ""},
		{map[string]any{"code": "server_error"}, ""},
		{nil, ""},
	} {
		observed := terminalResponse(map[string]any{"type": "response.failed", "response": map[string]any{"error": test.error}}, 1, 2)
		if observed.FailureKind != test.want {
			t.Fatalf("error=%v kind=%q want=%q", test.error, observed.FailureKind, test.want)
		}
	}
}
