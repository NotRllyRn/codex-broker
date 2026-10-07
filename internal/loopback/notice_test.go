package loopback

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const noticeTestStream = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"output\":[]}}\n\n" +
	"data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"msg_answer\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[]}}\n\n" +
	"data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"item_id\":\"msg_answer\",\"delta\":\"Hello\"}\n\n" +
	"data: {\"type\":\"response.output_text.done\",\"item_id\":\"msg_answer\",\"text\":\"Hello\"}\n\n" +
	"data: {\"type\":\"response.content_part.done\",\"item_id\":\"msg_answer\",\"part\":{\"type\":\"output_text\",\"text\":\"Hello\"}}\n\n" +
	"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"msg_answer\",\"type\":\"message\",\"role\":\"assistant\",\"phase\":\"final_answer\",\"content\":[{\"type\":\"output_text\",\"text\":\"Hello\"}]}}\n\n" +
	"data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"id\":\"msg_answer\",\"content\":[{\"type\":\"output_text\",\"text\":\"Hello\"}]}],\"usage\":{\"total_tokens\":10}}}\n\n" +
	"data: [DONE]\n\n"

func TestNoticePrefixesFinalAnswerConsistently(t *testing.T) {
	short, weekly := int64(82), int64(61)
	prefix := accountNotice(lease{AccountID: "a", AccountLabel: "Personal", ShortRemainingPercent: &short, WeeklyRemainingPercent: &weekly})
	for _, phase := range []string{"late", "early", "legacy"} {
		t.Run(phase, func(t *testing.T) {
			stream := noticeTestStream
			if phase == "early" {
				stream = strings.Replace(stream, `"content":[]`, `"phase":"final_answer","content":[]`, 1)
			}
			if phase == "legacy" {
				stream = strings.ReplaceAll(stream, `"phase":"final_answer",`, "")
			}
			var output bytes.Buffer
			inserted, err := streamNotice(&output, strings.NewReader(stream), prefix)
			if err != nil || !inserted {
				t.Fatalf("injected = %v, error = %v", inserted, err)
			}
			events := readNoticeEvents(t, output.String())
			if len(events) != 7 {
				t.Fatalf("events = %d", len(events))
			}
			for index, event := range events {
				if event["sequence_number"] != float64(index+1) {
					t.Fatalf("sequence = %#v", event)
				}
				if value, ok := event["output_index"]; ok && value != float64(0) {
					t.Fatalf("changed index: %#v", event)
				}
			}
			if events[2]["delta"] != prefix+"Hello" || events[3]["text"] != prefix+"Hello" || events[4]["part"].(map[string]any)["text"] != prefix+"Hello" {
				t.Fatalf("text events = %#v", events[2:5])
			}
			item := events[5]["item"].(map[string]any)
			if item["id"] != "msg_answer" || item["phase"] != "final_answer" || item["content"].([]any)[0].(map[string]any)["text"] != prefix+"Hello" {
				t.Fatalf("item = %#v", item)
			}
			response := events[6]["response"].(map[string]any)
			items := response["output"].([]any)
			if len(items) != 1 || items[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] != prefix+"Hello" || response["usage"].(map[string]any)["total_tokens"] != float64(10) {
				t.Fatalf("completed = %#v", response)
			}
			if !strings.HasSuffix(output.String(), "data: [DONE]\n\n") {
				t.Fatal("lost terminator")
			}
		})
	}
}

func TestCommentaryNeverGetsNotice(t *testing.T) {
	commentary := strings.ReplaceAll(strings.ReplaceAll(noticeTestStream, "msg_answer", "msg_progress"), `"phase":"final_answer"`, `"phase":"commentary"`)
	var output bytes.Buffer
	inserted, err := streamNotice(&output, strings.NewReader(commentary), "STATUS\n\n")
	if err != nil || inserted || strings.Contains(output.String(), "STATUS") {
		t.Fatalf("injected = %v, error = %v", inserted, err)
	}
	output.Reset()
	commentary = strings.Split(commentary, `data: {"type":"response.completed"`)[0]
	inserted, err = streamNotice(&output, strings.NewReader(commentary+noticeTestStream), "STATUS\n\n")
	if err != nil || !inserted {
		t.Fatalf("injected = %v, error = %v", inserted, err)
	}
	var deltas []string
	for _, event := range readNoticeEvents(t, output.String()) {
		if text, ok := event["delta"].(string); ok {
			deltas = append(deltas, text)
		}
	}
	if len(deltas) != 2 || deltas[0] != "Hello" || deltas[1] != "STATUS\n\nHello" {
		t.Fatalf("deltas = %#v", deltas)
	}
}

func TestPrepareNoticePreservesAnswersAndToolContinuations(t *testing.T) {
	for _, input := range []string{
		`{"input":[{"role":"user","content":"hi"}]}`,
		`{"input":"hi"}`,
		`{"input":[{"role":"user"},{"type":"function_call_output","output":"done"}]}`,
		`{"input":[{"type":"function_call_output","output":"done"}]}`,
		`{"input":[{"role":"assistant","content":"hi"}]}`,
		`{}`, `{"input":null}`, `invalid`,
	} {
		body := prepareNotice([]byte(input))
		if string(body) != input {
			t.Fatalf("prepared = %s", body)
		}
	}
	for _, content := range []any{accountNotice(lease{AccountID: "a"}) + "Actual answer", []any{map[string]any{"type": "output_text", "text": accountNotice(lease{AccountID: "a"}) + "Actual answer"}}} {
		input, _ := json.Marshal(map[string]any{"model": "test", "input": []any{
			map[string]any{"role": "assistant", "id": noticePrefix + "old", "content": []any{}},
			map[string]any{"role": "assistant", "id": "msg_answer", "content": content},
			map[string]any{"role": "user", "content": "next"},
		}})
		body := prepareNotice(input)
		if strings.Contains(string(body), "Codex Broker:") || strings.Contains(string(body), noticePrefix) || !strings.Contains(string(body), "Actual answer") || !strings.Contains(string(body), `"model":"test"`) {
			t.Fatalf("prepared = %s", body)
		}
	}
}

