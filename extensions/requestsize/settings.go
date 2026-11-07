package requestsize

import (
	"fmt"

	"github.com/NotRllyRn/codex-broker/extensions/settings"
)

var Settings = settings.Schema{Description: "Control oversized request cleanup. Sizes are measured in MiB.", Fields: []settings.Field{
	{Key: "triggerMiB", Type: "number", Label: "Start cleanup at", Default: settings.JSON(50), Min: settings.Number(1), Max: settings.Number(64), Step: settings.Number(1), Unit: "MiB", Required: true},
	{Key: "targetMiB", Type: "number", Label: "Target request size", Default: settings.JSON(40), Min: settings.Number(1), Max: settings.Number(63), Step: settings.Number(1), Unit: "MiB", Required: true},
}}

func ValidateSettings(values settings.Values) error {
	if values["targetMiB"].(float64) >= values["triggerMiB"].(float64) {
		return fmt.Errorf("target size must be smaller than the cleanup trigger")
	}
	return nil
}
func Policy(values settings.Values) ScrubPolicy {
	return ScrubPolicy{TriggerBytes: int(values["triggerMiB"].(float64)) << 20, TargetBytes: int(values["targetMiB"].(float64)) << 20}
}
