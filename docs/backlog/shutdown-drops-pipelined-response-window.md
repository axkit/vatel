---
worth: later
where: vatel.go:490
added: 2026-11-09
---
# Shutdown can drop a pipelined response in a microsecond window inside fasthttp

`Vatel.Shutdown` relies on fasthttp v1.77 `ShutdownWithContext` (started from `stopServer`). With HTTP/1.1
pipelining, fasthttp leaves a response unflushed while the next request is already buffered. If Shutdown
starts in the microseconds between writing that response and `serveConn`'s stop check / `closeIdleConns`,
the connection is closed and the buffered response is lost. Found by reading fasthttp's `serveConn` during
the v1.25 review (fixer iteration 3), not reproduced. HTTP/1.1 lets clients retry unanswered pipelined
requests, and vatel cannot fix it without patching fasthttp.

Unknown that settles it: whether the window can be hit in practice — a reproduction (pipelined load plus
repeated Shutdown) would decide between an upstream fasthttp issue (`yes`) and dropping the item (`no`).
