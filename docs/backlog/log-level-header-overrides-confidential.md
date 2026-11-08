---
worth: later
where: endpoint.go:246
added: 2026-11-09
---
# X-Set-Log-Level lets any client enable body logging on confidential endpoints

`resolveLogLevel` honours the `X-Set-Log-Level` request header unless `WithStaticLoggingLevel` is set. Any
client can send `req-body,req-input` and have request bodies logged on endpoints configured with
`LogConfidential` or `LogSilent`, so secrets without a `mask` tag end up in the log. The header can also be
used to flood the log. Surfaced by the plan review of `docs/plans/20261108-finish-refactor-v1.25.md`
(Gostly issue #10 context).

Unknown that settles it: how the header is used in practice (dev only, or prod debugging too). Options:
- off by default, enabled by an explicit option (e.g. `WithLogLevelHeader()`); `WithStaticLoggingLevel`
  goes away — changes the default, goes to MIGRATION.md;
- keep the default, but the header cannot add `req-body`/`req-input` where `LogOptions` are set explicitly
  (does not stop log flooding);
- docs only: recommend `WithStaticLoggingLevel` in production.
