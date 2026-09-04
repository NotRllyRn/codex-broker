# Informative webhook notifications

1. Give every event type a stable `WK-###` code while keeping
   `CODEX_BROKER` as the first-line source identifier; include the unique event
   ID separately.
2. Enrich incident events with account identity, state, severity, occurrence
   count, the original problem, underlying error, and recovery guidance.
3. Render Slack and Discord as readable provider-native messages; keep generic
   webhooks as structured JSON with the same notification metadata.
4. Avoid mentions, redact secrets before persistence, bound message lengths,
   and leave delivery and retry semantics unchanged.
5. Cover opened, repeated, resolved, test, and generic webhook payloads with
   focused tests.
