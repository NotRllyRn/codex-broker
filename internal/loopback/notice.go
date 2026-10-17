package loopback

import (
	"io"
	"net/http"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/responsesproxy"
)

const noticePrefix = responsesproxy.NoticePrefix

func notice(selected lease) responsesproxy.Notice {
	return responsesproxy.Notice{
		AccountID: selected.AccountID, AccountLabel: selected.AccountLabel,
		ShortRemainingPercent: selected.ShortRemainingPercent, WeeklyRemainingPercent: selected.WeeklyRemainingPercent,
		ShortResetsAt: selected.ShortResetsAt, WeeklyResetsAt: selected.WeeklyResetsAt,
	}
}

func prepareNotice(body []byte) []byte    { return responsesproxy.PrepareNotice(body) }
func accountNotice(selected lease) string { return responsesproxy.AccountNotice(notice(selected)) }
func accountNoticeAt(selected lease, now time.Time) string {
	return responsesproxy.AccountNoticeAt(notice(selected), now)
}
func streamNotice(w io.Writer, r io.Reader, prefix string) (bool, error) {
	return responsesproxy.StreamNotice(w, r, prefix)
}

func (a *Adapter) copyNoticeResponse(w http.ResponseWriter, response *http.Response, selected lease) {
	_, err := responsesproxy.CopyNoticeResponse(w, response, notice(selected), func(result responsesproxy.NoticeResult) {
		switch result {
		case responsesproxy.NoticeInjected:
			a.logf("request notice result=injected")
		case responsesproxy.NoticeNoFinalAnswer:
			a.logf("request notice result=skipped reason=no_final_answer")
		default:
			a.logf("request notice result=skipped reason=not_sse")
		}
	})
	if err != nil {
		a.logf("request notice result=failed stage=stream_validation")
		http.Error(w, "upstream stream validation failed", http.StatusBadGateway)
	}
}
