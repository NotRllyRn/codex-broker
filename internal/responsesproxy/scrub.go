package responsesproxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	MaxInboundRequest = 128 << 20
	ScrubThreshold    = 50 << 20
	ScrubTarget       = 40 << 20
	toolOutputLimit   = 256 << 10
	toolOutputKeep    = 8 << 10
)

func IsCompactionRequest(path string, body []byte) bool {
	if path == "/v1/responses/compact" {
		return true
	}
	if path != "/v1/responses" {
		return false
	}
	var request struct {
		Input []itemHeader `json:"input"`
	}
	if json.Unmarshal(body, &request) != nil {
		return false
	}
	for _, item := range request.Input {
		if item.Type == "compaction_trigger" {
			return true
		}
	}
	return false
}

var (
	ErrInvalidScrubRequest = errors.New("invalid responses request")
	ErrScrubTarget         = errors.New("request remains too large after cleanup")
)

type ScrubPolicy struct {
	TriggerBytes int
	TargetBytes  int
}

var DefaultScrubPolicy = ScrubPolicy{TriggerBytes: ScrubThreshold, TargetBytes: ScrubTarget}

type ScrubResult struct {
	Body                    []byte
	BeforeBytes, AfterBytes int
	RemovedBytes            int
	Changed                 bool
}

type scrubItem struct {
	raw                    json.RawMessage
	typeName, role, callID string
}

type itemHeader struct {
	Type   string `json:"type"`
	Role   string `json:"role"`
	CallID string `json:"call_id"`
}

func Scrub(body []byte, policy ScrubPolicy) (ScrubResult, error) {
	result := ScrubResult{Body: body, BeforeBytes: len(body), AfterBytes: len(body)}
	if len(body) < policy.TriggerBytes {
		return result, nil
	}
	if policy.TargetBytes <= 0 || policy.TriggerBytes <= policy.TargetBytes {
		return result, fmt.Errorf("%w: invalid policy", ErrInvalidScrubRequest)
	}

	var request map[string]json.RawMessage
	if json.Unmarshal(body, &request) != nil {
		return result, ErrInvalidScrubRequest
	}
	var rawItems []json.RawMessage
	if input, ok := request["input"]; !ok || json.Unmarshal(input, &rawItems) != nil {
		return result, ErrScrubTarget
	}
	items := classify(rawItems)

	finish := func() (bool, error) {
		encoded, err := encodeRequest(request, items)
		if err != nil {
			return false, err
		}
		result.Body, result.AfterBytes = encoded, len(encoded)
		result.RemovedBytes = max(0, result.BeforeBytes-result.AfterBytes)
		result.Changed = true
		return result.AfterBytes <= policy.TargetBytes, nil
	}

	if compact := lastType(items, "compaction"); compact > 0 {
		protected := protectedItems(items)
		kept := make([]scrubItem, 0, len(items)-compact)
		for index, item := range items {
			if index >= compact || protected[index] {
				kept = append(kept, item)
			}
		}
		if len(kept) != len(items) {
			items = kept
			if done, err := finish(); done || err != nil {
				return result, err
			}
		}
	}

	protected := protectedItems(items)
	mediaChanged := false
	for index := range items {
		if protected[index] {
			continue
		}
		rewritten, changed := scrubMedia(items[index])
		if changed {
			items[index].raw, mediaChanged = rewritten, true
		}
	}
	if mediaChanged {
		if done, err := finish(); done || err != nil {
			return result, err
		}
	}

	outputChanged := false
	for index := range items {
		if protected[index] {
			continue
		}
		rewritten, changed := scrubToolOutput(items[index])
		if changed {
			items[index].raw, outputChanged = rewritten, true
		}
	}
	if outputChanged {
		if done, err := finish(); done || err != nil {
			return result, err
		}
	}

	for {
		start, end := oldestEvictableTurn(items)
		if start < 0 {
			break
		}
		items = append(items[:start:start], items[end:]...)
		if done, err := finish(); done || err != nil {
			return result, err
		}
	}
	return result, ErrScrubTarget
}

