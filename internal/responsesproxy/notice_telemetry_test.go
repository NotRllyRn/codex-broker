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
			_, err := streamNotice(&output, strings.NewReader(stream), "", 10, func(value TerminalResponse) { observed = value }, StreamOptions{})
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

func TestStreamNoticeArmsNativeCompactionOnCompletedUsageOnly(t *testing.T) {
	for _, test := range []struct {
		status    string
		arm       bool
		wantIn    int64
		wantOut   int64
		wantTotal int64
	}{
		{status: "completed", arm: true, wantIn: NativeCompactionInputTokens, wantOut: 5, wantTotal: NativeCompactionInputTokens + 5},
		{status: "completed", arm: false, wantIn: 3, wantOut: 5, wantTotal: 8},
		{status: "incomplete", arm: true, wantIn: 3, wantOut: 5, wantTotal: 8},
		{status: "failed", arm: true, wantIn: 3, wantOut: 5, wantTotal: 8},
	} {
		t.Run(fmt.Sprintf("%s/arm=%t", test.status, test.arm), func(t *testing.T) {
			stream := "data: {\"type\":\"response.created\",\"response\":{}}\n\n" +
				fmt.Sprintf("data: {\"type\":\"response.%s\",\"response\":{\"future\":7,\"usage\":{\"input_tokens\":3,\"output_tokens\":5,\"total_tokens\":8,\"future_counter\":9}}}\n\n", test.status)
			var output bytes.Buffer
			var observed TerminalResponse
			armed := 0
			_, err := streamNotice(&output, strings.NewReader(stream), "", 10, func(value TerminalResponse) { observed = value }, StreamOptions{
				ArmNativeCompaction:     test.arm,
				OnNativeCompactionArmed: func() { armed++ },
			})
			if err != nil {
				t.Fatal(err)
			}
			events := strings.Count(output.String(), `"type":"response.`+test.status+`"`)
			terminal := noticeEvent([]byte(output.String()[strings.LastIndex(output.String(), "event: response."):]))
			response := terminal["response"].(map[string]any)
			usage := response["usage"].(map[string]any)
			if events != 1 || int64(usage["input_tokens"].(float64)) != test.wantIn || int64(usage["output_tokens"].(float64)) != test.wantOut || int64(usage["total_tokens"].(float64)) != test.wantTotal || response["future"] != float64(7) || usage["future_counter"] != float64(9) {
				t.Fatalf("events=%d response=%v", events, response)
			}
			wantArmed := 0
			if test.status == "completed" && test.arm {
				wantArmed = 1
			}
			if armed != wantArmed {
				t.Fatalf("armed=%d want=%d", armed, wantArmed)
			}
			if observed.InputTokens == nil || *observed.InputTokens != 3 || observed.TotalTokens == nil || *observed.TotalTokens != 8 {
				t.Fatalf("telemetry was rewritten: %+v", observed)
			}
		})
	}
}

func TestStreamNoticeSynthesizesUsageWhenArmed(t *testing.T) {
	stream := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp\"}}\n\n"
	var output bytes.Buffer
	_, err := streamNotice(&output, strings.NewReader(stream), "", 0, nil, StreamOptions{ArmNativeCompaction: true})
	if err != nil {
		t.Fatal(err)
	}
	event := noticeEvent(output.Bytes())
	usage := event["response"].(map[string]any)["usage"].(map[string]any)
	if usage["input_tokens"] != float64(NativeCompactionInputTokens) || usage["output_tokens"] != float64(0) || usage["total_tokens"] != float64(NativeCompactionInputTokens) {
		t.Fatalf("usage=%v", usage)
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
