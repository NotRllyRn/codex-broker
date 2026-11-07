package proxyextension

import (
	"maps"
	"runtime/debug"

	"github.com/NotRllyRn/codex-broker/extensions/accountusage"
	"github.com/NotRllyRn/codex-broker/extensions/requestsize"
	"github.com/NotRllyRn/codex-broker/internal/responsesproxy"
)

const UsageID = accountusage.ID

func (s *Session) nativeCall(id, hook string, call func() error) (ok bool) {
	if s.nativeFailed[id] {
		return false
	}
	message := ""
	defer func() {
		if p := recover(); p != nil {
			message = "Native extension operation panicked"
			if text, ok := p.(string); ok {
				message += ": " + text
			}
			message += "\n" + string(debug.Stack())
		}
		if message != "" {
			if s.nativeFailed == nil {
				s.nativeFailed = map[string]bool{}
			}
			s.nativeFailed[id] = true
			s.writeLog(&instance{id: id, hook: hook}, "ERROR", message, nil, true)
			ok = false
		}
	}()
	if err := call(); err != nil {
		message = err.Error()
		return false
	}
	return true
}

// UsageNotice is native to preserve the default wire behavior without a VM.
func (s *Session) UsageNotice(notice responsesproxy.Notice) (text string) {
	s.nativeCall(UsageID, "response", func() error {
		values := s.native[UsageID]
		if show, ok := values["showAccountLabel"].(bool); ok && !show {
			notice.AccountID, notice.AccountLabel = "", "Usage stats"
		}
		if show, ok := values["showRetryStatus"].(bool); ok && !show {
			notice.Status = ""
		}
		text = accountusage.Notice(s.Usage, notice)
		return nil
	})
	return text
}

// Expected validation/size problems remain host errors; unexpected failures
// leave the prior payload intact. Hard forwarding limits remain in the gateway.
func (s *Session) Prepare(path string, body []byte, policy requestsize.ScrubPolicy, metrics requestsize.Metrics) ([]byte, *requestsize.Problem) {
	original := body
	s.nativeCall(UsageID, "request", func() error { body = accountusage.Prepare(path, original); return nil })
	original = body
	var result []byte
	var problem *requestsize.Problem
	if !s.nativeCall(requestsize.ID, "prepared", func() error {
		if policy.TriggerBytes == 0 {
			policy = requestsize.Policy(s.native[requestsize.ID])
		}
		result, problem = s.compaction.Prepare(path, original, policy, responsesproxy.MaxRequest, metrics)
		return nil
	}) {
		s.compaction = requestsize.New(false)
		return original, nil
	}
	return result, problem
}
func (s *Session) ClientEvent(next func(map[string]any) (map[string]any, error)) func(map[string]any) (map[string]any, error) {
	native := s.compaction.ClientEvent(nil)
	if native == nil {
		return next
	}
	return func(event map[string]any) (map[string]any, error) {
		if !s.nativeFailed[requestsize.ID] {
			// Handoff changes terminal response usage only. Copy those maps before it
			// runs so a failed callback cannot expose a half-applied transformation.
			candidate := maps.Clone(event)
			if response, ok := event["response"].(map[string]any); ok {
				response = maps.Clone(response)
				candidate["response"] = response
				if usage, ok := response["usage"].(map[string]any); ok {
					response["usage"] = maps.Clone(usage)
				}
			}
			var transformed map[string]any
			if s.nativeCall(requestsize.ID, "client_event", func() error { var err error; transformed, err = native(candidate); return err }) {
				event = transformed
			}
		}
		if next != nil {
			return next(event)
		}
		return event, nil
	}
}