func classify(rawItems []json.RawMessage) []scrubItem {
	items := make([]scrubItem, len(rawItems))
	for index, raw := range rawItems {
		var header itemHeader
		_ = json.Unmarshal(raw, &header)
		items[index] = scrubItem{raw: raw, typeName: header.Type, role: header.Role, callID: header.CallID}
	}
	return items
}

func encodeRequest(request map[string]json.RawMessage, items []scrubItem) ([]byte, error) {
	raw := make([]json.RawMessage, len(items))
	for index := range items {
		raw[index] = items[index].raw
	}
	input, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: encode input", ErrInvalidScrubRequest)
	}
	request["input"] = input
	body, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("%w: encode request", ErrInvalidScrubRequest)
	}
	return body, nil
}

func lastType(items []scrubItem, typeName string) int {
	for index := len(items) - 1; index >= 0; index-- {
		if items[index].typeName == typeName {
			return index
		}
	}
	return -1
}

func protectedItems(items []scrubItem) []bool {
	protected := make([]bool, len(items))
	boundary := lastUser(items)
	if compact := lastType(items, "compaction"); compact >= 0 && (boundary < 0 || compact < boundary) {
		boundary = compact
	}
	if boundary < 0 {
		boundary = 0
	}
	for index, item := range items {
		protected[index] = index >= boundary || item.role == "system" || item.role == "developer"
	}
	for _, indices := range callIndices(items) {
		crosses := false
		for _, index := range indices {
			crosses = crosses || protected[index]
		}
		if crosses {
			for _, index := range indices {
				protected[index] = true
			}
		}
	}
	return protected
}

func lastUser(items []scrubItem) int {
	for index := len(items) - 1; index >= 0; index-- {
		if items[index].role == "user" {
			return index
		}
	}
	return -1
}

func callIndices(items []scrubItem) map[string][]int {
	result := map[string][]int{}
	for index, item := range items {
		if item.callID != "" {
			result[item.callID] = append(result[item.callID], index)
		}
	}
	return result
}

func scrubMedia(item scrubItem) (json.RawMessage, bool) {
	var object map[string]json.RawMessage
	if json.Unmarshal(item.raw, &object) != nil {
		return item.raw, false
	}
	var changed bool
	switch {
	case item.role == "user":
		object["content"], changed = scrubContentParts(object["content"])
	case item.typeName == "function_call_output" || item.typeName == "custom_tool_call_output":
		object["output"], changed = scrubContentParts(object["output"])
	case item.typeName == "computer_call_output":
		var output map[string]json.RawMessage
		var screenshotType, imageURL string
		if json.Unmarshal(object["output"], &output) == nil {
			_ = json.Unmarshal(output["type"], &screenshotType)
			_ = json.Unmarshal(output["image_url"], &imageURL)
		}
		if screenshotType == "computer_screenshot" && strings.HasPrefix(imageURL, "data:") {
			output["image_url"], _ = json.Marshal(onePixelPNG)
			delete(output, "file_id")
			object["output"], _ = json.Marshal(output)
			changed = true
		}
	}
	if !changed {
		return item.raw, false
	}
	rewritten, err := json.Marshal(object)
	return rewritten, err == nil
}

