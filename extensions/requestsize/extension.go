// Package requestsize provides the built-in oversized Responses request workaround.
package requestsize

import "errors"

const ID = "request-size-compaction"
const NativeCompactionInputTokens int64 = 1_000_000

type Metrics interface {
	Scrub(int, int)
	RejectScrub()
	NativeCompactionArmed()
}

type Problem struct {
	Status        int
	Code, Message string
}
type Request struct {
	enabled, arm bool
	metrics      Metrics
}

func New(enabled bool) Request { return Request{enabled: enabled} }
func (p ScrubPolicy) Normalized() ScrubPolicy {
	if p.TriggerBytes == 0 {
		return DefaultScrubPolicy
	}
	return p
}

// Prepare owns cleanup policy, errors, metrics and the request-local handoff.
// The caller supplies its hard forwarding cap; disabling cleanup cannot bypass it.
func (r *Request) Prepare(path string, body []byte, policy ScrubPolicy, maxForward int, metrics Metrics) ([]byte, *Problem) {
	if !r.enabled || path != "/v1/responses" && path != "/v1/responses/compact" {
		return body, nil
	}
	r.metrics = metrics
	result, err := Scrub(body, policy.Normalized())
	if err != nil {
		if metrics != nil {
			metrics.RejectScrub()
		}
		if errors.Is(err, ErrInvalidScrubRequest) && len(body) <= maxForward {
			return nil, &Problem{400, "invalid_request", "Large Responses request is not valid JSON."}
		}
		return nil, &Problem{413, "request_too_large_after_scrub", "Request remains too large after historical content cleanup."}
	}
	r.arm = path == "/v1/responses" && result.Changed && !IsCompactionRequest(path, body)
	if result.Changed && metrics != nil {
		metrics.Scrub(result.BeforeBytes, result.AfterBytes)
	}
	if len(result.Body) > maxForward {
		if metrics != nil {
			metrics.RejectScrub()
		}
		return nil, &Problem{413, "request_too_large_after_scrub", "Request remains too large after historical content cleanup."}
	}
	return result.Body, nil
}

// ClientEvent runs after upstream observation and before user client_event hooks.
func (r *Request) ClientEvent(next func(map[string]any) (map[string]any, error)) func(map[string]any) (map[string]any, error) {
	if !r.arm {
		return next
	}
	var onArmed func()
	if r.metrics != nil {
		onArmed = r.metrics.NativeCompactionArmed
	}
	handoff := Handoff(onArmed)
	return func(event map[string]any) (map[string]any, error) {
		event, err := handoff(event)
		if err != nil || next == nil {
			return event, err
		}
		return next(event)
	}
}

// Handoff preserves the existing client-visible native-compaction signal. Real
// usage has already been captured by the host before this transformation runs.
func Handoff(onArmed func()) func(map[string]any) (map[string]any, error) {
	return func(event map[string]any) (map[string]any, error) {
		response, ok := event["response"].(map[string]any)
		if !ok || event["type"] != "response.completed" {
			return event, nil
		}
		usage, _ := response["usage"].(map[string]any)
		if usage == nil {
			usage = map[string]any{}
			response["usage"] = usage
		}
		outputTokens := int64(0)
		if number, ok := usage["output_tokens"].(float64); ok && number >= 0 && number == float64(int64(number)) {
			outputTokens = int64(number)
		}
		usage["input_tokens"] = NativeCompactionInputTokens
		usage["output_tokens"] = outputTokens
		usage["total_tokens"] = NativeCompactionInputTokens + outputTokens
		if onArmed != nil {
			onArmed()
		}
		return event, nil
	}
}
