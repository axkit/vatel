---
worth: yes
where: ws_vatel.go:333
added: 2026-11-09
---
# Panics in WebSocket OnOpen/OnClose callbacks are not recovered

Since v1.25 vatel recovers panics in the HTTP pipeline and in WS message handlers (500 `VTL-0005`, stack in
the log, Alarmer). The user callbacks set with `WebsocketVatel.OnOpen` / `OnClose` run from `onOpen` /
`onClose` outside message handling, so a panic there is not recovered and takes down the process (fasthttp
and the websocket library do not recover). Left out of the v1.25 panic-recovery change as out of its scope
(scope was message handlers).

Fix: wrap `wv.callbackOnOpen(c)` / `wv.callbackOnClose(c, err)` in a recover that logs the panic and stack
and calls Alarmer (no response to write); a panic in OnOpen should also close the connection. Add tests next
to `TestWS_HandlerPanic`.
