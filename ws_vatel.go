package vatel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/axkit/errors"
	"github.com/valyala/fasthttp"

	"github.com/regorov/websocket"
	"github.com/rs/zerolog"
)

type WebsocketVatel struct {
	isPublicAccessAllowed bool
	ws                    websocket.Server
	path                  map[string]*Endpoint
	cfg                   WsOption
	callbackOnOpen        func(*websocket.Conn)
	callbackOnClose       func(*websocket.Conn, error)
	callbackOnTokenUpdate func(cid uint64, tp TokenPayloader) error
}

type WsOption struct {
	log *zerolog.Logger
	td  TokenDecoder
}

func WithLogger(log *zerolog.Logger) func(*WsOption) {
	return func(o *WsOption) {
		zl := log.With().Str("layer", "ww").Logger()
		o.log = &zl
	}
}

func WithTokenDecoder(td TokenDecoder) func(*WsOption) {
	return func(o *WsOption) {
		o.td = td
	}
}

func NewWebsocketVatel(optFunc ...func(*WsOption)) *WebsocketVatel {
	ww := WebsocketVatel{
		path: make(map[string]*Endpoint),
	}

	for i := range optFunc {
		optFunc[i](&ww.cfg)
	}

	ww.ws.HandleData(ww.onMessage)
	return &ww
}

func (wv *WebsocketVatel) Endpoints() []Endpoint {
	return []Endpoint{
		{
			Path:                "updateAccessToken",
			Method:              "WS",
			LogOptions:          LogConfidential,
			WebsocketController: func() WebsocketHandler { return &AccessTokenUpdateHandler{wv: wv} },
		},
	}
}

// RegisterEndpoint is invocated by Vatel.MustBuildHandler() for every endpoint having method "WS".
func (ww *WebsocketVatel) RegisterEndpoint(v *Vatel, e *Endpoint, l *zerolog.Logger) error {
	if _, ok := ww.path[e.Path]; ok {
		return errors.New("endpoint is already registered").Set("path", e.Path)
	}

	if err := ww.compile(v, e, l); err != nil {
		return err
	}

	ww.path[e.Path] = e
	return nil
}

func (ww *WebsocketVatel) compile(v *Vatel, e *Endpoint, l *zerolog.Logger) error {
	opath := "ws:" + e.Path
	e.auth = v.auth
	e.td = v.td
	e.pm = v.pm
	e.rd = v.rd
	e.rtc = v.rtc
	e.middlewares = v.mdw
	e.staticLoggingLevel = v.cfg.staticLoggingLevel
	e.verboseError = v.cfg.verboseError
	e.logRequestID = v.cfg.logRequestID
	e.jm = v.cfg.jm
	e.ala = v.cfg.ala
	e.mr = v.cfg.mr
	e.wsLogger = l

	if e.LogOptions == LogUnknown {
		e.LogOptions = v.cfg.defaultLogOption
	}

	if e.LogOptions&LogSilent == e.LogOptions {
		e.LogOptions = e.LogOptions
	}

	if len(e.Perms) > 0 {
		if e.auth == nil && !v.authDisabled {
			return fmt.Errorf("endpoint %s %s requires calling SetAuthorizer() before", e.Method, opath)
		}
		if e.td == nil && !v.authDisabled {
			return fmt.Errorf("endpoint %s %s requires calling SetTokenDecode() before", e.Method, opath)
		}

		if e.pm == nil && !v.authDisabled {
			return fmt.Errorf("endpoint %s %s requires calling SetPermissionManager() before", e.Method, opath)
		}

		for i := range e.Perms {
			pb, ok := v.pm.PermissionBitPos(e.Perms[i])
			if !ok {
				return fmt.Errorf("endpoint %s %s mentioned unknown permission %s", e.Method, opath, e.Perms[i])
			}
			e.perms = append(e.perms, pb)
		}
	}

	c := e.WebsocketController()
	if c == nil {
		return fmt.Errorf("endpoint %s %s missing WebsocketController handler ", e.Method, opath)
	}

	ri, hasRespBody := c.(Resulter)
	if hasRespBody && e.jm != nil {
		e.resultFields = e.jm.Fields(ri.Result(), "mask")
	}
	e.hasRespBody = hasRespBody

	ii, isInputer := c.(Inputer)
	if isInputer && e.jm != nil {
		e.inputFields = e.jm.Fields(ii.Input(), "mask")
	}

	e.isRequestBodyExpected = isInputer
	return nil
}

