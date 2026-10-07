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

const noticeTestStream = "data: {\"type\":\"response.created\",\"response\":{\"output\":[]},\"sequence_number\":0}\n\n" +
	"data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"msg_answer\",\"type\":\"message\"}}\n\n" +
	"data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"item_id\":\"msg_answer\",\"delta\":\"Hello\"}\n\n" +
	"data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"id\":\"msg_answer\"}],\"usage\":{\"total_tokens\":10}}}\n\n" +
	"data: [DONE]\n\n"

func TestNoticeStreamPreservesAnswerAndIndices(t *testing.T) {
	short, weekly := int64(82), int64(61)
	item := accountNotice(lease{AccountID: "a", AccountLabel: "Personal", ShortRemainingPercent: &short, WeeklyRemainingPercent: &weekly})
	var output bytes.Buffer
	if err := streamNotice(&output, strings.NewReader(noticeTestStream), item); err != nil {
		t.Fatal(err)
	}
	events := readNoticeEvents(t, output.String())
	if len(events) != 10 {
		t.Fatalf("events = %d", len(events))
	}
	for index, event := range events {
		if event["sequence_number"] != float64(index+1) {
			t.Fatalf("sequence = %#v", event)
		}
	}
	if events[3]["delta"] != "Codex Broker: Personal · short 82% · weekly 61%" {
		t.Fatalf("notice = %#v", events[3])
	}
	if events[7]["output_index"] != float64(1) || events[8]["delta"] != "Hello" || events[8]["item_id"] != "msg_answer" {
		t.Fatalf("answer events = %#v", events[7:9])
	}
	response := events[9]["response"].(map[string]any)
	items := response["output"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["id"] != item["id"] || items[1].(map[string]any)["id"] != "msg_answer" || response["usage"].(map[string]any)["total_tokens"] != float64(10) {
		t.Fatalf("completed = %#v", response)
	}
	if !strings.HasSuffix(output.String(), "data: [DONE]\n\n") {
		t.Fatal("lost stream terminator")
	}
}

func TestPrepareNoticeSkipsContinuationsAndRemovesPreviousNotice(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		notice      bool
	}{
		{"user", `{"input":[{"role":"user","content":"hi"}]}`, true},
		{"string", `{"input":"hi"}`, true},
		{"tool", `{"input":[{"role":"user"},{"type":"function_call_output","output":"done"}]}`, false},
		{"assistant", `{"input":[{"role":"user"},{"role":"assistant","content":"hi"}]}`, false},
		{"missing", `{}`, false},
		{"null", `{"input":null}`, false},
		{"malformed", `invalid`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, notice := prepareNotice([]byte(tc.input))
			if notice != tc.notice || string(body) != tc.input {
				t.Fatalf("prepared = %s, notice = %v", body, notice)
			}
		})
	}
	input := `{"model":"test","input":[{"role":"assistant","id":"msg_codex_broker_previous","content":[]},{"role":"user","content":"next"}]}`
	body, notice := prepareNotice([]byte(input))
	if !notice || strings.Contains(string(body), noticePrefix) || !strings.Contains(string(body), `"model":"test"`) {
		t.Fatalf("prepared = %s, notice = %v", body, notice)
	}
}

func TestAccountNoticeUsesPublicFallbackAndUnknownQuota(t *testing.T) {
	item := accountNotice(lease{AccountID: "public-a", AccessToken: "secret", ChatGPTAccountID: "private"})
	body, _ := json.Marshal(item)
	if !strings.Contains(string(body), "Codex Broker: public-a · short — · weekly —") || strings.Contains(string(body), "secret") || strings.Contains(string(body), "private") {
		t.Fatalf("notice = %s", body)
	}
}

func TestNoticeOnlyForSuccessfulUserResponse(t *testing.T) {
	for _, tc := range []struct {
		name, path, input string
		notice            bool
	}{
		{"user", "/v1/responses", `{"input":[{"role":"user","content":"hello"}]}`, true},
		{"tool", "/v1/responses", `{"input":[{"type":"function_call_output","output":"done"}]}`, false},
		{"compact", "/v1/responses/compact", `{"input":[{"role":"user","content":"hello"}]}`, false},
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
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Content-Length", fmt.Sprint(len(noticeTestStream)))
				_, _ = io.WriteString(w, noticeTestStream)
			}))
			defer upstream.Close()
			server := httptest.NewServer(newTestAdapter(t, broker.URL, ca, upstream).Handler())
			defer server.Close()
			request, _ := http.NewRequest("POST", server.URL+tc.path, strings.NewReader(tc.input))
			request.Header.Set("Authorization", "Bearer native")
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != 200 || routes.Load() != 2 || strings.Contains(string(body), "Codex Broker: Account b") != tc.notice || strings.Contains(string(body), "Codex Broker: Account a") {
				t.Fatalf("response = %d %s", response.StatusCode, body)
			}
			if !tc.notice && string(body) != noticeTestStream {
				t.Fatalf("changed continuation: %s", body)
			}
		})
	}
}

func TestNoticeStreamsBeforeUpstreamCompletes(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()
	done := make(chan error, 1)
	go func() {
		done <- streamNotice(w, strings.NewReader(noticeTestStream), accountNotice(lease{AccountID: "a"}))
	}()
	reader := strings.Builder{}
	buffer := make([]byte, 4096)
	for !strings.Contains(reader.String(), "response.output_item.done") {
		n, err := r.Read(buffer)
		if err != nil {
			t.Fatal(err)
		}
		reader.Write(buffer[:n])
	}
	if !strings.Contains(reader.String(), "Codex Broker: a") || strings.Contains(reader.String(), "Hello") {
		t.Fatalf("early stream = %s", reader.String())
	}
	go io.Copy(io.Discard, r)
	if err := <-done; err != nil {
		t.Fatal(err)
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
