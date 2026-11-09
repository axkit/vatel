---
worth: later
where: endpoint.go:404
added: 2026-11-09
---
# Duplicate "message" JSON key in request log lines

zerolog writes the line text under the `message` key. `withUserValues` also writes every request user value
as its own field, so a context value set with `ctx.Set("message", …)` or a path param named `message` adds a
second `message` key on the enter and error lines (and `"***"` on the exit line for a masked path param).
No value leaks; noted by the fixer of codex iteration 5 (v1.25 review) as cosmetic and pre-existing.

Unknown that settles it: whether the log pipeline that reads vatel logs (Loki, Elasticsearch, jq-based
tooling) rejects or mishandles duplicate keys. If it does, rename the user field (e.g. `ctxMessage`) or skip
it; if not, drop the item.