// OnOpen invocates by websocket server when new connection established.
func (ww *WebsocketVatel) OnOpen(f func(*websocket.Conn)) {
	ww.callbackOnOpen = f
	ww.ws.HandleOpen(ww.onOpen)
}

func (ww *WebsocketVatel) OnClose(f func(*websocket.Conn, error)) {
	ww.callbackOnClose = f
	ww.ws.HandleClose(ww.onClose)
}

func (dws *WebsocketVatel) onOpen(c *websocket.Conn) {
	if dws.callbackOnOpen != nil {
		dws.callbackOnOpen(c)
	}
}

// OnClose invocates by websocket server when connection is closed.
func (ww *WebsocketVatel) onClose(c *websocket.Conn, err error) {
	if ww.callbackOnClose != nil {
		ww.callbackOnClose(c, err)
	}
}

func (ww *WebsocketVatel) Upgrade(ctx *fasthttp.RequestCtx) {
	ww.ws.Upgrade(ctx)
}

func (ww *WebsocketVatel) UpgradeWithID(ctx *fasthttp.RequestCtx, id uint64) {
	ww.ws.UpgradeWithID(ctx, id)
}

type WebsocketRequest struct {
	Path string          `json:"path"`
	Data json.RawMessage `json:"data,omitempty"`
}

type AuthMessageData struct {
	AccessToken string `json:"accessToken"`
}

