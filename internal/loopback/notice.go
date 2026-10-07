package loopback

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const noticePrefix = "msg_codex_broker_"

// Remove our display-only messages from subsequent model inputs. Only a user
// message at the end of the input starts a turn; tool continuations do not.
func prepareNotice(body []byte) ([]byte, bool) {
	var request map[string]json.RawMessage
	if json.Unmarshal(body, &request) != nil {
		return body, false
	}
	var text string
	if json.Unmarshal(request["input"], &text) == nil && text != "" {
		return body, true
	}
	var input []json.RawMessage
	if json.Unmarshal(request["input"], &input) != nil {
		return body, false
	}
	filtered := make([]json.RawMessage, 0, len(input))
	changed, user := false, false
	for _, raw := range input {
		var item struct{ ID, Role, Type string }
		_ = json.Unmarshal(raw, &item)
		if item.Role == "assistant" && strings.HasPrefix(item.ID, noticePrefix) {
			changed = true
			continue
		}
		filtered = append(filtered, raw)
		user = item.Role == "user" && (item.Type == "" || item.Type == "message")
	}
	if changed {
		request["input"], _ = json.Marshal(filtered)
		body, _ = json.Marshal(request)
	}
	return body, user
}

func accountNotice(selected lease) map[string]any {
	label := selected.AccountLabel
	if label == "" {
		label = selected.AccountID
	}
	percent := func(value *int64) string {
		if value == nil {
			return "—"
		}
		return fmt.Sprintf("%d%%", *value)
	}
	return map[string]any{
		"id": noticePrefix + randomID(), "type": "message", "role": "assistant",
		"status": "completed", "channel": "commentary", "phase": "commentary",
		"content": []any{map[string]any{
			"type": "output_text", "annotations": []any{}, "logprobs": []any{},
			"text": fmt.Sprintf("Codex Broker: %s · short %s · weekly %s", label, percent(selected.ShortRemainingPercent), percent(selected.WeeklyRemainingPercent)),
		}},
	}
}

// Insert a normal assistant message after response.created and keep the
// subsequent event indices, sequence numbers and response output consistent.
func copyNoticeResponse(w http.ResponseWriter, response *http.Response, selected lease) {
	defer response.Body.Close()
	copyHeaders(w.Header(), response.Header)
	w.Header().Del("Set-Cookie")
	w.Header().Del("Content-Length")
	w.WriteHeader(response.StatusCode)
	writer := io.Writer(w)
	if flusher, ok := w.(http.Flusher); ok {
		writer = flushWriter{w, flusher}
	}
	if err := streamNotice(writer, response.Body, accountNotice(selected)); err != nil {
		// The stream is already committed; terminate it rather than replaying it.
		panic(http.ErrAbortHandler)
	}
}

func streamNotice(w io.Writer, r io.Reader, item map[string]any) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), maxRequest)
	var frame []string
	frameBytes := 0
	inserted, sequence := false, 0
	emit := func(event map[string]any) error {
		sequence++
		event["sequence_number"] = sequence
		data, err := json.Marshal(event)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], data)
		return err
	}
	process := func() error {
		var data []string
		for _, line := range frame {
			if strings.HasPrefix(line, "data:") {
				data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			}
		}
		var event map[string]any
		if json.Unmarshal([]byte(strings.Join(data, "\n")), &event) != nil || event["type"] == nil {
			_, err := io.WriteString(w, strings.Join(frame, "\n")+"\n\n")
			return err
		}
		if inserted {
			if index, ok := event["output_index"].(float64); ok {
				event["output_index"] = index + 1
			}
			if response, ok := event["response"].(map[string]any); ok {
				if output, ok := response["output"].([]any); ok {
					response["output"] = append([]any{item}, output...)
				}
			}
		}
		if err := emit(event); err != nil {
			return err
		}
		if event["type"] != "response.created" || inserted {
			return nil
		}
		inserted = true
		part := item["content"].([]any)[0].(map[string]any)
		for _, notice := range []map[string]any{
			{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"id": item["id"], "type": "message", "role": "assistant", "status": "in_progress", "channel": "commentary", "phase": "commentary", "content": []any{}}},
			{"type": "response.content_part.added", "output_index": 0, "item_id": item["id"], "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}, "logprobs": []any{}}},
			{"type": "response.output_text.delta", "output_index": 0, "item_id": item["id"], "content_index": 0, "delta": part["text"], "logprobs": []any{}},
			{"type": "response.output_text.done", "output_index": 0, "item_id": item["id"], "content_index": 0, "text": part["text"], "logprobs": []any{}},
			{"type": "response.content_part.done", "output_index": 0, "item_id": item["id"], "content_index": 0, "part": part},
			{"type": "response.output_item.done", "output_index": 0, "item": item},
		} {
			if err := emit(notice); err != nil {
				return err
			}
		}
		return nil
	}
	for scanner.Scan() {
		if scanner.Text() != "" {
			frameBytes += len(scanner.Text()) + 1
			if frameBytes > maxRequest {
				return fmt.Errorf("response event exceeds limit")
			}
			frame = append(frame, scanner.Text())
			continue
		}
		if len(frame) > 0 {
			if err := process(); err != nil {
				return err
			}
			frame = nil
			frameBytes = 0
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if len(frame) > 0 {
		return process()
	}
	return nil
}
