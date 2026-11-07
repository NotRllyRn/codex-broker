package proxyextension

import "github.com/NotRllyRn/codex-broker/internal/responsesproxy"

// UsageNotice is the built-in account-usage presentation extension. Keep the
// existing formatter and stream reconciliation in Go to preserve wire behavior
// and avoid booting a VM for the default configuration. It has no authority over
// accounting, replay cleanup, or native compaction.
func (s *Session) UsageNotice(notice responsesproxy.Notice) string {
	if !s.Usage {
		return ""
	}
	return responsesproxy.AccountNotice(notice)
}
