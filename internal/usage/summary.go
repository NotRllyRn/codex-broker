package usage

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/usagereport"
)

type Cost struct {
	Input     float64 `json:"input"`
	CacheRead float64 `json:"cache_read"`
	Output    float64 `json:"output"`
	Other     float64 `json:"other"`
}

type SpeedCost struct {
	Standard  float64 `json:"standard"`
	Fast      float64 `json:"fast"`
	Ultrafast float64 `json:"ultrafast"`
	Premium   float64 `json:"premium"`
}

type Totals struct {
	Reports               int64     `json:"reports"`
	Responses             int64     `json:"responses"`
	ProcessedTokens       int64     `json:"processed_tokens"`
	CachedInput           int64     `json:"cached_input"`
	UncachedInput         int64     `json:"uncached_input"`
	Output                int64     `json:"output"`
	CostUSD               float64   `json:"cost_usd"`
	CacheSavingsUSD       float64   `json:"cache_savings_usd"`
	UnpricedResponses     int64     `json:"unpriced_responses"`
	MissingUsageResponses int64     `json:"missing_usage_responses"`
	Category              Cost      `json:"category"`
	Speed                 SpeedCost `json:"speed"`
}

type Day struct {
	Day string `json:"day"`
	Totals
}

type ModelUsage struct {
	Name            string  `json:"name"`
	ProcessedTokens int64   `json:"processed_tokens"`
	CostUSD         float64 `json:"cost_usd"`
	CostKnown       bool    `json:"cost_known"`
}

// Provider groups model families; reports do not identify the serving vendor.
func Provider(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	for strings.HasPrefix(name, "[") && strings.Contains(name, "]") {
		name = strings.TrimSpace(name[strings.Index(name, "]")+1:])
	}
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	for _, prefix := range []string{"gpt-", "codex", "o1", "o3", "o4", "chatgpt"} {
		if strings.HasPrefix(name, prefix) {
			return "openai"
		}
	}
	if strings.HasPrefix(name, "claude") {
		return "anthropic"
	}
	if strings.HasPrefix(name, "gemini") {
		return "google"
	}
	return "other"
}

type Filter struct{ Provider, Model string }

func (f Filter) matches(name string) bool {
	return (f.Provider == "" || f.Provider == "all" || Provider(name) == f.Provider) && (f.Model == "" || f.Model == "all" || name == f.Model)
}
func selectedFilter(filters []Filter) Filter {
	if len(filters) > 0 {
		return filters[0]
	}
	return Filter{}
}

type Summary struct {
	AvailableModels []string     `json:"available_models"`
	Models          []ModelUsage `json:"models"`
	CCUsage         bool         `json:"ccusage"`
	Totals
	Daily        []Day         `json:"daily"`
	Since        string        `json:"since"`
	Until        string        `json:"until"`
	HistorySince string        `json:"history_since"`
	Pricing      PricingStatus `json:"pricing"`
}

// The profile endpoint has no model/cache breakdown, so only recorded proxy
// tokens enter these estimates. Reasoning is already included in output.
func ReadSummary(ctx context.Context, database *sql.DB, rates RateTable, account string, days int, now time.Time, filters ...Filter) (Summary, error) {
	return readSummary(ctx, database, rates, `SELECT d.*,a.public_token AS owner,'' AS device_id,0 AS cache_write_tokens FROM usage_daily d JOIN accounts a USING(account_id)`, account, "all", days, now, filters...)
}

