package usagereport

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func sample() json.RawMessage {
	return json.RawMessage(`{"period":"2026-09-01","agent":"all","inputTokens":10,"outputTokens":5,"cacheCreationTokens":0,"cacheReadTokens":20,"totalTokens":35,"totalCost":0.01,"agents":[{"agent":"future-tool","inputTokens":10,"outputTokens":5,"modelBreakdowns":[{"modelName":"model","inputTokens":10,"outputTokens":5}]}],"unknown":{"kept":true}}`)
}
func TestUnifiedReportsPreserveFieldsAndAcceptAnyAgent(t *testing.T) {
	raw, row, err := Canonical(sample())
	if err != nil || row.Output != 5 || !bytes.Contains(raw, []byte(`"unknown"`)) {
		t.Fatal(row, err)
	}
	list, err := Daily([]byte(`{"daily":[` + string(raw) + `],"totals":{}}`))
	if err != nil || len(list) != 1 {
		t.Fatal(err)
	}
	for _, bad := range []string{`{}`, `{"daily":null}`, `{"daily":[` + string(raw) + `,` + string(raw) + `]}`, `{"daily":[` + strings.Replace(string(raw), `"outputTokens":5`, `"outputTokens":-1`, 1) + `]}`} {
		if _, err := Daily([]byte(bad)); err == nil {
			t.Fatal("invalid report accepted", bad)
		}
	}
	for _, change := range []func(string) string{
		func(s string) string { return strings.Replace(s, `"totalCost":0.01`, `"totalCost":-1`, 1) },
		func(s string) string { return strings.Replace(s, `"inputTokens":10`, `"inputTokens":1.5`, 1) },
		func(s string) string { return strings.Replace(s, `"agents":[`, `"agents":null,"unused":[`, 1) },
		func(s string) string { return strings.Replace(s, `"period":"2026-09-01"`, `"period":"yesterday"`, 1) },
	} {
		if _, _, err := Canonical([]byte(change(string(raw)))); err == nil {
			t.Fatal("invalid row accepted")
		}
	}
}
func TestDecreaseChecksAgentsAndModelsNotPrices(t *testing.T) {
	_, old, err := Canonical(sample())
	if err != nil {
		t.Fatal(err)
	}
	_, newer, err := Canonical(sample())
	if err != nil {
		t.Fatal(err)
	}
	newer.Cost = 0.001
	if Decreases(old, newer) {
		t.Fatal("price change is not log loss")
	}
	newer.Output += 100
	newer.Total += 100
	newer.Agents[0].Models[0].Output--
	if !Decreases(old, newer) {
		t.Fatal("model shrink hidden by larger daily total")
	}
}

func TestModelDetailsPreservedAndValidated(t *testing.T) {
	for _, cost := range []string{"0.01", "-1"} {
		raw := strings.Replace(string(sample()), `"unknown":`, `"modelBreakdowns":[{"modelName":"example","inputTokens":10,"cost":`+cost+`}],"unknown":`, 1)
		_, row, err := Canonical([]byte(raw))
		if cost == "-1" {
			if err == nil {
				t.Fatal("negative model cost accepted")
			}
			continue
		}
		if err != nil || len(row.Models) != 1 || row.Models[0].Cost == nil || *row.Models[0].Cost != 0.01 {
			t.Fatal("model cost lost", row.Models, err)
		}
	}
}
