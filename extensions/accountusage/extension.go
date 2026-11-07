// Package accountusage provides the built-in account/quota answer prefix.
package accountusage

import "github.com/NotRllyRn/codex-broker/internal/responsesproxy"

const ID = "account-usage"

func Notice(enabled bool, notice responsesproxy.Notice) string {
	if !enabled {
		return ""
	}
	return responsesproxy.AccountNotice(notice)
}

// Cleanup remains active while presentation is disabled, so old prefixes are
// never submitted back to the upstream model as ordinary conversation text.
func Prepare(path string, body []byte) []byte {
	if path == "/v1/responses" || path == "/v1/responses/compact" {
		return responsesproxy.PrepareNotice(body)
	}
	return body
}
