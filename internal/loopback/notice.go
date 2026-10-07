package loopback

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const noticePrefix = "msg_codex_broker_" // Remove notices from older adapter versions too.

func prepareNotice(body []byte) []byte {
	var request map[string]json.RawMessage
	if json.Unmarshal(body, &request) != nil {
		return body
	}
	var input []json.RawMessage
	if json.Unmarshal(request["input"], &input) != nil {
		return body
	}
	filtered := make([]json.RawMessage, 0, len(input))
	changed := false
	for _, raw := range input {
		var item map[string]any
		_ = json.Unmarshal(raw, &item)
		if item["role"] == "assistant" {
			id, _ := item["id"].(string)
			if strings.HasPrefix(id, noticePrefix) {
				changed = true
				continue
			}
			// Strip only our status line, retaining the model's actual answer.
			strip := func(text string) string {
				line, rest, ok := strings.Cut(text, "\n\n")
				if ok && strings.HasPrefix(line, "Codex Broker: ") && strings.Contains(line, " · short ") && strings.Contains(line, " · weekly ") {
					changed = true
					return rest
				}
				return text
			}
			if content, ok := item["content"].(string); ok {
				item["content"] = strip(content)
			}
			if content, ok := item["content"].([]any); ok {
				for _, part := range content {
					if part, ok := part.(map[string]any); ok {
						if text, ok := part["text"].(string); ok {
							part["text"] = strip(text)
						}
					}
				}
			}
			if changed {
				raw, _ = json.Marshal(item)
			}
		}
		filtered = append(filtered, raw)
	}
	if changed {
		request["input"], _ = json.Marshal(filtered)
		body, _ = json.Marshal(request)
	}
	return body
}

func accountNotice(selected lease) string {
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
	return fmt.Sprintf("Codex Broker: %s · short %s · weekly %s\n\n", label, percent(selected.ShortRemainingPercent), percent(selected.WeeklyRemainingPercent))
}

// Validate the stream before changing headers: the Codex backend can return
// SSE with text/plain, but an ordinary JSON response must pass through intact.
func (a *Adapter) copyNoticeResponse(w http.ResponseWriter, response *http.Response, selected lease) {
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	var prefix []byte
	for {
		frame, err := readNoticeFrame(reader)
		prefix = append(prefix, frame...)
		if len(prefix) > maxRequest {
			err = errors.New("response event exceeds limit")
		}
		if err != nil && err != io.EOF {
			a.logf("request notice result=failed stage=stream_validation")
			http.Error(w, "upstream stream validation failed", http.StatusBadGateway)
			return
		}
		event := noticeEvent(frame)
		kind, _ := event["type"].(string)
		if strings.HasPrefix(kind, "response.") {
			break
		}
		if err == io.EOF || len(bytes.TrimSpace(frame)) != 0 && !bytes.HasPrefix(bytes.TrimSpace(frame), []byte(":")) {
			a.logf("request notice result=skipped reason=not_sse")
			response.Body = io.NopCloser(io.MultiReader(bytes.NewReader(prefix), reader))
			copyResponse(w, response)
			return
		}
	}
	copyHeaders(w.Header(), response.Header)
	w.Header().Del("Set-Cookie")
	w.Header().Del("Content-Length")
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(response.StatusCode)
	writer := io.Writer(w)
	if flusher, ok := w.(http.Flusher); ok {
		writer = flushWriter{w, flusher}
	}
	inserted, err := streamNotice(writer, io.MultiReader(bytes.NewReader(prefix), reader), accountNotice(selected))
	if err != nil {
		a.logf("request notice result=failed stage=stream")
		panic(http.ErrAbortHandler) // Never replay a partially delivered response.
	}
	if inserted {
		a.logf("request notice result=injected")
	} else {
		a.logf("request notice result=skipped reason=no_final_answer")
	}
}

