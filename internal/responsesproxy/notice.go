package responsesproxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const NoticePrefix = "msg_codex_broker_"

const NativeCompactionInputTokens int64 = 1_000_000

type StreamOptions struct {
	ArmNativeCompaction     bool
	OnNativeCompactionArmed func()
}

type Notice struct {
	AccountID, AccountLabel                       string
	ShortRemainingPercent, WeeklyRemainingPercent *int64
	ShortResetsAt, WeeklyResetsAt                 string
	Status                                        string
}

type NoticeResult string

const (
	NoticeInjected      NoticeResult = "injected"
	NoticeNotSSE        NoticeResult = "not_sse"
	NoticeNoFinalAnswer NoticeResult = "no_final_answer"
)

type TerminalResponse struct {
	ID, Model, ServiceTier, Status, FailureKind  string
	FirstEventAtMS, TerminalEventAtMS            int64
	InputTokens, CachedInputTokens, OutputTokens *int64
	ReasoningOutputTokens, TotalTokens           *int64
	UsageJSON                                    []byte
}

func PrepareNotice(body []byte) []byte {
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
			if strings.HasPrefix(id, NoticePrefix) {
				changed = true
				continue
			}
			strip := func(text string) string {
				line, rest, ok := strings.Cut(text, "\n\n")
				legacy := strings.HasPrefix(line, "Codex Broker: ") && strings.Contains(line, " · short ") && strings.Contains(line, " · weekly ")
				current := strings.HasPrefix(line, "🔨 ") && strings.Contains(line, " · 5h ") && strings.Contains(line, " · 7d ")
				if ok && (legacy || current) {
					changed = true
					return rest
				}
				return text
			}
			if content, ok := item["content"].(string); ok {
				item["content"] = strip(content)
			}
			if content, ok := item["content"].([]any); ok {
				for _, raw := range content {
					if part, ok := raw.(map[string]any); ok {
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

func AccountNotice(selected Notice) string { return AccountNoticeAt(selected, time.Now()) }

func AccountNoticeAt(selected Notice, now time.Time) string {
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
	reset := func(value string) string {
		at, err := time.Parse(time.RFC3339, value)
		if err != nil {
			return "—"
		}
		minutes := max(0, int((at.Sub(now)+time.Minute-1)/time.Minute))
		if minutes >= 24*60 {
			return fmt.Sprintf("%dd%dh", minutes/(24*60), minutes/60%24)
		}
		if minutes >= 60 {
			return fmt.Sprintf("%dh%dm", minutes/60, minutes%60)
		}
		return fmt.Sprintf("%dm", minutes)
	}
	status := ""
	if selected.Status != "" {
		status = " · " + selected.Status
	}
	return fmt.Sprintf("🔨 %s · 5h %s %s · 7d %s %s%s\n\n", label, percent(selected.ShortRemainingPercent), reset(selected.ShortResetsAt), percent(selected.WeeklyRemainingPercent), reset(selected.WeeklyResetsAt), status)
}

// CopyNoticeResponse validates an SSE response before changing its headers and
// prefixes only a final assistant answer. Non-SSE responses pass through.
func CopyNoticeResponse(w http.ResponseWriter, response *http.Response, notice Notice, report func(NoticeResult), observe func(TerminalResponse), options StreamOptions) (NoticeResult, error) {
	notify := func(result NoticeResult) NoticeResult {
		if report != nil {
			report(result)
		}
		return result
	}
	reader := bufio.NewReader(response.Body)
	var prefix []byte
	var firstEventAtMS int64
	for {
		frame, err := readNoticeFrame(reader)
		prefix = append(prefix, frame...)
		if len(prefix) > MaxRequest {
			err = errors.New("response event exceeds limit")
		}
		if err != nil && err != io.EOF {
			response.Body.Close()
			return "", err
		}
		event := noticeEvent(frame)
		kind, _ := event["type"].(string)
		if strings.HasPrefix(kind, "response.") || kind == "error" {
			firstEventAtMS = time.Now().UnixMilli()
			break
		}
		if err == io.EOF || len(bytes.TrimSpace(frame)) != 0 && !bytes.HasPrefix(bytes.TrimSpace(frame), []byte(":")) {
			response.Body = io.NopCloser(io.MultiReader(bytes.NewReader(prefix), reader))
			notify(NoticeNotSSE)
			CopyResponse(w, response)
			return NoticeNotSSE, nil
		}
	}
	defer response.Body.Close()
	CopyHeaders(w.Header(), response.Header)
	w.Header().Del("Content-Length")
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(response.StatusCode)
	controller := http.NewResponseController(w)
	_ = controller.Flush()
	inserted, err := streamNotice(flushWriter{w, controller}, io.MultiReader(bytes.NewReader(prefix), reader), AccountNotice(notice), firstEventAtMS, observe, options)
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	if inserted {
		return notify(NoticeInjected), nil
	}
	return notify(NoticeNoFinalAnswer), nil
}

func readNoticeFrame(reader *bufio.Reader) ([]byte, error) {
	var frame []byte
	for {
		line, err := reader.ReadSlice('\n')
		frame = append(frame, line...)
		if len(frame) > MaxRequest {
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

// StreamNotice preserves real message IDs and output indices. If phase is not
// available yet, it holds that assistant message until completion identifies it.
func StreamNotice(w io.Writer, r io.Reader, prefix string) (bool, error) {
	return streamNotice(w, r, prefix, 0, nil, StreamOptions{})
}

func streamNotice(w io.Writer, r io.Reader, prefix string, firstEventAtMS int64, observe func(TerminalResponse), options StreamOptions) (bool, error) {
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
			if event["type"] == "response.completed" && options.ArmNativeCompaction {
				usage, _ := response["usage"].(map[string]any)
				if usage == nil {
					usage = map[string]any{}
					response["usage"] = usage
				}
				outputTokens := int64(0)
				if value := tokenCount(usage["output_tokens"]); value != nil {
					outputTokens = *value
				}
				usage["input_tokens"] = NativeCompactionInputTokens
				usage["output_tokens"] = outputTokens
				usage["total_tokens"] = NativeCompactionInputTokens + outputTokens
				if options.OnNativeCompactionArmed != nil {
					options.OnNativeCompactionArmed()
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
		terminal := event["type"] == "response.completed" || event["type"] == "response.incomplete" || event["type"] == "response.failed" || event["type"] == "error"
		if terminal && observe != nil {
			// Account availability must be updated even if the downstream disconnects.
			observe(terminalResponse(event, firstEventAtMS, time.Now().UnixMilli()))
		}
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
			if pendingBytes > MaxRequest {
				return false, errors.New("assistant message exceeds limit")
			}
			pending = append(pending, frame)
			if terminal {
				for _, buffered := range pending {
					if err := emit(buffered); err != nil {
						return false, err
					}
				}
				return inserted, nil
			}
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
		if terminal {
			return inserted, nil
		}
	}
	if pendingID != "" {
		return false, errors.New("stream ended before assistant message completed")
	}
	return inserted, nil
}

func terminalResponse(event map[string]any, firstEventAtMS, terminalEventAtMS int64) TerminalResponse {
	status, _ := event["type"].(string)
	response, _ := event["response"].(map[string]any)
	result := TerminalResponse{Status: strings.TrimPrefix(status, "response."), FirstEventAtMS: firstEventAtMS, TerminalEventAtMS: terminalEventAtMS}
	if status == "error" {
		result.Status = "failed"
		failure := event["error"]
		if failure == nil {
			failure = event
		}
		result.FailureKind = terminalFailureKind(failure)
		return result
	}
	result.ID, _ = response["id"].(string)
	result.Model, _ = response["model"].(string)
	result.ServiceTier, _ = response["service_tier"].(string)
	if result.Status == "failed" {
		result.FailureKind = terminalFailureKind(response["error"])
	}
	usage, _ := response["usage"].(map[string]any)
	if usage == nil {
		return result
	}
	result.UsageJSON, _ = json.Marshal(usage)
	result.InputTokens = tokenCount(usage["input_tokens"])
	result.OutputTokens = tokenCount(usage["output_tokens"])
	result.TotalTokens = tokenCount(usage["total_tokens"])
	if details, ok := usage["input_tokens_details"].(map[string]any); ok {
		result.CachedInputTokens = tokenCount(details["cached_tokens"])
	}
	if details, ok := usage["output_tokens_details"].(map[string]any); ok {
		result.ReasoningOutputTokens = tokenCount(details["reasoning_tokens"])
	}
	return result
}

func terminalFailureKind(value any) string {
	encoded, _ := json.Marshal(value)
	message := strings.ToLower(string(encoded))
	for _, marker := range []string{"usage_limit", "usage limit", "quota", "rate_limit", "rate limit", "too many requests", "usage maxed out", "out of usage", "usage is maxed out", "usage has been exhausted"} {
		if strings.Contains(message, marker) {
			return "quota"
		}
	}
	return ""
}

func tokenCount(value any) *int64 {
	number, ok := value.(float64)
	if !ok || number < 0 || number != float64(int64(number)) {
		return nil
	}
	result := int64(number)
	return &result
}
