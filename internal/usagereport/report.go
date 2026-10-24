// Package usagereport describes ccusage's unified JSON, not agent log formats.
package usagereport

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"
)

const Version = "20.0.43"
const MaxRowBytes = 1 << 20

type Tokens struct {
	Input      int64 `json:"inputTokens"`
	Output     int64 `json:"outputTokens"`
	CacheWrite int64 `json:"cacheCreationTokens"`
	CacheRead  int64 `json:"cacheReadTokens"`
}

type Row struct {
	Tokens
	Period string  `json:"period"`
	Agent  string  `json:"agent"`
	Total  int64   `json:"totalTokens"`
	Cost   float64 `json:"totalCost"`
	Agents []Agent `json:"agents"`
}

type Agent struct {
	Tokens
	Agent  string  `json:"agent"`
	Models []Model `json:"modelBreakdowns"`
}

type Model struct {
	Tokens
	Name string `json:"modelName"`
}

// Canonical preserves unknown ccusage fields while making whitespace/key order immaterial.
func Canonical(raw json.RawMessage) (json.RawMessage, Row, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, Row{}, err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, Row{}, err
	}
	var row Row
	if err := json.Unmarshal(raw, &row); err != nil {
		return nil, row, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, row, err
	}
	for _, key := range []string{"period", "agent", "inputTokens", "outputTokens", "cacheCreationTokens", "cacheReadTokens", "totalTokens", "totalCost", "agents"} {
		if len(fields[key]) == 0 || bytes.Equal(fields[key], []byte("null")) {
			return nil, row, fmt.Errorf("ccusage report is missing %s", key)
		}
	}
	date, err := time.Parse(time.DateOnly, row.Period)
	if err != nil || date.Year() < 2000 || row.Agent != "all" || len(data) > MaxRowBytes {
		return nil, row, errors.New("invalid unified daily ccusage report")
	}
	if err := validTokens(row.Tokens); err != nil {
		return nil, row, err
	}
	if row.Total < 0 || row.Total > 1e15 || math.IsNaN(row.Cost) || math.IsInf(row.Cost, 0) || row.Cost < 0 || row.Cost > 1e12 {
		return nil, row, errors.New("invalid report totals")
	}
	agents := map[string]bool{}
	for _, agent := range row.Agents {
		if agent.Agent == "" || len(agent.Agent) > 100 || agents[agent.Agent] {
			return nil, row, errors.New("invalid or duplicate agent")
		}
		agents[agent.Agent] = true
		if err := validTokens(agent.Tokens); err != nil {
			return nil, row, err
		}
		for _, model := range agent.Models {
			if err := validTokens(model.Tokens); err != nil {
				return nil, row, err
			}
		}
	}
	return data, row, nil
}

func validTokens(t Tokens) error {
	for _, n := range []int64{t.Input, t.Output, t.CacheRead, t.CacheWrite} {
		if n < 0 || n > 1e15 {
			return errors.New("invalid token count")
		}
	}
	return nil
}

// Decreases detects disappearing logs even when another agent/model grew that day.
func Decreases(old, next Row) bool {
	lower := func(a, b Tokens) bool {
		return b.Input < a.Input || b.Output < a.Output || b.CacheRead < a.CacheRead || b.CacheWrite < a.CacheWrite
	}
	if lower(old.Tokens, next.Tokens) || next.Total < old.Total {
		return true
	}
	index := func(row Row) map[string]Tokens {
		result := map[string]Tokens{}
		for _, agent := range row.Agents {
			result[agent.Agent] = agent.Tokens
			for _, model := range agent.Models {
				result[agent.Agent+"\x00"+model.Name] = model.Tokens
			}
		}
		return result
	}
	newer := index(next)
	for key, tokens := range index(old) {
		if lower(tokens, newer[key]) {
			return true
		}
	}
	return false
}

func Daily(data []byte) ([]json.RawMessage, error) {
	var report struct {
		Daily *[]json.RawMessage `json:"daily"`
	}
	if err := json.Unmarshal(data, &report); err != nil {
		return nil, err
	}
	if report.Daily == nil {
		return nil, errors.New("ccusage did not produce a unified daily JSON report")
	}
	seen := map[string]bool{}
	result := []json.RawMessage{}
	for _, raw := range *report.Daily {
		canonical, row, err := Canonical(raw)
		if err != nil {
			return nil, err
		}
		if seen[row.Period] {
			return nil, errors.New("duplicate ccusage day")
		}
		seen[row.Period] = true
		result = append(result, canonical)
	}
	return result, nil
}
