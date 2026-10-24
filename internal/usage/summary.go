package usage

import (
	"context"
	"database/sql"
	"time"
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

type Summary struct {
	Totals
	Daily        []Day         `json:"daily"`
	Since        string        `json:"since"`
	Until        string        `json:"until"`
	HistorySince string        `json:"history_since"`
	Pricing      PricingStatus `json:"pricing"`
}

// The profile endpoint has no model/cache breakdown, so only recorded proxy
// tokens enter these estimates. Reasoning is already included in output.
func ReadSummary(ctx context.Context, database *sql.DB, rates RateTable, account string, days int, now time.Time) (Summary, error) {
	return readSummary(ctx, database, rates, `SELECT d.*,a.public_token AS owner,'' AS device_id,0 AS cache_write_tokens FROM usage_daily d JOIN accounts a USING(account_id)`, account, "all", days, now)
}

// Identity events retain numerical history independently of proxy response retention.
func ReadIdentitySummary(ctx context.Context, database *sql.DB, rates RateTable, identity, device string, days int, now time.Time) (Summary, error) {
	return readSummary(ctx, database, rates, `SELECT strftime('%Y-%m-%d',timestamp_ms/1000,'unixepoch') AS day, identity_id AS owner,COALESCE(device_id,'proxy') AS device_id,model,service_tier,usage_known,1 AS responses,input_tokens-cached_input_tokens AS uncached_input_tokens,cached_input_tokens,output_tokens,cache_write_tokens FROM identity_usage`, identity, device, days, now)
}

func readSummary(ctx context.Context, database *sql.DB, rates RateTable, source, owner, device string, days int, now time.Time) (Summary, error) {
	until := now.UTC().Format(time.DateOnly)
	since := now.UTC().AddDate(0, 0, 1-days).Format(time.DateOnly)
	result := Summary{Since: since, Until: until, Daily: []Day{}}
	var earliest sql.NullString
	if err := database.QueryRowContext(ctx, `SELECT min(d.day) FROM (`+source+`) d WHERE (?='all' OR d.owner=?) AND (?='all' OR d.device_id=?)`, owner, owner, device, device).Scan(&earliest); err != nil {
		return result, err
	}
	result.HistorySince = earliest.String
	rows, err := database.QueryContext(ctx, `SELECT d.day,d.model,d.service_tier,d.usage_known,sum(d.responses),sum(d.uncached_input_tokens),sum(d.cached_input_tokens),sum(d.output_tokens),sum(d.cache_write_tokens)
		FROM (`+source+`) d
		WHERE d.day>=? AND d.day<=? AND (?='all' OR d.owner=?) AND (?='all' OR d.device_id=?)
		GROUP BY d.day,d.model,d.service_tier,d.usage_known ORDER BY d.day`, since, until, owner, owner, device, device)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	byDay := map[string]*Totals{}
	for rows.Next() {
		var day, model, tier string
		var known bool
		var value Totals
		var cacheWrite int64
		if err := rows.Scan(&day, &model, &tier, &known, &value.Responses, &value.UncachedInput, &value.CachedInput, &value.Output, &cacheWrite); err != nil {
			return result, err
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
	return result, nil
}

func (t *Totals) add(v Totals) {
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
