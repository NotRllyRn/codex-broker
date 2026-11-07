# Account usage

ID: `account-usage`. Enabled by default; globally toggleable under Settings →
Proxy extensions.

Adds the existing account/quota prefix to final answers using the shared stream
formatter. Disabling presentation retains replay cleanup and actual accounting.
Request size compaction is an independently toggleable extension.

Settings expose `showAccountLabel` and `showRetryStatus` (both default true).
Changes apply to new requests. Disabling display fields does not alter accounting.
