// Package usage prices recorded proxy tokens at API-equivalent list rates.
package usage

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/store"
)

const RatesURL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"

type TokenRates struct {
	Input, Cached, Output float64
}

type ModelRate struct {
	Standard        TokenRates
	Fast, Ultrafast *TokenRates
}

type RateTable map[string]ModelRate

func number(entry map[string]json.RawMessage, key string) (float64, bool) {
	var n float64
	err := json.Unmarshal(entry[key], &n)
	return n, err == nil && string(entry[key]) != "null" && n >= 0 && !math.IsNaN(n) && !math.IsInf(n, 0)
}

func readRates(entry map[string]json.RawMessage, suffix string, standard *TokenRates) *TokenRates {
	input, hasInput := number(entry, "input_cost_per_token"+suffix)
	output, hasOutput := number(entry, "output_cost_per_token"+suffix)
	if !hasInput || !hasOutput {
		return nil
	}
	cached, ok := number(entry, "cache_read_input_token_cost"+suffix)
	if !ok {
		cached = input
		if standard != nil && standard.Input > 0 {
			cached = input * standard.Cached / standard.Input
		}
	}
	return &TokenRates{Input: input, Cached: cached, Output: output}
}

func ParseRates(document []byte) RateTable {
	var entries map[string]map[string]json.RawMessage
	if json.Unmarshal(document, &entries) != nil {
		return RateTable{}
	}
	rates := RateTable{}
	for name, entry := range entries {
		standard := readRates(entry, "", nil)
		if standard == nil {
			continue
		}
		fast := readRates(entry, "_priority", standard)
		var specific map[string]json.RawMessage
		if json.Unmarshal(entry["provider_specific_entry"], &specific) == nil {
			if multiple, ok := number(specific, "fast"); ok && multiple > 0 {
				fast = &TokenRates{standard.Input * multiple, standard.Cached * multiple, standard.Output * multiple}
			}
		}
		rates[strings.ToLower(strings.TrimSpace(name))] = ModelRate{*standard, fast, readRates(entry, "_ultrafast", standard)}
	}
	return rates
}

func (rates RateTable) Lookup(model, tier string) (TokenRates, TokenRates, bool) {
	rate, ok := rates[strings.ToLower(strings.TrimSpace(model))]
	if !ok {
		return TokenRates{}, TokenRates{}, false
	}
	selected := rate.Standard
	switch tier {
	case "", "default", "standard", "auto":
	case "priority", "fast":
		if rate.Fast != nil {
			selected = *rate.Fast
		}
	case "ultrafast":
		if rate.Ultrafast != nil {
			selected = *rate.Ultrafast
		}
	default:
		return TokenRates{}, TokenRates{}, false
	}
	return selected, rate.Standard, true
}

// A daily refresh shares one fetch across readers and keeps working offline.
type Catalog struct {
	mu                     sync.Mutex
	database               *store.Store
	client                 *http.Client
	url                    string
	rates                  RateTable
	fetchedAt, attemptedAt time.Time
}

func NewCatalog(database *store.Store) *Catalog {
	return &Catalog{database: database, client: &http.Client{Timeout: 8 * time.Second}, url: RatesURL}
}

type PricingStatus struct {
	Source    string     `json:"source"`
	Status    string     `json:"status"`
	FetchedAt *time.Time `json:"fetched_at"`
}

func (c *Catalog) Read(ctx context.Context) (RateTable, PricingStatus) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now().UTC()
	if c.rates == nil {
		c.rates = RateTable{}
		var document []byte
		var fetched int64
		if c.database.DB.QueryRowContext(ctx, "SELECT fetched_at_ms,document FROM usage_rate_cache WHERE singleton_id=1").Scan(&fetched, &document) == nil {
			c.rates = ParseRates(document)
			c.fetchedAt = time.UnixMilli(fetched).UTC()
		}
	}
	if now.Sub(c.fetchedAt) >= 24*time.Hour && now.Sub(c.attemptedAt) >= time.Minute && ctx.Err() == nil {
		c.attemptedAt = now
		document := c.fetch(ctx)
		// Changing dashboard filters aborts its request, not future rate reads.
		if ctx.Err() != nil {
			c.attemptedAt = time.Time{}
		}
		if len(document) > 0 {
			if parsed := ParseRates(document); len(parsed) > 0 {
				c.rates, c.fetchedAt = parsed, now
				// Cache failure must not prevent displaying the freshly fetched rates.
				_ = c.database.Write(ctx, func(db store.Executor) error {
					_, err := db.ExecContext(ctx, `INSERT INTO usage_rate_cache VALUES(1,?,?) ON CONFLICT(singleton_id) DO UPDATE SET fetched_at_ms=excluded.fetched_at_ms,document=excluded.document`, now.UnixMilli(), string(document))
					return err
				})
			}
		}
	}
	status := PricingStatus{Source: RatesURL, Status: "unavailable"}
	if len(c.rates) > 0 {
		fetchedAt := c.fetchedAt
		status.Status, status.FetchedAt = "fresh", &fetchedAt
		if now.Sub(c.fetchedAt) >= 24*time.Hour {
			status.Status = "cached"
		}
	}
	return c.rates, status
}

func (c *Catalog) fetch(ctx context.Context) []byte {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return nil
	}
	response, err := c.client.Do(request)
	if err != nil {
		return nil
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil
	}
	const maxDocument = 16 << 20
	document, err := io.ReadAll(io.LimitReader(response.Body, maxDocument+1))
	if err != nil || len(document) > maxDocument {
		return nil
	}
	return document
}
