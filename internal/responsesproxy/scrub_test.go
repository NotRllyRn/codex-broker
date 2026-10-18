package responsesproxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestScrubFastPathIsOpaque(t *testing.T) {
	body := []byte(" \n{\"input\":[],\"unknown\":true} \n")
	result, err := Scrub(body, ScrubPolicy{TriggerBytes: len(body) + 1, TargetBytes: len(body)})
	if err != nil || !bytes.Equal(result.Body, body) || result.Changed {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestIsCompactionRequest(t *testing.T) {
	for _, test := range []struct {
		path string
		body string
		want bool
	}{
		{"/v1/responses/compact", `{}`, true},
		{"/v1/responses", `{"input":[{"type":"compaction_trigger"}]}`, true},
		{"/v1/responses", `{"input":[{"type":"message","content":"compaction_trigger"}]}`, false},
		{"/v1/responses", `not-json`, false},
		{"/v1/chat/completions", `{"input":[{"type":"compaction_trigger"}]}`, false},
	} {
		if got := IsCompactionRequest(test.path, []byte(test.body)); got != test.want {
			t.Fatalf("path=%q body=%q got=%t want=%t", test.path, test.body, got, test.want)
		}
	}
}

func TestScrubHistoricalMediaProtectsCurrentTurn(t *testing.T) {
	oldImage := "data:image/png;base64," + strings.Repeat("a", 1200)
	currentImage := "data:image/png;base64," + strings.Repeat("b", 100)
	body := scrubRequest(t,
		map[string]any{"type": "message", "role": "user", "content": []any{
			map[string]any{"type": "input_image", "image_url": oldImage},
			map[string]any{"type": "input_image", "image_url": "https://example.com/kept.png"},
			map[string]any{"type": "input_file", "file_id": "file_kept"},
		}},
		map[string]any{"type": "reasoning", "encrypted_content": "old-secret"},
		map[string]any{"type": "function_call", "call_id": "old-call", "name": "inspect", "arguments": "{}"},
		map[string]any{"type": "function_call_output", "call_id": "old-call", "output": []any{map[string]any{"type": "input_file", "file_data": strings.Repeat("z", 1200), "filename": "old.pdf"}}},
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": currentImage}}},
		map[string]any{"type": "reasoning", "encrypted_content": "current-secret"},
	)
	result := mustScrub(t, body, 500)
	items := decodedItems(t, result.Body)
	if len(items) != 6 || stringValue(items[1]["encrypted_content"]) != "old-secret" || stringValue(items[5]["encrypted_content"]) != "current-secret" {
		t.Fatalf("reasoning or items changed: %#v", items)
	}
	oldContent := items[0]["content"].([]any)[0].(map[string]any)
	remoteContent := items[0]["content"].([]any)[1].(map[string]any)
	fileReference := items[0]["content"].([]any)[2].(map[string]any)
	fileContent := items[3]["output"].([]any)[0].(map[string]any)
	currentContent := items[4]["content"].([]any)[0].(map[string]any)
	if oldContent["type"] != "input_text" || remoteContent["image_url"] != "https://example.com/kept.png" || fileReference["file_id"] != "file_kept" || fileContent["type"] != "input_text" || currentContent["image_url"] != currentImage {
		t.Fatalf("media rewrite=%#v %#v %#v %#v %#v", oldContent, remoteContent, fileReference, fileContent, currentContent)
	}
	if items[2]["call_id"] != items[3]["call_id"] {
		t.Fatal("tool pair changed")
	}
}

func TestScrubTruncatesHistoricalToolOutput(t *testing.T) {
	output := strings.Repeat("head", toolOutputLimit/4) + "tail"
	body := scrubRequest(t,
		map[string]any{"type": "message", "role": "user", "content": "old"},
		map[string]any{"type": "function_call", "call_id": "call-1", "name": "shell", "arguments": "{}"},
		map[string]any{"type": "function_call_output", "call_id": "call-1", "output": output},
		map[string]any{"type": "message", "role": "user", "content": "current"},
	)
	result := mustScrub(t, body, 64<<10)
	items := decodedItems(t, result.Body)
	truncated := items[2]["output"].(string)
	if len(items) != 4 || !strings.Contains(truncated, "bytes omitted by Codex Broker") || !strings.HasSuffix(truncated, "tail") || items[1]["call_id"] != items[2]["call_id"] {
		t.Fatalf("items=%#v", items)
	}
}

func TestScrubEvictsCompleteToolTurn(t *testing.T) {
	body := scrubRequest(t,
		map[string]any{"type": "message", "role": "developer", "content": "always keep"},
		map[string]any{"type": "message", "role": "user", "content": strings.Repeat("x", 2000)},
		map[string]any{"type": "reasoning", "encrypted_content": "opaque"},
		map[string]any{"type": "function_call", "call_id": "call-1", "name": "shell", "arguments": "{}"},
		map[string]any{"type": "function_call_output", "call_id": "call-1", "output": "done"},
		map[string]any{"type": "message", "role": "assistant", "content": "complete"},
		map[string]any{"type": "message", "role": "user", "content": "current"},
	)
	result := mustScrub(t, body, 1000)
	items := decodedItems(t, result.Body)
	if len(items) != 2 || items[0]["role"] != "developer" || items[0]["content"] != "always keep" || items[1]["role"] != "user" || items[1]["content"] != "current" {
		t.Fatalf("items=%#v", items)
	}
}

func TestScrubRefusesCrossTurnToolPair(t *testing.T) {
	body := scrubRequest(t,
		map[string]any{"type": "message", "role": "user", "content": strings.Repeat("x", 2000)},
		map[string]any{"type": "function_call", "call_id": "call-1", "name": "shell", "arguments": "{}"},
		map[string]any{"type": "message", "role": "user", "content": "current"},
		map[string]any{"type": "function_call_output", "call_id": "call-1", "output": "done"},
	)
	_, err := Scrub(body, ScrubPolicy{TriggerBytes: len(body) - 1, TargetBytes: len(body) - 1000})
	if !errors.Is(err, ErrScrubTarget) {
		t.Fatalf("err=%v", err)
	}
}

func TestScrubComputerScreenshotUsesSchemaValidPlaceholder(t *testing.T) {
	body := scrubRequest(t,
		map[string]any{"type": "message", "role": "user", "content": "old"},
		map[string]any{"type": "computer_call", "call_id": "computer-1", "action": map[string]any{"type": "screenshot"}},
		map[string]any{"type": "computer_call_output", "call_id": "computer-1", "output": map[string]any{"type": "computer_screenshot", "image_url": "data:image/png;base64," + strings.Repeat("a", 1200), "file_id": "unused"}},
		map[string]any{"type": "message", "role": "user", "content": "current"},
	)
	result := mustScrub(t, body, 500)
	items := decodedItems(t, result.Body)
	output := items[2]["output"].(map[string]any)
	if output["type"] != "computer_screenshot" || output["image_url"] != onePixelPNG || output["file_id"] != nil || items[1]["call_id"] != items[2]["call_id"] {
		t.Fatalf("output=%#v", output)
	}
}

func TestScrubDropsOnlyPrefixBeforeLatestCompaction(t *testing.T) {
	body := scrubRequest(t,
		map[string]any{"type": "message", "role": "user", "content": strings.Repeat("x", 1200)},
		map[string]any{"type": "reasoning", "encrypted_content": "old"},
		map[string]any{"type": "compaction", "encrypted_content": "compact-secret"},
		map[string]any{"type": "message", "role": "user", "content": "current"},
	)
	result := mustScrub(t, body, 500)
	items := decodedItems(t, result.Body)
	if len(items) != 2 || items[0]["type"] != "compaction" || items[0]["encrypted_content"] != "compact-secret" || items[1]["content"] != "current" {
		t.Fatalf("items=%#v", items)
	}
}

func TestScrubFailsClosedWithoutUserBoundary(t *testing.T) {
	body := scrubRequest(t, map[string]any{"type": "function_call_output", "call_id": "call-1", "output": strings.Repeat("x", 2000)})
	_, err := Scrub(body, ScrubPolicy{TriggerBytes: len(body) - 1, TargetBytes: len(body) - 1000})
	if !errors.Is(err, ErrScrubTarget) {
		t.Fatalf("err=%v", err)
	}
}

func TestScrubRejectsMalformedLargeRequest(t *testing.T) {
	_, err := Scrub([]byte("not-json"), ScrubPolicy{TriggerBytes: 2, TargetBytes: 1})
	if !errors.Is(err, ErrInvalidScrubRequest) {
		t.Fatalf("err=%v", err)
	}
}

func TestScrubFailsClosedForStringInput(t *testing.T) {
	body := []byte(`{"model":"test","input":"` + strings.Repeat("x", 100) + `"}`)
	_, err := Scrub(body, ScrubPolicy{TriggerBytes: 2, TargetBytes: 1})
	if !errors.Is(err, ErrScrubTarget) {
		t.Fatalf("err=%v", err)
	}
}

func FuzzScrub(f *testing.F) {
	f.Add([]byte(`{"input":[]}`))
	f.Add([]byte(`{"input":[{"role":"user","content":"current"}]}`))
	f.Add([]byte("not-json"))
	f.Fuzz(func(t *testing.T, body []byte) {
		policy := ScrubPolicy{TriggerBytes: 64, TargetBytes: 32}
		first, firstErr := Scrub(body, policy)
		second, secondErr := Scrub(body, policy)
		if !bytes.Equal(first.Body, second.Body) || !sameError(firstErr, secondErr) {
			t.Fatal("scrub is not deterministic")
		}
		if firstErr == nil && first.Changed && (!json.Valid(first.Body) || first.AfterBytes > policy.TargetBytes) {
			t.Fatalf("invalid successful result: %+v", first)
		}
	})
}

func BenchmarkScrubLargeImage(b *testing.B) {
	body, err := json.Marshal(map[string]any{"model": "test", "input": []any{
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + strings.Repeat("a", 57<<20)}}},
		map[string]any{"role": "user", "content": "current"},
	}})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := Scrub(body, DefaultScrubPolicy); err != nil {
			b.Fatal(err)
		}
	}
}

func scrubRequest(t *testing.T, items ...any) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{"model": "test", "store": false, "input": items, "unknown": map[string]any{"preserved": true}})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func mustScrub(t *testing.T, body []byte, reduction int) ScrubResult {
	t.Helper()
	result, err := Scrub(body, ScrubPolicy{TriggerBytes: len(body) - 1, TargetBytes: len(body) - reduction})
	if err != nil {
		t.Fatalf("scrub: %v", err)
	}
	if !result.Changed || result.AfterBytes > len(body)-reduction || result.RemovedBytes != result.BeforeBytes-result.AfterBytes {
		t.Fatalf("result=%+v", result)
	}
	return result
}

func decodedItems(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var request struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatal(err)
	}
	return request.Input
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func sameError(left, right error) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.Error() == right.Error()
}
