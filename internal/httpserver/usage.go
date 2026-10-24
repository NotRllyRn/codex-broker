package httpserver

import (
	"net/http"
	"strconv"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/core"
	"github.com/NotRllyRn/codex-broker/internal/usage"
)

func (s *Server) internalUsage(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireSession(r); err != nil {
		return err
	}
	days := 30
	if raw := r.URL.Query().Get("days"); raw == "all" {
		days = 0
	} else if raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || (parsed != 7 && parsed != 30 && parsed != 90) {
			return core.NewError("INVALID_USAGE_WINDOW", "Choose 7, 30, or 90 days, or all time", 400)
		}
		days = parsed
	}
	account := r.URL.Query().Get("account")
	if account == "" {
		account = "all"
	}
	var summary usage.Summary
	var err error
	if owner := r.URL.Query().Get("identity"); owner != "" {
		device := r.URL.Query().Get("device")
		if device == "" {
			device = "all"
		}
		summary, err = usage.ReadIdentitySummary(r.Context(), s.App.Store.DB, nil, owner, device, days, time.Now())
	} else {
		s.usageRatesOnce.Do(func() { s.usageRates = usage.NewCatalog(s.App.Store) })
		rates, pricing := s.usageRates.Read(r.Context())
		summary, err = usage.ReadSummary(r.Context(), s.App.Store.DB, rates, account, days, time.Now())
		summary.Pricing = pricing
	}
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, summary)
}