func (dws *WebsocketVatel) onMessage(c *websocket.Conn, isBinary bool, data []byte) {

	ctx := NewWsContext(c)

	var msg WebsocketRequest
	if err := json.Unmarshal(data, &msg); err != nil {
		ex := errors.Catch(err).Set("reason", "invalid json").StatusCode(400).Msg("bad request")
		c.Write(errors.ToClientJSON(ex))
		return
	}

	if msg.Path == "" {
		c.Write(errors.ToClientJSON(errors.New("bad request").Set("reason", "empty path").StatusCode(400)))
		return
	}

	e, ok := dws.path[msg.Path]
	if !ok {
		err := errors.New("unknown path").StatusCode(404)
		c.Write(errors.ToClientJSON(err))
		return
	}

	var (
		zc  zerolog.Context
		zco zerolog.Context
	)

	verbose := e.verboseError

	var lo LogOption
	if !e.staticLoggingLevel {
		lo = LogOption(atomic.LoadUint32((*uint32)(&e.LogOptions)))
	} else {
		lo = e.LogOptions
	}

	zco = e.wsLogger.With().Str("client", c.RemoteAddr().String())
	if e.logRequestID {
		zco = zco.Uint64("connectionId", c.ID())
	}
	zc = zco

	// AUTH PART !!!
	// dws.mux.RLock()
	// cidx, ok := dws.idx[c.ID()]
	// if !ok {
	// 	dws.mux.RUnlock()
	// 	dws.mux.Lock()
	// 	if cidx, ok = dws.idx[c.ID()]; !ok {
	// 		cidx = dws.onOpen(c)
	// 	}
	// 	dws.mux.Unlock()
	// 	dws.mux.RLock()
	// }

	// cp := &dws.conns[cidx]
	// if cp.tp != nil {
	// 	ctx.SetTokenPayload(cp.tp)
	// }
	// dws.mux.RUnlock()

	// for i := range e.middlewares[BeforeAuthorization] {
	// 	if err := e.middlewares[BeforeAuthorization][i](ctx); err != nil {
	// 		e.writeErrorResponse(ctx, verbose, &zc, err)
	// 		return
	// 	}
	// }

	// inDebug := e.LogOptions&ConfidentialInput != ConfidentialInput
	// outDebug := e.LogOptions&ConfidentialOutput != ConfidentialOutput

	if len(e.Perms) > 0 && e.auth != nil {
		switch len(e.Perms) {
		case 0:
			break
		case 1:
			zc = zc.Str("perm", e.Perms[0])
		default:
			zc = zc.Strs("perms", e.Perms)
		}

		// AUTH PART !!!
		// if cp.tp.Role() == 0 {
		// 	wsWriteErrorResponse(e, ctx, c, verbose, &zc, errors.Forbidden().Capture())
		// 	return
		// }

		if err := e.wsAuthorize(ctx); err != nil {
			wsWriteErrorResponse(e, ctx, c, verbose, &zc, err)
			return
		}

		// if e.rd != nil {
		// 	//	inDebug, outDebug = e.rd.IsDebugRequired(token.ApplicationPayload())
		// }
		// t := token.ApplicationPayload()
		// ctx.SetTokenPayload(t)
		// verbose = verbose || t.Debug()
	}

	h := e.WebsocketController()

	if lo&LogEnter == LogEnter {
		// ctx.RequestCtx().VisitUserValues(func(key []byte, v interface{}) {
		// 	zc = zc.Interface(string(key), v)
		// })

		zl := zc.Logger()
		zl.Debug().Msg("new request")
		zc = zco
	}
	if input, ok := h.(Inputer); ok {
		err := json.Unmarshal(msg.Data, input.Input())
		if err != nil {
			wsWriteErrorResponse(e, ctx, c, verbose, &zc, err)
			return
		}
	}

	if err := h.Handle(ctx); err != nil {
		wsWriteErrorResponse(e, ctx, c, verbose, &zc, err)
		return
	}

	if e.hasRespBody {
		if err := e.wsWriteResponse(c, lo, h.(Resulter).Result(), &zc); err != nil {
			wsWriteErrorResponse(e, ctx, c, verbose, &zc, err)
			return
		}
	}

	dur := time.Since(ctx.Created())
	if lo&LogExit == LogExit {
		msg := "completed"
		if e.LogOptions&LogEnter != LogEnter {
			msg = "processed"
		}
		ctx.VisitValues(func(key []byte, v interface{}) {
			if bytes.Equal(key, []byte("message")) {
				msg = v.(string)
				return
			}
			zc = zc.Interface(string(key), v)
		})

		zl := zc.Logger()
		zl.Debug().Str("dur", dur.String()).Msg(msg)
	}

	if e.mr != nil {
		e.mr.ReportMetric(e.Method, e.Path, 200, dur.Seconds(), 0) // len(fctx.Response.Body()))
	}

	//e.wsLogger.Debug().RawJSON("input", data).Msg("request body")

	//e.Controller().Inp
}

// func (dws *WebsocketWrapper) PingAll() {
// 	dws.mux.RLock()
// 	if len(dws.conns) == 0 {
// 		dws.mux.RUnlock()
// 		return
// 	}
// 	cs := make([]*websocket.Conn, 0, len(dws.conns))
// 	for i := range dws.conns {
// 		if dws.conns[i].wc != nil {
// 			cs = append(cs, dws.conns[i].wc)
// 		}
// 	}
// 	dws.mux.RUnlock()
// 	for i := range cs {
// 		cs[i].Write([]byte(`{"push": "ping"}`))
// 	}
// }

// func (wg *WebsocketWrapper) Auth(c *websocket.Conn, token []byte) error {

// 	if len(token) == 0 {
// 		return nil
// 	}

// 	if wg.va.td == nil {
// 		return errors.Forbidden().Msg("token decoder is missed")
// 	}

// 	t, err := wg.va.td.Decode(token)
// 	if err != nil {
// 		return err
// 	}

// 	wg.mux.Lock()
// 	defer wg.mux.Unlock()
// 	idx, ok := wg.cdx[c.ID()]
// 	if !ok {
// 		idx = wg.onOpen(c)
// 	}

// 	wg.conns[idx].tp = t.ApplicationPayload()
// 	return nil
// }