func scrubContentParts(raw json.RawMessage) (json.RawMessage, bool) {
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return raw, false
	}
	changed := false
	for index, part := range parts {
		var object map[string]json.RawMessage
		var typeName string
		if json.Unmarshal(part, &object) != nil || json.Unmarshal(object["type"], &typeName) != nil {
			continue
		}
		field, kind := "", ""
		switch typeName {
		case "input_image":
			field, kind = "image_url", "image"
		case "input_file":
			field, kind = "file_data", "file"
		}
		var value string
		if field == "" || json.Unmarshal(object[field], &value) != nil || value == "" || (field == "image_url" && !strings.HasPrefix(value, "data:")) {
			continue
		}
		parts[index], _ = json.Marshal(map[string]string{"type": "input_text", "text": fmt.Sprintf("[Historical %s omitted by Codex Broker: %d encoded bytes]", kind, len(value))})
		changed = true
	}
	if !changed {
		return raw, false
	}
	rewritten, err := json.Marshal(parts)
	return rewritten, err == nil
}

func scrubToolOutput(item scrubItem) (json.RawMessage, bool) {
	if item.typeName != "function_call_output" && item.typeName != "custom_tool_call_output" {
		return item.raw, false
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(item.raw, &object) != nil {
		return item.raw, false
	}
	var output string
	if json.Unmarshal(object["output"], &output) == nil {
		if len(output) <= toolOutputLimit {
			return item.raw, false
		}
		object["output"], _ = json.Marshal(truncateOutput(output))
	} else {
		var changed bool
		object["output"], changed = truncateTextParts(object["output"])
		if !changed {
			return item.raw, false
		}
	}
	rewritten, err := json.Marshal(object)
	return rewritten, err == nil
}

func truncateTextParts(raw json.RawMessage) (json.RawMessage, bool) {
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return raw, false
	}
	changed := false
	for index, part := range parts {
		var object map[string]json.RawMessage
		var typeName, value string
		if json.Unmarshal(part, &object) != nil || json.Unmarshal(object["type"], &typeName) != nil || typeName != "input_text" || json.Unmarshal(object["text"], &value) != nil || len(value) <= toolOutputLimit {
			continue
		}
		object["text"], _ = json.Marshal(truncateOutput(value))
		parts[index], _ = json.Marshal(object)
		changed = true
	}
	if !changed {
		return raw, false
	}
	rewritten, err := json.Marshal(parts)
	return rewritten, err == nil
}

func truncateOutput(value string) string {
	headEnd := min(toolOutputKeep, len(value))
	for headEnd > 0 && !utf8.ValidString(value[:headEnd]) {
		headEnd--
	}
	tailStart := max(headEnd, len(value)-toolOutputKeep)
	for tailStart < len(value) && !utf8.RuneStart(value[tailStart]) {
		tailStart++
	}
	omitted := max(0, len(value)-headEnd-(len(value)-tailStart))
	return fmt.Sprintf("%s\n[... %d bytes omitted by Codex Broker ...]\n%s", value[:headEnd], omitted, value[tailStart:])
}

func oldestEvictableTurn(items []scrubItem) (int, int) {
	protected, calls := protectedItems(items), callIndices(items)
	for start, item := range items {
		if item.role != "user" || protected[start] {
			continue
		}
		end := len(items)
		for index := start + 1; index < len(items); index++ {
			if items[index].role == "user" {
				end = index
				break
			}
		}
		if validTurn(items, protected, calls, start, end) {
			return start, end
		}
	}
	return -1, -1
}

func validTurn(items []scrubItem, protected []bool, calls map[string][]int, start, end int) bool {
	for index := start; index < end; index++ {
		if protected[index] || items[index].role == "system" || items[index].role == "developer" || items[index].typeName == "compaction" {
			return false
		}
		if callID := items[index].callID; callID != "" {
			indices, hasCall, hasOutput, known := calls[callID], false, false, true
			for _, related := range indices {
				if related < start || related >= end {
					return false
				}
				switch items[related].typeName {
				case "function_call", "custom_tool_call", "computer_call", "local_shell_call":
					hasCall = true
				case "function_call_output", "custom_tool_call_output", "computer_call_output", "local_shell_call_output":
					hasOutput = true
				default:
					known = false
				}
			}
			if !known || !hasCall || !hasOutput {
				return false
			}
		}
	}
	return true
}

const onePixelPNG = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="
