package proxyextension

import (
	"github.com/NotRllyRn/codex-broker/extensions/accountusage"
	"github.com/NotRllyRn/codex-broker/extensions/requestsize"
	"github.com/NotRllyRn/codex-broker/internal/responsesproxy"
)

const UsageID = accountusage.ID

// UsageNotice is the built-in account-usage presentation extension. Keep the
// existing formatter and stream reconciliation in Go to preserve wire behavior
// and avoid booting a VM for the default configuration. It has no authority over
// accounting. Replay cleanup and request-size compaction run independently.
func (s *Session) UsageNotice(notice responsesproxy.Notice) string {
	return accountusage.Notice(s.Usage, notice)
}

// Prepare runs native request extensions without copying large bodies into JS.
func (s *Session) Prepare(path string, body []byte, policy requestsize.ScrubPolicy, metrics requestsize.Metrics) ([]byte, *requestsize.Problem) {
	return s.compaction.Prepare(path, accountusage.Prepare(path, body), policy, responsesproxy.MaxRequest, metrics)
}
func (s *Session) ClientEvent(next func(map[string]any) (map[string]any, error)) func(map[string]any) (map[string]any, error) {
	return s.compaction.ClientEvent(next)
}
