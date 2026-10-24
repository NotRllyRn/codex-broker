package usage

import (
	"encoding/json"
	"testing"
	"time"
)

func TestStoredModelBreakdownsRespectFiltersAndAvoidDoubleCounting(t *testing.T) {
	db := usageDatabase(t)
	for _, sql := range []string{
		`INSERT INTO usage_identities VALUES('one','Personal usage',x'01',1),('two','Other usage',x'02',1)`,
		`INSERT INTO usage_devices(device_id,identity_id,installation_id,name,token_hash,created_at_ms) VALUES('a','one','a','Work laptop',x'01',1),('b','one','b','Home laptop',x'02',1),('c','two','c','Other device',x'03',1)`,
	} {
		if _, err := db.DB.Exec(sql); err != nil {
			t.Fatal(err)
		}
	}
	insert := func(device, day string, top bool, cost any) {
		t.Helper()
		model := map[string]any{"modelName": "example-model", "inputTokens": 10, "cacheReadTokens": 80, "cacheCreationTokens": 5, "outputTokens": 5}
		if cost != nil {
			model["cost"] = cost
		}
		row := map[string]any{"period": day, "agent": "all", "inputTokens": 10, "cacheReadTokens": 80, "cacheCreationTokens": 5, "outputTokens": 5, "totalTokens": 100, "totalCost": 1, "agents": []any{map[string]any{"agent": "example-agent", "modelBreakdowns": []any{model}}}}
		if top {
			row["modelBreakdowns"] = []any{model}
		}
		raw, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.DB.Exec(`INSERT INTO usage_reports VALUES(?,?,1,'20.0.43',?,1)`, device, day, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	insert("a", "2026-10-20", true, 1)
	insert("b", "2026-10-20", false, 1)
	insert("a", "2026-09-01", true, 1)
	insert("c", "2026-10-20", true, nil)
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		identity, device string
		days             int
		tokens           int64
		known            bool
	}{
		{"one", "all", 7, 200, true}, {"one", "a", 7, 100, true}, {"one", "all", 0, 300, true}, {"all", "all", 7, 300, false},
	} {
		summary, err := ReadIdentitySummary(t.Context(), db.DB, nil, tc.identity, tc.device, tc.days, now)
		if err != nil {
			t.Fatal(err)
		}
		if len(summary.Models) != 1 || summary.Models[0].ProcessedTokens != tc.tokens || summary.Models[0].CostKnown != tc.known {
			t.Fatalf("%+v: %+v", tc, summary.Models)
		}
		if tc.known {
			closeTo(t, summary.Models[0].CostUSD, float64(tc.tokens)/100)
		}
		if summary.CachedInput*100/(summary.CachedInput+summary.UncachedInput) != 84 {
			t.Fatal("cache share lost cache-write tokens")
		}
	}
}

func TestModelProviderFiltersDailyTotals(t *testing.T) {
	db := usageDatabase(t)
	for _, query := range []string{
		`INSERT INTO usage_identities VALUES('one','Personal usage',x'01',1)`,
		`INSERT INTO usage_devices(device_id,identity_id,installation_id,name,token_hash,created_at_ms) VALUES('a','one','a','Work laptop',x'01',1)`,
		`INSERT INTO usage_reports VALUES('a','2026-10-20',1,'20.0.43','{"period":"2026-10-20","agent":"all","inputTokens":30,"outputTokens":10,"cacheReadTokens":60,"cacheCreationTokens":0,"totalTokens":100,"totalCost":3,"agents":[],"modelBreakdowns":[{"modelName":"[pi] gpt-5","inputTokens":10,"outputTokens":5,"cacheReadTokens":40,"cost":1},{"modelName":"claude-sonnet","inputTokens":20,"outputTokens":5,"cacheReadTokens":20,"cost":2}]}',1)`,
	} {
		if _, err := db.DB.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		filter            Filter
		tokens            int64
		cost              float64
		models, available int
	}{
		{Filter{}, 100, 3, 2, 2}, {Filter{Provider: "all", Model: "all"}, 100, 3, 2, 2},
		{Filter{Provider: "openai"}, 55, 1, 1, 1}, {Filter{Model: "claude-sonnet"}, 45, 2, 1, 2},
		{Filter{Provider: "openai", Model: "claude-sonnet"}, 0, 0, 0, 1},
	} {
		s, err := ReadIdentitySummary(t.Context(), db.DB, nil, "all", "all", 7, now, tc.filter)
		if err != nil {
			t.Fatal(err)
		}
		if s.ProcessedTokens != tc.tokens || s.CostUSD != tc.cost || len(s.Models) != tc.models || len(s.AvailableModels) != tc.available || s.Daily[6].ProcessedTokens != tc.tokens {
			t.Fatalf("filter %+v: %+v", tc.filter, s)
		}
	}
	for name, want := range map[string]string{"[pi] gpt-5": "openai", "openai-codex/gpt-5": "openai", "claude-sonnet": "anthropic", "gemini-pro": "google", "custom-model": "other"} {
		if got := Provider(name); got != want {
			t.Fatalf("%s: %s", name, got)
		}
	}
}