func TestAccountNoticeUsesPublicFallback(t *testing.T) {
	text := accountNotice(lease{AccountID: "public-a", AccessToken: "secret", ChatGPTAccountID: "private"})
	if text != "Codex Broker: public-a · short — · weekly —\n\n" {
		t.Fatalf("notice = %s", text)
	}
}

func TestPlainTextSSEAfterFailover(t *testing.T) {
	for _, tc := range []struct {
		name, path, response, contentType string
		stream, notice                    bool
	}{
		{"plain_sse", "/v1/responses", noticeTestStream, "text/plain; charset=utf-8", true, true},
		{"implicit_stream", "/v1/responses", noticeTestStream, "text/plain; charset=utf-8", false, true},
		{"alternate_input", "/v1/responses", noticeTestStream, "text/plain; charset=utf-8", true, true},
		{"sse", "/v1/responses", noticeTestStream, "text/event-stream", true, true},
		{"json", "/v1/responses", `{"output":[]}`, "application/json", true, false},
		{"non_streaming", "/v1/responses", `{"output":[]}`, "application/json", false, false},
		{"compact", "/v1/responses/compact", noticeTestStream, "text/plain", true, false},
		{"tool_continuation", "/v1/responses", `data: {"type":"response.created","response":{}}` + "\n\n" + `data: {"type":"response.output_item.done","item":{"id":"tool","type":"function_call","name":"shell","arguments":"{}"}}` + "\n\n" + `data: {"type":"response.completed","response":{"output":[]}}` + "\n\n", "text/plain", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var routes atomic.Int32
			broker, ca := testBroker(t, func(w http.ResponseWriter, _ *http.Request) {
				id := "a"
				if routes.Add(1) > 1 {
					id = "b"
				}
				writeJSON(w, 200, lease{Status: "ok", AccountID: id, AccountLabel: "Account " + id, AccessToken: "access-" + id, ChatGPTAccountID: "upstream-" + id})
			})
			defer broker.Close()
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") == "Bearer access-a" {
					writeJSON(w, 429, map[string]string{"error": "quota"})
					return
				}
				w.Header().Set("Content-Type", tc.contentType)
				w.Header().Set("Content-Length", fmt.Sprint(len(tc.response)))
				_, _ = io.WriteString(w, tc.response)
			}))
			defer upstream.Close()
			adapter := newTestAdapter(t, broker.URL, ca, upstream)
			var logs bytes.Buffer
			adapter.logf = func(format string, args ...any) { fmt.Fprintf(&logs, format+"\n", args...) }
			body := fmt.Sprintf(`{"stream":%t,"input":[{"role":"user","content":"hello"}]}`, tc.stream)
			if tc.name == "implicit_stream" {
				body = `{"input":[{"role":"user","content":"hello"}]}`
			}
			if tc.name == "alternate_input" {
				body = `{"input":[{"type":"agent_message","author":"user","recipient":"assistant","content":[{"type":"input_text","text":"hello"}]}]}`
			}
			request := httptest.NewRequest("POST", tc.path, strings.NewReader(body))
			request.Header.Set("Authorization", "Bearer native")
			response := httptest.NewRecorder()
			adapter.Handler().ServeHTTP(response, request)
			if response.Code != 200 || routes.Load() != 2 || strings.Contains(response.Body.String(), "Codex Broker: Account b") != tc.notice || strings.Contains(response.Body.String(), "Codex Broker: Account a") {
				t.Fatalf("response = %d %s", response.Code, response.Body.String())
			}
			if tc.notice && (response.Header().Get("Content-Type") != "text/event-stream" || response.Header().Get("Content-Length") != "" || !strings.Contains(logs.String(), "result=injected")) {
				t.Fatalf("headers = %#v, logs = %s", response.Header(), logs.String())
			}
			if !tc.notice && tc.name != "tool_continuation" && response.Body.String() != tc.response {
				t.Fatalf("changed response = %s", response.Body.String())
			}
			for _, secret := range []string{"access-", "upstream-", "Account b", "native", "cbk_"} {
				if strings.Contains(logs.String(), secret) {
					t.Fatalf("log contains %q", secret)
				}
			}
		})
	}
}

func TestKnownFinalPhaseStreamsImmediately(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()
	done := make(chan error, 1)
	stream := strings.Replace(noticeTestStream, `"content":[]`, `"phase":"final_answer","content":[]`, 1)
	go func() { _, err := streamNotice(w, strings.NewReader(stream), "STATUS\n\n"); done <- err }()
	var output strings.Builder
	buffer := make([]byte, 4096)
	for !strings.Contains(output.String(), "STATUS") {
		n, err := r.Read(buffer)
		if err != nil {
			t.Fatal(err)
		}
		output.Write(buffer[:n])
	}
	if strings.Contains(output.String(), "response.completed") {
		t.Fatal("buffered completed response")
	}
	go io.Copy(io.Discard, r)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestTruncatedMessageFails(t *testing.T) {
	stream := strings.Split(noticeTestStream, `data: {"type":"response.output_item.done"`)[0]
	var output bytes.Buffer
	_, err := streamNotice(&output, strings.NewReader(stream), "STATUS\n\n")
	if err == nil {
		t.Fatal("accepted incomplete message")
	}
}

func readNoticeEvents(t *testing.T, stream string) []map[string]any {
	t.Helper()
	var events []map[string]any
	for _, line := range strings.Split(stream, "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	return events
}
