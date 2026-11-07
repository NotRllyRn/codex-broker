package accountusage

import "github.com/NotRllyRn/codex-broker/extensions/settings"

var Settings = settings.Schema{Fields: []settings.Field{
	{Key: "showAccountLabel", Type: "boolean", Label: "Show account label", Default: settings.JSON(true), Description: "Turn off to display the generic Usage stats label."},
	{Key: "showRetryStatus", Type: "boolean", Label: "Show retry and failover status", Default: settings.JSON(true)},
}}
