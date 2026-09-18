package broker

import (
	"encoding/json"
	"math"
)

const weekMinutes = 10_080

func NormalizeUsage(payload map[string]any) Usage {
	source := payload
	selected := ""
	if limits, ok := payload["rateLimitsByLimitId"].(map[string]any); ok {
		if codex, ok := limits["codex"].(map[string]any); ok {
			source, selected = codex, "codex"
		}
	}
	var raw []any
	if values, ok := source["windows"].([]any); ok {
		raw = values
	}
	if len(raw) == 0 {
		for _, key := range []string{"primary", "secondary"} {
			if value := source[key]; value != nil {
				raw = append(raw, value)
			}
		}
	}
	usage := Usage{SelectedLimitID: selected}
	var windows []Window
	var weekly, short []int
	for index, item := range raw {
		object, _ := item.(map[string]any)
		if object == nil {
			continue
		}
		window := Window{Slot: stringValue(object["name"]), UsedPercent: intPointer(object["usedPercent"]), DurationMinutes: intPointer(object["windowDurationMins"]), ResetsAtS: intPointer(object["resetsAt"])}
		if window.Slot == "" {
			window.Slot = fmtInt(index)
		}
		windows = append(windows, window)
		position := len(windows) - 1
		if window.DurationMinutes != nil && math.Abs(float64(*window.DurationMinutes-weekMinutes)) <= weekMinutes*.05 {
			weekly = append(weekly, position)
		} else if window.DurationMinutes != nil && *window.DurationMinutes > 0 && *window.DurationMinutes < 1440 {
			short = append(short, position)
		}
	}
	chosenWeekly, chosenShort := -1, -1
	if len(weekly) == 1 {
		chosenWeekly = weekly[0]
		value := windows[chosenWeekly]
		usage.Weekly = &value
	}
	if len(short) > 0 {
		for i := range short {
			for j := i + 1; j < len(short); j++ {
				if *windows[short[j]].DurationMinutes < *windows[short[i]].DurationMinutes {
					short[i], short[j] = short[j], short[i]
				}
			}
		}
		if len(short) == 1 || *windows[short[0]].DurationMinutes != *windows[short[1]].DurationMinutes {
			chosenShort = short[0]
			value := windows[chosenShort]
			usage.Short = &value
		}
	}
	for index, window := range windows {
		if index != chosenShort && index != chosenWeekly {
			usage.Others = append(usage.Others, window)
		}
	}
	return usage
}

func intPointer(value any) *int64 {
	if number, ok := value.(float64); ok {
		result := int64(number)
		return &result
	}
	if number, ok := value.(int64); ok {
		return &number
	}
	return nil
}
func stringValue(value any) string { result, _ := value.(string); return result }
func fmtInt(value int) string      { encoded, _ := json.Marshal(value); return string(encoded) }

func freshness(last, now int64, pollSeconds int) string {
	if last == 0 {
		return "UNKNOWN"
	}
	age := now - last
	if age <= int64(pollSeconds)*2000 {
		return "FRESH"
	}
	if age <= 30*60*1000 {
		return "AGING"
	}
	return "STALE"
}

func overall(enabled bool, auth, worker, usage string, hasError bool) string {
	if !enabled {
		return "DISABLED"
	}
	if auth == "AUTH_REQUIRED" || auth == "WORKSPACE_MISMATCH" || auth == "CREDENTIAL_ERROR" {
		return "ACTION_REQUIRED"
	}
	if hasError || worker == "CRASHED" {
		return "ERROR"
	}
	if worker == "STARTING" || auth == "UNCONFIGURED" || auth == "ENROLLING" {
		return "STARTING"
	}
	if usage == "AGING" || usage == "STALE" || usage == "ERROR" {
		return "WARNING"
	}
	return "HEALTHY"
}
