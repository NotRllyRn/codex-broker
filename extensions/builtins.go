// Package extensions is the catalog of extensions built into Codex Broker.
package extensions

import (
	"github.com/NotRllyRn/codex-broker/extensions/accountusage"
	"github.com/NotRllyRn/codex-broker/extensions/requestsize"
)

type Info struct {
	ID, Name, Description string
	Enabled               bool
}

func Builtins(disabled map[string]bool) []Info {
	return []Info{
		{ID: accountusage.ID, Name: "Account usage", Description: "Add the account and quota prefix to answers. Actual usage accounting and replay cleanup stay active when disabled.", Enabled: !disabled[accountusage.ID]},
		{ID: requestsize.ID, Name: "Request size compaction", Description: "Clean oversized request history and prompt native compaction. Disabling keeps history unchanged; requests above the forwarding limit are rejected.", Enabled: !disabled[requestsize.ID]},
	}
}
func Builtin(id string) bool {
	for _, entry := range Builtins(nil) {
		if entry.ID == id {
			return true
		}
	}
	return false
}
