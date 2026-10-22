package httpserver

import "github.com/NotRllyRn/codex-broker/internal/broker"

type poolWindow struct {
	Label                       string
	Used, Remaining             float64
	Accounts, Unknown, Excluded int
}

// Pool percentages give each account one equal share of capacity; upstream
// percentages do not expose comparable token limits across subscription plans.
func poolUsage(accounts []broker.AccountSummary) []poolWindow {
	windows := []poolWindow{{Label: "5-hour window"}, {Label: "Weekly window"}}
	valid := func(value *int64) bool { return value != nil && *value >= 0 && *value <= 100 }
	for _, account := range accounts {
		for i, used := range []*int64{account.ShortPercent, account.WeeklyPercent} {
			window := &windows[i]
			if i == 0 && (!account.Enabled || account.AuthState != "VERIFIED" || account.QuotaBlocked || account.ShortPercent != nil && *account.ShortPercent == 100 || account.WeeklyPercent != nil && *account.WeeklyPercent == 100) {
				window.Excluded++
				continue
			}
			if !valid(used) {
				window.Unknown++
				continue
			}
			window.Used += float64(*used)
			window.Accounts++
		}
	}
	for i := range windows {
		if windows[i].Accounts > 0 {
			windows[i].Used /= float64(windows[i].Accounts)
			windows[i].Remaining = 100 - windows[i].Used
		}
	}
	return windows
}
