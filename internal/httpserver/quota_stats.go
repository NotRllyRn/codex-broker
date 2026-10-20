package httpserver

import (
	"context"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/proxytelemetry"
)

type quotaWindowUsage struct {
	Samples     int64    `json:"samples"`
	UsedPercent int64    `json:"used_percent"`
	ActiveMS    int64    `json:"active_ms"`
	PerHour     *float64 `json:"usage_per_hour"`
}
type quotaPeriod struct {
	Short  quotaWindowUsage `json:"short"`
	Weekly quotaWindowUsage `json:"weekly"`
}
type quotaAccountUsage struct {
	AccountID    string      `json:"account_id"`
	AccountLabel string      `json:"account_label"`
	Rolling      quotaPeriod `json:"rolling_7d"`
	Lifetime     quotaPeriod `json:"lifetime"`
}

func (s *Server) quotaUsage(ctx context.Context, now time.Time) ([]*quotaAccountUsage, error) {
	rows, err := proxytelemetry.ReadQuotaTotals(ctx, s.App.Store.DB, now.Add(-proxytelemetry.QuotaRetention).UnixMilli())
	if err != nil {
		return nil, err
	}
	all := &quotaAccountUsage{AccountID: "all", AccountLabel: "All accounts"}
	result := []*quotaAccountUsage{all}
	byAccount := map[string]*quotaAccountUsage{}
	for _, row := range rows {
		item := byAccount[row.AccountID]
		if item == nil {
			item = &quotaAccountUsage{AccountID: row.AccountID, AccountLabel: row.AccountLabel}
			byAccount[row.AccountID] = item
			result = append(result, item)
		}
		for _, account := range []*quotaAccountUsage{item, all} {
			period := &account.Rolling
			if row.Period == proxytelemetry.LifetimePeriod {
				period = &account.Lifetime
			}
			window := &period.Short
			if row.Window == "weekly" {
				window = &period.Weekly
			}
			window.Samples += row.Samples
			window.UsedPercent += row.UsedPercent
			window.ActiveMS += row.ActiveMS
		}
	}
	for _, account := range result {
		for _, window := range []*quotaWindowUsage{&account.Rolling.Short, &account.Rolling.Weekly, &account.Lifetime.Short, &account.Lifetime.Weekly} {
			if window.ActiveMS > 0 {
				rate := float64(window.UsedPercent) * float64(time.Hour/time.Millisecond) / float64(window.ActiveMS)
				window.PerHour = &rate
			}
		}
	}
	return result, nil
}