func readNoticeFrame(reader *bufio.Reader) ([]byte, error) {
	var frame []byte
	for {
		line, err := reader.ReadSlice('\n')
		frame = append(frame, line...)
		if len(frame) > maxRequest {
			return nil, errors.New("response event exceeds limit")
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err == io.EOF && len(frame) != 0 {
			return frame, nil
		}
		if err != nil || bytes.Equal(line, []byte("\n")) || bytes.Equal(line, []byte("\r\n")) {
			return frame, err
		}
	}
}

func noticeEvent(frame []byte) map[string]any {
	var data []string
	for _, line := range strings.Split(string(frame), "\n") {
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	var event map[string]any
	_ = json.Unmarshal([]byte(strings.Join(data, "\n")), &event)
	return event
}

// Preserve real message IDs and output indices. If phase is not available yet,
// hold that assistant message until output_item.done identifies its phase.
func streamNotice(w io.Writer, r io.Reader, prefix string) (bool, error) {
	reader := bufio.NewReader(r)
	final, started := map[string]bool{}, map[string]bool{}
	inserted := false
	var pending [][]byte
	pendingID, pendingBytes, sequence := "", 0, 0
	emit := func(frame []byte) error {
		event := noticeEvent(frame)
		if event["type"] == nil {
			_, err := w.Write(frame)
			return err
		}
		sequence++
		event["sequence_number"] = sequence
		id, _ := event["item_id"].(string)
		if item, ok := event["item"].(map[string]any); ok {
			id, _ = item["id"].(string)
		}
		prefixItem := func(item map[string]any) {
			if content, ok := item["content"].([]any); ok {
				for _, raw := range content {
					if part, ok := raw.(map[string]any); ok && part["type"] == "output_text" {
						if text, ok := part["text"].(string); ok && text != "" {
							part["text"] = prefix + text
							inserted = true
							break
						}
					}
				}
			}
		}
		if final[id] {
			if item, ok := event["item"].(map[string]any); ok {
				prefixItem(item)
				item["phase"] = "final_answer"
			}
			index, _ := event["content_index"].(float64)
			if index == 0 {
				if delta, ok := event["delta"].(string); ok && event["type"] == "response.output_text.delta" && !started[id] {
					event["delta"] = prefix + delta
					started[id] = true
					inserted = true
				}
				if text, ok := event["text"].(string); ok && event["type"] == "response.output_text.done" {
					event["text"] = prefix + text
				}
				if part, ok := event["part"].(map[string]any); ok && event["type"] == "response.content_part.done" && part["type"] == "output_text" {
					if text, ok := part["text"].(string); ok {
						part["text"] = prefix + text
					}
				}
			}
		}
		if response, ok := event["response"].(map[string]any); ok {
			if output, ok := response["output"].([]any); ok {
				for _, raw := range output {
					if item, ok := raw.(map[string]any); ok {
						if id, _ := item["id"].(string); final[id] {
							prefixItem(item)
						}
					}
				}
			}
		}
		data, err := json.Marshal(event)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], data)
		return err
	}
	for {
		frame, err := readNoticeFrame(reader)
		if err == io.EOF {
			break
		}
		if err != nil {
			return false, err
		}
		event := noticeEvent(frame)
		item, _ := event["item"].(map[string]any)
		id, _ := item["id"].(string)
		if pendingID == "" && event["type"] == "response.output_item.added" && item["type"] == "message" && item["role"] == "assistant" {
			if item["phase"] == "final_answer" {
				final[id] = true
			} else if item["phase"] != "commentary" {
				pendingID = id
			}
		}
		if pendingID != "" {
			pendingBytes += len(frame)
			if pendingBytes > maxRequest {
				return false, errors.New("assistant message exceeds limit")
			}
			pending = append(pending, frame)
			if event["type"] != "response.output_item.done" || id != pendingID {
				continue
			}
			final[id] = item["phase"] != "commentary"
			for _, buffered := range pending {
				if err := emit(buffered); err != nil {
					return false, err
				}
			}
			pending, pendingID, pendingBytes = nil, "", 0
			continue
		}
		if err := emit(frame); err != nil {
			return false, err
		}
	}
	if pendingID != "" {
		return false, errors.New("stream ended before assistant message completed")
	}
	return inserted, nil
}