// Identity totals come exclusively from cumulative ccusage reports, never old events.
func ReadIdentitySummary(ctx context.Context, database *sql.DB, _ RateTable, identity, device string, days int, now time.Time, filters ...Filter) (Summary, error) {
	until := now.UTC().Format(time.DateOnly)
	since := now.UTC().AddDate(0, 0, 1-days).Format(time.DateOnly)
	result := Summary{CCUsage: true, Until: until, Daily: []Day{}, Pricing: PricingStatus{Status: "ccusage"}}
	rows, err := database.QueryContext(ctx, `SELECT r.day,r.payload FROM usage_reports r JOIN usage_devices d USING(device_id) WHERE (?='all' OR d.identity_id=?) AND (?='all' OR r.device_id=?) AND r.day<=? ORDER BY r.day`, identity, identity, device, device, until)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	byDay := map[string]*Totals{}
	byModel := map[string]*ModelUsage{}
	filter := selectedFilter(filters)
	available := map[string]bool{}
	for rows.Next() {
		var day string
		var raw []byte
		if err := rows.Scan(&day, &raw); err != nil {
			return result, err
		}
		if result.HistorySince == "" {
			result.HistorySince = day
		}
		if days != 0 && day < since {
			continue
		}
		_, report, err := usagereport.Canonical(raw)
		if err != nil {
			return result, err
		}
		models := report.Models
		if len(models) == 0 {
			for _, agent := range report.Agents {
				models = append(models, agent.Models...)
			}
		}
		value := Totals{Reports: 1, ProcessedTokens: report.Total, UncachedInput: report.Input + report.CacheWrite, CachedInput: report.CacheRead, Output: report.Output, CostUSD: report.Cost}
		filtered := (filter.Provider != "" && filter.Provider != "all") || (filter.Model != "" && filter.Model != "all")
		if filtered {
			value = Totals{}
		}
		for _, model := range models {
			if filter.Provider == "" || filter.Provider == "all" || Provider(model.Name) == filter.Provider {
				available[model.Name] = true
			}
			if !filter.matches(model.Name) {
				continue
			}
			known := model.Cost != nil
			cost := 0.0
			if known {
				cost = *model.Cost
			}
			if filtered {
				value.Reports = 1
				value.ProcessedTokens += model.Input + model.Output + model.CacheRead + model.CacheWrite
				value.UncachedInput += model.Input + model.CacheWrite
				value.CachedInput += model.CacheRead
				value.Output += model.Output
				value.CostUSD += cost
			}
			addModel(byModel, model.Name, model.Input+model.Output+model.CacheRead+model.CacheWrite, cost, known)
		}
		if byDay[day] == nil {
			byDay[day] = &Totals{}
		}
		byDay[day].add(value)
		result.Totals.add(value)
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	if days == 0 {
		since = result.HistorySince
		if since == "" {
			since = until
		}
	}
	result.Since = since
	start, _ := time.Parse(time.DateOnly, since)
	for date := start; date.Format(time.DateOnly) <= until; date = date.AddDate(0, 0, 1) {
		day := Day{Day: date.Format(time.DateOnly)}
		if value := byDay[day.Day]; value != nil {
			day.Totals = *value
		}
		result.Daily = append(result.Daily, day)
	}
	for name := range available {
		result.AvailableModels = append(result.AvailableModels, name)
	}
	sort.Strings(result.AvailableModels)
	result.Models = sortedModels(byModel)
	return result, nil
}

func readSummary(ctx context.Context, database *sql.DB, rates RateTable, source, owner, device string, days int, now time.Time, filters ...Filter) (Summary, error) {
	until := now.UTC().Format(time.DateOnly)
	since := now.UTC().AddDate(0, 0, 1-days).Format(time.DateOnly)
	result := Summary{Since: since, Until: until, Daily: []Day{}}
	var earliest sql.NullString
	if err := database.QueryRowContext(ctx, `SELECT min(d.day) FROM (`+source+`) d WHERE (?='all' OR d.owner=?) AND (?='all' OR d.device_id=?)`, owner, owner, device, device).Scan(&earliest); err != nil {
		return result, err
	}
	result.HistorySince = earliest.String
	if days == 0 {
		since = until
		if earliest.Valid && earliest.String < until {
			since = earliest.String
		}
		result.Since = since
		start, _ := time.Parse(time.DateOnly, since)
		end, _ := time.Parse(time.DateOnly, until)
		days = int(end.Sub(start).Hours()/24) + 1
	}
	rows, err := database.QueryContext(ctx, `SELECT d.day,d.model,d.service_tier,d.usage_known,sum(d.responses),sum(d.uncached_input_tokens),sum(d.cached_input_tokens),sum(d.output_tokens),sum(d.cache_write_tokens)
		FROM (`+source+`) d
		WHERE d.day>=? AND d.day<=? AND (?='all' OR d.owner=?) AND (?='all' OR d.device_id=?)
		GROUP BY d.day,d.model,d.service_tier,d.usage_known ORDER BY d.day`, since, until, owner, owner, device, device)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	byDay := map[string]*Totals{}
	byModel := map[string]*ModelUsage{}
	filter := selectedFilter(filters)
	available := map[string]bool{}
	for rows.Next() {
		var day, model, tier string
		var known bool
		var value Totals
		var cacheWrite int64
		if err := rows.Scan(&day, &model, &tier, &known, &value.Responses, &value.UncachedInput, &value.CachedInput, &value.Output, &cacheWrite); err != nil {
			return result, err
		}
		if filter.Provider == "" || filter.Provider == "all" || Provider(model) == filter.Provider {
			available[model] = true
		}
		if !filter.matches(model) {
			continue
		}
		value.ProcessedTokens = value.UncachedInput + value.CachedInput + value.Output
		selected, standard, priced := rates.Lookup(model, tier)
		if !known {
			value.MissingUsageResponses = value.Responses
		} else if !priced || cacheWrite > 0 {
			value.UnpricedResponses = value.Responses
		} else {
			value.Category = Cost{Input: float64(value.UncachedInput) * selected.Input, CacheRead: float64(value.CachedInput) * selected.Cached, Output: float64(value.Output) * selected.Output}
			value.CostUSD = value.Category.Input + value.Category.CacheRead + value.Category.Output
			value.CacheSavingsUSD = float64(value.CachedInput) * (selected.Input - selected.Cached)
			switch tier {
			case "priority", "fast":
				value.Speed.Fast = value.CostUSD
			case "ultrafast":
				value.Speed.Ultrafast = value.CostUSD
			default:
				value.Speed.Standard = value.CostUSD
			}
			base := float64(value.UncachedInput)*standard.Input + float64(value.CachedInput)*standard.Cached + float64(value.Output)*standard.Output
			value.Speed.Premium = value.CostUSD - base
		}
		addModel(byModel, model, value.ProcessedTokens, value.CostUSD, known && priced && cacheWrite == 0)
		if byDay[day] == nil {
			byDay[day] = &Totals{}
		}
		byDay[day].add(value)
		result.Totals.add(value)
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	for date := now.UTC().AddDate(0, 0, 1-days); date.Format(time.DateOnly) <= until; date = date.AddDate(0, 0, 1) {
		day := Day{Day: date.Format(time.DateOnly)}
		if totals := byDay[day.Day]; totals != nil {
			day.Totals = *totals
		}
		result.Daily = append(result.Daily, day)
	}
	for name := range available {
		result.AvailableModels = append(result.AvailableModels, name)
	}
	sort.Strings(result.AvailableModels)
	result.Models = sortedModels(byModel)
	return result, nil
}

func (t *Totals) add(v Totals) {
	t.Reports += v.Reports
	t.Responses += v.Responses
	t.ProcessedTokens += v.ProcessedTokens
	t.CachedInput += v.CachedInput
	t.UncachedInput += v.UncachedInput
	t.Output += v.Output
	t.CostUSD += v.CostUSD
	t.CacheSavingsUSD += v.CacheSavingsUSD
	t.UnpricedResponses += v.UnpricedResponses
	t.MissingUsageResponses += v.MissingUsageResponses
	t.Category.Input += v.Category.Input
	t.Category.CacheRead += v.Category.CacheRead
	t.Category.Output += v.Category.Output
	t.Category.Other += v.Category.Other
	t.Speed.Standard += v.Speed.Standard
	t.Speed.Fast += v.Speed.Fast
	t.Speed.Ultrafast += v.Speed.Ultrafast
	t.Speed.Premium += v.Speed.Premium
}

func addModel(models map[string]*ModelUsage, name string, tokens int64, cost float64, known bool) {
	if name == "" {
		name = "Unknown model"
	}
	model := models[name]
	if model == nil {
		model = &ModelUsage{Name: name, CostKnown: true}
		models[name] = model
	}
	model.ProcessedTokens += tokens
	model.CostUSD += cost
	model.CostKnown = model.CostKnown && known
}

func sortedModels(models map[string]*ModelUsage) []ModelUsage {
	result := make([]ModelUsage, 0, len(models))
	for _, model := range models {
		result = append(result, *model)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CostUSD != result[j].CostUSD {
			return result[i].CostUSD > result[j].CostUSD
		}
		return result[i].Name < result[j].Name
	})
	return result
}
