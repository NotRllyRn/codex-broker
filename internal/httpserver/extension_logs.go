package httpserver

import (
	"context"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/logbook"
	"github.com/NotRllyRn/codex-broker/internal/proxyextension"
)

func (s *Server) recordExtensionLog(entry proxyextension.LogEntry) {
	if err := entry.Validate(); err != nil {
		return
	}
	entry.Message = logbook.Redact(entry.Message).(string)
	entry.Fields, _ = logbook.Redact(entry.Fields).(map[string]any)
	if s.App.Logs != nil {
		event := "proxy.extension.log"
		if entry.Failed {
			event = "proxy.extension.failed"
		}
		s.App.Logs.LogFields(entry.Level, event, entry.Message, map[string]any{
			"extension_id": entry.ExtensionID, "hook": entry.Hook, "request_id": entry.RequestID,
			"client_id": entry.ClientID, "thread_id": entry.ThreadID, "turn_id": entry.TurnID,
			"occurred_at_ms": entry.AtMS, "fields": entry.Fields, "relay_id": entry.RelayID,
		})
	}
	if entry.Level == "ERROR" {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := proxyextension.SaveLastError(ctx, s.App.Store, entry); err != nil && s.App.Logs != nil {
			s.App.Logs.Log("ERROR", "proxy.extension.diagnostics", "Latest extension error could not be saved")
		}
	}
}
