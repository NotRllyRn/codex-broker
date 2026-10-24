package httpserver

import (
	"fmt"
	"math"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/broker"
)

type poolWindow struct {
	Label                       string
	Used, Remaining             float64
	Accounts, Unknown, Excluded int
	Available                   int
}

// Available accounts have measured short-window capacity and no known quota block.
func accountAvailable(account broker.AccountSummary) bool {
	return account.Enabled && account.AuthState == "VERIFIED" && !account.QuotaBlocked &&
		account.ShortPercent != nil && *account.ShortPercent >= 0 && *account.ShortPercent < 100 &&
		(account.WeeklyPercent == nil || *account.WeeklyPercent < 100)
}

type poolReset struct {
	Label string
	Hours float64
}

func (r *poolReset) Duration() string {
	minutes := int(math.Ceil(r.Hours * 60))
	return fmt.Sprintf("%dh %dm", minutes/60, minutes%60)
}

func nextAvailableReset(accounts []broker.AccountSummary, now time.Time) *poolReset {
	var next *poolReset
	var earliest int64
	for _, account := range accounts {
		if !account.Enabled || account.AuthState != "VERIFIED" {
			continue
		}
		for i, reset := range []*int64{account.ShortResetMS, account.WeeklyResetMS} {
			if reset == nil || *reset <= now.UnixMilli() || earliest != 0 && *reset >= earliest {
				continue
			}
			// A short reset cannot restore capacity while the weekly window is full.
			if i == 0 && account.WeeklyPercent != nil && *account.WeeklyPercent >= 100 {
				continue
			}
			earliest = *reset
			next = &poolReset{Label: []string{"5-hour", "Weekly"}[i], Hours: time.UnixMilli(*reset).Sub(now).Hours()}
		}
	}
	return next
}

// Pool percentages give each account one equal share of capacity; upstream
// percentages do not expose comparable token limits across subscription plans.
func poolUsage(accounts []broker.AccountSummary) []poolWindow {
	windows := []poolWindow{{Label: "5-hour window"}, {Label: "Weekly window"}}
	valid := func(value *int64) bool { return value != nil && *value >= 0 && *value <= 100 }
	for _, account := range accounts {
		for i, used := range []*int64{account.ShortPercent, account.WeeklyPercent} {
			window := &windows[i]
			if i == 0 && (!account.Enabled || account.AuthState != "VERIFIED") {
				window.Excluded++
				continue
			}
			// Exhausted accounts retain their share of the pool but cannot
			// supply short-window capacity, even when that reading is zero or missing.
			if i == 0 && (account.QuotaBlocked || account.WeeklyPercent != nil && *account.WeeklyPercent == 100) {
				window.Used += 100
				window.Accounts++
				continue
			}
			if !valid(used) {
				window.Unknown++
				continue
			}
			// Availability uses measured capacity, without counting exhausted shares.
			if i == 0 && accountAvailable(account) {
				window.Available++
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