func wsWriteErrorResponse(e *Endpoint, ctx WebsocketContext, c *websocket.Conn, verbose bool, zc *zerolog.Context, err error) {
	if err == nil {
		return
	}

	statusCode := 500
	ce, ok := err.(*errors.CatchedError)
	if ok {
		statusCode = ce.Last().StatusCode
		if statusCode == 429 {
			// in case of too many requests, look if error has attribute Retry-After
			var hv []byte
			if ra, ok := ce.Get("Retry-After"); ok {
				switch ra.(type) {
				case int, int64, int32, int16, int8, uint, uint64, uint32, uint16, uint8:
					hv = []byte(fmt.Sprintf("%d", ra))
				case string:
					hv = []byte(ra.(string))
				case []byte:
					hv = ra.([]byte)
				}
				ctx.Set("Retry-After", hv)
			}
		}
	}

	z := *zc
	ctx.VisitValues(func(key []byte, v interface{}) {
		z = z.Interface(string(key), v)
	})

	zl := z.RawJSON("err", errors.ToServerJSON(err)).Logger()
	zl.Error().Msg("request failed")

	var ff errors.FormattingFlag
	if verbose {
		ff = errors.AddStack | errors.AddFields | errors.AddWrappedErrors
	}

	buf := errors.ToJSON(err, ff)
	_, xerr := c.Write(buf)

	if xerr != nil {
		//zl.With().Error().RawJSON("err", errors.ToServerJSON(xerr)).Msg("writing http response failed")
	}

	if e != nil && e.mr != nil {
		e.mr.ReportMetric(e.Method, e.Path, statusCode, time.Since(ctx.Created()).Seconds(), len(buf))
	}

	if e != nil && e.ala != nil && statusCode >= 500 {
		e.ala.Alarm(err)
	}

	return
}

func (e *Endpoint) wsAuthorize(ctx WebsocketContext) error {

	isAllowed, err := e.auth.IsAllowed(ctx.TokenPayload().Perms(), e.perms...)
	if err == nil {
		if isAllowed {
			return nil
		}
		return errors.Forbidden().
			Set("user", ctx.TokenPayload().Login()).
			Set("role", ctx.TokenPayload().Role()).
			SetStrs("perms", e.Perms...)
	}

	return errors.Catch(err).
		Set("user", ctx.TokenPayload().Login()).
		Set("role", ctx.TokenPayload().Role()).
		SetStrs("perms", e.Perms...).
		StatusCode(401)
}

// func (wg *WebsocketWrapper) TraverseAll(fn func(*WebsocketConnection) bool) {

// 	wg.mux.RLock()
// 	defer wg.mux.RUnlock()
// 	for i := range wg.conns {
// 		if wg.conns[i].wc == nil {
// 			continue
// 		}
// 		if stop := fn(&wg.conns[i]); stop {
// 			break
// 		}
// 	}
// }

// func (ww *WebsocketWrapper) Traverse(cids []uint64, fn func(*WebsocketConnection)) {
// 	ww.mux.RLock()
// 	defer ww.mux.RUnlock()
// 	for _, cid := range cids {
// 		if idx, ok := ww.idx[cid]; ok {
// 			fn(&ww.conns[idx])
// 		}
// 	}
// }

// func (ww *WebsocketWrapper) Write(cids []uint64, data []byte) {
// 	ww.mux.RLock()
// 	defer ww.mux.RUnlock()
// 	for _, cid := range cids {
// 		if idx, ok := ww.idx[cid]; ok {
// 			ww.conns[idx].wc.Write(data)
// 		}
// 	}
// }

// func (ww *WebsocketWrapper) SetOnCloseCallback(fn func(cid uint64)) {
// 	ww.callbackOnClose = fn
// }

func (wv *WebsocketVatel) UpdateAccessToken(cid uint64, token string) error {
	if wv.cfg.td != nil {
		at, err := wv.cfg.td.Decode([]byte(token))
		if err != nil {
			return err
		}
		if wv.callbackOnTokenUpdate != nil {
			wv.callbackOnTokenUpdate(cid, at.ApplicationPayload())
		}
	}
	return nil
}
