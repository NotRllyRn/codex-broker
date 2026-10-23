# Client key label scopes

Labels remain account groups. Fix dashboard search to match names and labels,
and use one shared scope selector when creating or editing a client key.

## Rules

- No rules: all eligible accounts, including unlabeled accounts.
- Allow: require at least one matching allowed label if any allows exist.
- Block: exclude any matching blocked label, taking precedence over allows.
- Account label edits change access; exhaustion never widens the permitted pool.
- Existing keys receive no rules, preserving their tokens and access.

## Implementation

1. Store one allow/block rule per key and label in a join table. Foreign keys
   remove rules when keys are deleted. Preserve labels referenced by scopes even
   when their last account loses that label, so removal cannot widen access.
2. Validate and save scope changes transactionally. Reuse one SQL predicate for
   central selection and the permitted account IDs published to relays.
3. Filter before preferred-account selection, retries, or reserve fallback.
   Version the relay protocol so older relays reject incompatible snapshots.
   Offline permission changes follow the existing snapshot freshness bound.
4. Use one select per label (No rule / Allow / Block), in a shared template.
   Admin session and CSRF protections apply to scope edits as to key creation.

## Verification

Exercise legacy migration, atomic validation, allow-any/block precedence,
label edits/removal, unlabeled accounts, preferred/failed-account attempts,
no permitted capacity, relay snapshots and offline reserve selection, and
admin creation/editing. Run repository checks before pushing small commits.
