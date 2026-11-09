package vatel

import (
	"bytes"
	"encoding/json"
	stderrors "errors"
	"strings"
	"testing"

	"github.com/axkit/vatel/jsonmask"
	"github.com/fasthttp/router"
	"github.com/rs/zerolog"
	"github.com/valyala/fasthttp"
)

// serveLogged registers endpoints on a real router (path params become user
// values of the request context, as in production), serves one request and
// returns the decoded log lines.
func serveLogged(t *testing.T, v *Vatel, eps Endpoints, method, uri string) (*fasthttp.RequestCtx, []map[string]any, string) {
	t.Helper()
	var buf bytes.Buffer
	l := zerolog.New(&buf)
	v.Add(eps)
	mux := router.New()
	if err := v.BuildHandlers(mux, &l); err != nil {
		t.Fatalf("BuildHandlers: %v", err)
	}
	buf.Reset() // drop "handler registered" lines
	fctx := &fasthttp.RequestCtx{}
	fctx.Request.Header.SetMethod(method)
	fctx.Request.SetRequestURI(uri)
	mux.Handler(fctx)

	var lines []map[string]any
	for _, raw := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		if len(raw) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("log line is not JSON: %v (%s)", err, raw)
		}
		lines = append(lines, m)
	}
	if len(lines) == 0 {
		t.Fatalf("nothing logged")
	}
	return fctx, lines, buf.String()
}

// checkKeyCount reports a raw log line where the JSON key does not occur
// exactly want times.
func checkKeyCount(t *testing.T, raw, key string, want int) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if got := strings.Count(line, `"`+key+`":`); got != want {
			t.Errorf("key %q occurs %d times, want %d: %s", key, got, want, line)
		}
	}
}

type secretPathHandler struct {
	PathParams[struct {
		ID    int    `param:"id"`
		Token string `param:"token" mask:"secret"`
		Code  string `param:"code" mask:"-"`
	}]
	fail error
	seen struct {
		ID          int
		Token, Code string
	}
}

func (h *secretPathHandler) Handle(Context) error {
	h.seen.ID, h.seen.Token, h.seen.Code = h.Params.ID, h.Params.Token, h.Params.Code
	return h.fail
}

func TestLog_PathParamsMasked(t *testing.T) {
	cases := []struct {
		name   string
		lo     LogOption
		fail   error
		status int
	}{
		{"exit", LogExit, nil, 200},
		{"enter and exit", LogEnter | LogExit, nil, 200},
		{"error", LogExit, stderrors.New("boom"), 500},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &secretPathHandler{fail: tc.fail}
			fctx, lines, raw := serveLogged(t, NewVatel(), Endpoints{
				GET("/items/{id}/{token}/{code}", func() Handler { return h }, WithLogOption(tc.lo)),
			}, "GET", "/items/7/tok-path-secret/code-path-secret")

			if got := responseStatus(fctx); got != tc.status {
				t.Fatalf("status = %d, want %d: %s", got, tc.status, fctx.Response.Body())
			}
			// The first line carries the params: the enter line when LogEnter
			// is set, otherwise the exit or error line.
			m := lines[0]
			if m["id"] != "7" {
				t.Errorf("id = %v, want 7: %s", m["id"], raw)
			}
			if m["token"] != jsonmask.DefaultMask {
				t.Errorf("token = %v, want %q: %s", m["token"], jsonmask.DefaultMask, raw)
			}
			if _, ok := m["code"]; ok {
				t.Errorf("code must not be logged: %s", raw)
			}
			if strings.Contains(raw, "tok-path-secret") || strings.Contains(raw, "code-path-secret") {
				t.Errorf("secret path param leaked to log: %s", raw)
			}
			checkKeyCount(t, lines0Raw(raw), "id", 1)
			checkKeyCount(t, lines0Raw(raw), "token", 1)
			// With LogEnter the params are on the enter line only.
			if tc.lo&LogEnter != 0 {
				if len(lines) != 2 {
					t.Fatalf("want enter and exit lines: %s", raw)
				}
				for _, k := range []string{"id", "token", "code"} {
					if _, ok := lines[1][k]; ok {
						t.Errorf("exit line must not repeat path param %s: %s", k, raw)
					}
				}
			}

			// The handler sees the real values.
			if h.seen.ID != 7 || h.seen.Token != "tok-path-secret" || h.seen.Code != "code-path-secret" {
				t.Errorf("handler params = %+v, want unmasked values", h.seen)
			}
		})
	}
}

// lines0Raw returns the first raw log line.
func lines0Raw(raw string) string {
	return strings.SplitN(strings.TrimSpace(raw), "\n", 2)[0]
}

// TestLog_PathParamDecodeError_Masked checks the params decoded before the
// failing one; TestLog_MaskedParamParseError_NoLeak checks a failing masked
// param.
func TestLog_PathParamDecodeError_Masked(t *testing.T) {
	h := &secretPathHandler{}
	fctx, lines, raw := serveLogged(t, NewVatel(), Endpoints{
		GET("/items/{token}/{code}/{id}", func() Handler { return h }, WithLogOption(LogExit)),
	}, "GET", "/items/tok-path-secret/code-path-secret/abc")

	if got := responseStatus(fctx); got != 400 {
		t.Fatalf("status = %d, want 400", got)
	}
	if strings.Contains(raw, "tok-path-secret") || strings.Contains(raw, "code-path-secret") {
		t.Errorf("secret path param leaked to log: %s", raw)
	}
	// Decoding stops at the invalid id, so token may be absent, but never open.
	if v, ok := lines[0]["token"]; ok && v != jsonmask.DefaultMask {
		t.Errorf("token = %v, want %q or absent: %s", v, jsonmask.DefaultMask, raw)
	}
}

type secretQueryParamsHandler struct {
	QueryParams[struct {
		Name  string `param:"name"`
		Token string `param:"token" mask:"secret"`
		Code  string `param:"code" mask:"-"`
	}]
	fail error
	seen struct{ Name, Token, Code string }
}

func (h *secretQueryParamsHandler) Handle(Context) error {
	h.seen.Name, h.seen.Token, h.seen.Code = h.Query.Name, h.Query.Token, h.Query.Code
	return h.fail
}

type secretLegacyQueryHandler struct {
	in struct {
		Name  string `param:"name"`
		Token string `param:"token" mask:"secret"`
		Code  string `param:"code" mask:"-"`
	}
	fail error
}

func (h *secretLegacyQueryHandler) Input() any { return &h.in }
func (h *secretLegacyQueryHandler) Handle(Context) error {
	return h.fail
}

type nestedSecretQueryHandler struct {
	QueryParams[struct {
		Name  string `param:"name"`
		Creds struct {
			Token string `param:"token"`
		} `mask:"secret"`
	}]
}

func (h *nestedSecretQueryHandler) Handle(Context) error { return nil }

func TestLog_QueryParamsMasked(t *testing.T) {
	const uri = "/items?name=a&token=tok-query-secret&code=code-query-secret"
	cases := []struct {
		name   string
		ctrl   func(fail error) (Handler, func() (string, string, string))
		fail   error
		status int
	}{
		{"Querier exit", querierCtrl, nil, 200},
		{"Querier error", querierCtrl, stderrors.New("boom"), 500},
		{"Inputer exit", legacyCtrl, nil, 200},
		{"Inputer error", legacyCtrl, stderrors.New("boom"), 500},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, seen := tc.ctrl(tc.fail)
			fctx, lines, raw := serveLogged(t, NewVatel(), Endpoints{
				GET("/items", func() Handler { return h }, WithLogOption(LogExit)),
			}, "GET", uri)

			if got := responseStatus(fctx); got != tc.status {
				t.Fatalf("status = %d, want %d", got, tc.status)
			}
			m := lines[0]
			if m["name"] != "a" {
				t.Errorf("name = %v, want a: %s", m["name"], raw)
			}
			if m["token"] != jsonmask.DefaultMask {
				t.Errorf("token = %v, want %q: %s", m["token"], jsonmask.DefaultMask, raw)
			}
			if _, ok := m["code"]; ok {
				t.Errorf("code must not be logged: %s", raw)
			}
			if strings.Contains(raw, "tok-query-secret") || strings.Contains(raw, "code-query-secret") {
				t.Errorf("secret query param leaked to log: %s", raw)
			}
			checkKeyCount(t, raw, "name", 1)
			checkKeyCount(t, raw, "token", 1)

			name, token, code := seen()
			if name != "a" || token != "tok-query-secret" || code != "code-query-secret" {
				t.Errorf("handler input = %q %q %q, want unmasked values", name, token, code)
			}
		})
	}
}

func querierCtrl(fail error) (Handler, func() (string, string, string)) {
	h := &secretQueryParamsHandler{fail: fail}
	return h, func() (string, string, string) { return h.seen.Name, h.seen.Token, h.seen.Code }
}

func legacyCtrl(fail error) (Handler, func() (string, string, string)) {
	h := &secretLegacyQueryHandler{fail: fail}
	return h, func() (string, string, string) { return h.in.Name, h.in.Token, h.in.Code }
}

func TestLog_QueryParamsMasked_NestedStructTag(t *testing.T) {
	_, lines, raw := serveLogged(t, NewVatel(), Endpoints{
		GET("/items", func() Handler { return &nestedSecretQueryHandler{} }, WithLogOption(LogExit)),
	}, "GET", "/items?name=a&token=tok-nested-secret")

	if strings.Contains(raw, "tok-nested-secret") {
		t.Errorf("secret query param leaked to log: %s", raw)
	}
	if lines[0]["token"] != jsonmask.DefaultMask || lines[0]["name"] != "a" {
		t.Errorf("log = %s, want token masked and name kept", raw)
	}
}

func TestLog_ParamsWithoutMaskTag_Unchanged(t *testing.T) {
	h := &pathAndQueryHandler{}
	_, lines, raw := serveLogged(t, NewVatel(), Endpoints{
		GET("/items/{id}", func() Handler { return h }, WithLogOption(LogExit)),
	}, "GET", "/items/42?page=3")

	if lines[0]["id"] != "42" || lines[0]["page"] != "3" {
		t.Errorf("log = %s, want id=42 and page=3", raw)
	}
	checkKeyCount(t, raw, "id", 1)
	if h.Params.ID != 42 || h.Query.Page != 3 {
		t.Errorf("handler input = %d %d, want 42 3", h.Params.ID, h.Query.Page)
	}
}

type pathAndQueryHandler struct {
	PathParams[struct {
		ID int `param:"id"`
	}]
	QueryParams[struct {
		Page int `param:"page"`
	}]
}

func (h *pathAndQueryHandler) Handle(Context) error { return nil }

func TestPathParamKeys(t *testing.T) {
	cases := []struct {
		path string
		want []string
	}{
		{"/items", nil},
		{"/items/{id}", []string{"id"}},
		{"/a/{x}/b/{y?}", []string{"x", "y"}},
		{"/a/{id:[0-9]{3}}/{file:*}", []string{"id", "file"}},
	}
	for _, tc := range cases {
		got := parsePathParamKeys(tc.path)
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("parsePathParamKeys(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// ── request body log: non-canonical and duplicate keys ───────────────────────

func TestLog_RequestBody_NonCanonicalKeysMasked(t *testing.T) {
	bodies := map[string]string{
		"PascalCase key": `{"name":"a","Password":"SECRET1"}`,
		"duplicate key":  `{"name":"a","password":"x","password":"SECRET2"}`,
		"upper case key": `{"name":"a","PASSWORD":"SECRET3"}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			h := &secretBodyHandler{}
			fctx := newPOSTCtx("/items", []byte(body))
			runLogged(t, NewVatel(), POST("/items", func() Handler { return h },
				WithLogOption(LogFullOnExit)), fctx, &buf)

			if got := responseStatus(fctx); got != 200 {
				t.Fatalf("status = %d, want 200", got)
			}
			if !strings.HasPrefix(h.In.Password, "SECRET") {
				t.Fatalf("handler password = %q, want the secret", h.In.Password)
			}
			// The handler echoes the input; only the log must be masked.
			logged := strings.ReplaceAll(buf.String(), string(fctx.Response.Body()), "")
			if strings.Contains(logged, "SECRET") {
				t.Errorf("secret leaked to log: %s", buf.String())
			}
			m := lastLogLine(t, &buf)
			obj, ok := m["requestBody"].(map[string]any)
			if !ok || obj["password"] != jsonmask.DefaultMask || obj["name"] != "a" {
				t.Errorf("requestBody = %v, want password masked and name kept: %s", m["requestBody"], buf.String())
			}
		})
	}
}

func TestLog_RequestBody_InvalidJSON_WithMaskRules(t *testing.T) {
	var buf bytes.Buffer
	fctx := newPOSTCtx("/items", []byte(`{"password":"SECRET4",`))
	runLogged(t, NewVatel(), POST("/items", func() Handler { return &secretBodyHandler{} },
		WithLogOption(LogExit|LogReqBody)), fctx, &buf)

	if got := responseStatus(fctx); got != 400 {
		t.Fatalf("status = %d, want 400", got)
	}
	if strings.Contains(buf.String(), "SECRET4") {
		t.Errorf("secret leaked to log: %s", buf.String())
	}
	m := lastLogLine(t, &buf)
	if _, ok := m["requestBodyLoggingFailed"]; !ok {
		t.Errorf("requestBodyLoggingFailed missing: %s", buf.String())
	}
}

func TestLog_RequestBody_InvalidJSON_WithoutMaskRules(t *testing.T) {
	var buf bytes.Buffer
	fctx := newPOSTCtx("/items", []byte(`{"title":`))
	runLogged(t, NewVatel(), POST("/items", func() Handler { return &bodyHandler{} },
		WithLogOption(LogExit|LogReqBody)), fctx, &buf)

	m := lastLogLine(t, &buf)
	if _, ok := m["requestBodyLoggingFailed"]; !ok {
		t.Errorf("requestBodyLoggingFailed missing: %s", buf.String())
	}
}

// ── masked path/query params: parse errors ───────────────────────────────────

type secretPINPathHandler struct {
	PathParams[struct {
		PIN int `param:"pin" mask:"secret"`
	}]
}

func (h *secretPINPathHandler) Handle(Context) error { return nil }

type secretPINQueryHandler struct {
	QueryParams[struct {
		PIN  int    `param:"pin" mask:"secret"`
		Date string `param:"d"`
	}]
}

func (h *secretPINQueryHandler) Handle(Context) error { return nil }

func TestLog_MaskedParamParseError_NoLeak(t *testing.T) {
	cases := []struct {
		name, path, uri string
		ctrl            func() Handler
	}{
		{"path", "/p/{pin}", "/p/12SECRET34", func() Handler { return &secretPINPathHandler{} }},
		{"query", "/q", "/q?pin=12SECRET34", func() Handler { return &secretPINQueryHandler{} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fctx, lines, raw := serveLogged(t, NewVatel(WithVerboseError(true)), Endpoints{
				GET(tc.path, tc.ctrl, WithLogOption(LogExit)),
			}, "GET", tc.uri)

			if got := responseStatus(fctx); got != 400 {
				t.Fatalf("status = %d, want 400", got)
			}
			if strings.Contains(raw, "SECRET") {
				t.Errorf("masked param leaked to log: %s", raw)
			}
			if strings.Contains(string(fctx.Response.Body()), "SECRET") {
				t.Errorf("masked param leaked to verbose response: %s", fctx.Response.Body())
			}
			if lines[0]["pin"] != jsonmask.DefaultMask {
				t.Errorf("pin = %v, want %q: %s", lines[0]["pin"], jsonmask.DefaultMask, raw)
			}
		})
	}
}

func TestLog_UnmaskedParamParseError_KeepsCause(t *testing.T) {
	fctx, _, _ := serveLogged(t, NewVatel(WithVerboseError(true)), Endpoints{
		GET("/p/{id}", func() Handler { return &pathHandler{} }, WithLogOption(LogExit)),
	}, "GET", "/p/abc")

	if got := responseStatus(fctx); got != 400 {
		t.Fatalf("status = %d, want 400", got)
	}
	if !strings.Contains(string(fctx.Response.Body()), "abc") {
		t.Errorf("verbose response should carry the parse error of an unmasked param: %s", fctx.Response.Body())
	}
}

// ── masking error paths and custom masker ────────────────────────────────────

// failingMasker builds the default rules but fails to mask.
type failingMasker struct{ *jsonmask.RawJsonMask }

func (failingMasker) Mask([]byte, jsonmask.Fields) ([]byte, error) {
	return nil, stderrors.New("mask failed")
}

func TestLog_MaskingFailed_OnlyErrorLogged(t *testing.T) {
	var buf bytes.Buffer
	fctx := newPOSTCtx("/items", []byte(`{"name":"a","password":"p1"}`))
	runLogged(t, NewVatel(WithJsonMasker(failingMasker{jsonmask.New()})), POST("/items",
		func() Handler { return &secretBodyHandler{} }, WithLogOption(LogFullOnExit)), fctx, &buf)

	if got := responseStatus(fctx); got != 200 {
		t.Fatalf("status = %d, want 200", got)
	}
	if strings.Contains(buf.String(), "p1") {
		t.Errorf("unmasked secret logged after a masking error: %s", buf.String())
	}
	m := lastLogLine(t, &buf)
	for _, key := range []string{"requestBodyMaskingFailed", "reqInputMaskingFailed", "respBodyMaskingFailed"} {
		if m[key] != "mask failed" {
			t.Errorf("%s = %v, want the masking error: %s", key, m[key], buf.String())
		}
	}
	for _, key := range []string{"requestBody", "reqInput", "respBody"} {
		if _, ok := m[key]; ok {
			t.Errorf("%s must not be logged after a masking error", key)
		}
	}
}

func TestLog_WithJsonMasker_CustomFunc(t *testing.T) {
	jm := jsonmask.New()
	jm.AddFunc("secret", func(string) string { return "CUSTOM" })
	var buf bytes.Buffer
	fctx := newPOSTCtx("/items", []byte(`{"name":"a","password":"p1"}`))
	runLogged(t, NewVatel(WithJsonMasker(jm)), POST("/items",
		func() Handler { return &secretBodyHandler{} }, WithLogOption(LogFullOnExit)), fctx, &buf)

	m := lastLogLine(t, &buf)
	for _, key := range []string{"requestBody", "reqInput", "respBody"} {
		if obj, _ := m[key].(map[string]any); obj["password"] != "CUSTOM" {
			t.Errorf("%s = %v, want the custom masker output", key, m[key])
		}
	}
}

// dynamicResultHandler returns an untyped nil from Result() until Handle sets
// out, so its result type is unknown at compile time.
type dynamicResultHandler struct{ out *secretItem }

func (h *dynamicResultHandler) Result() any {
	if h.out == nil {
		return nil
	}
	return h.out
}

func (h *dynamicResultHandler) Handle(Context) error {
	h.out = &secretItem{Name: "a", Password: "p1"}
	return nil
}

func TestLog_DynamicResultMasked(t *testing.T) {
	v := NewVatel()
	e := GET("/items", func() Handler { return &dynamicResultHandler{} }, WithLogOption(LogExit|LogRespBody))
	if err := e.compile(v); err != nil {
		t.Fatal(err)
	}
	if e.resultFieldsByType == nil {
		t.Fatal("result rules must be built per type")
	}
	for i := 0; i < 2; i++ { // the second request uses the cached rules
		var buf bytes.Buffer
		l := zerolog.New(&buf)
		fctx := newGETCtx("/items")
		e.handler(&l)(fctx)
		if strings.Contains(buf.String(), "p1") {
			t.Errorf("secret leaked to log: %s", buf.String())
		}
		if obj, _ := lastLogLine(t, &buf)["respBody"].(map[string]any); obj["password"] != jsonmask.DefaultMask {
			t.Errorf("respBody = %s, want password masked", buf.String())
		}
	}
}

// switchingResultHandler returns a result without mask tags at compile time
// and a result of another type, with a mask tag, at request time.
type switchingResultHandler struct{ out any }

func (h *switchingResultHandler) Result() any { return h.out }

func (h *switchingResultHandler) Handle(Context) error {
	h.out = &secretItem{Name: "a", Password: "p1"}
	return nil
}

// The mask rules of a result follow its type at request time, not the type
// Result() returned at compile time.
func TestLog_ResultTypeChangesAtRequestTime(t *testing.T) {
	v := NewVatel()
	e := GET("/items", func() Handler {
		return &switchingResultHandler{out: &struct {
			Name string `json:"name"`
		}{}}
	}, WithLogOption(LogExit|LogRespBody))
	if err := e.compile(v); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	l := zerolog.New(&buf)
	fctx := newGETCtx("/items")
	e.handler(&l)(fctx)
	if !strings.Contains(string(fctx.Response.Body()), `"password":"p1"`) {
		t.Errorf("client body = %s, want it unmasked", fctx.Response.Body())
	}
	if strings.Contains(buf.String(), "p1") {
		t.Errorf("secret leaked to log: %s", buf.String())
	}
	if obj, _ := lastLogLine(t, &buf)["respBody"].(map[string]any); obj["password"] != jsonmask.DefaultMask {
		t.Errorf("respBody = %s, want password masked", buf.String())
	}
}

// ── log level controls ───────────────────────────────────────────────────────

func TestLog_LevelControls(t *testing.T) {
	logLines := func(v *Vatel, e Endpoint, header string) string {
		if err := e.compile(v); err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		l := zerolog.New(&buf)
		h := e.handler(&l)
		fctx := newGETCtx("/items")
		if header != "" {
			fctx.Request.Header.Set("X-Set-Log-Level", header)
		}
		h(fctx)
		return buf.String()
	}
	ok := func() Handler { return &plainHandler{} }

	t.Run("default logs nothing on success", func(t *testing.T) {
		if out := logLines(NewVatel(), GET("/items", ok), ""); out != "" {
			t.Errorf("logged: %s", out)
		}
	})
	t.Run("WithDefaultLogOption", func(t *testing.T) {
		if out := logLines(NewVatel(WithDefaultLogOption(LogExit)), GET("/items", ok), ""); !strings.Contains(out, "processed") {
			t.Errorf("want an exit line, got %q", out)
		}
	})
	t.Run("header affects one request only", func(t *testing.T) {
		v := NewVatel()
		e := GET("/items", ok)
		if err := e.compile(v); err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		l := zerolog.New(&buf)
		h := e.handler(&l)
		fctx := newGETCtx("/items")
		fctx.Request.Header.Set("X-Set-Log-Level", "exit")
		h(fctx)
		if !strings.Contains(buf.String(), "processed") {
			t.Fatalf("header request not logged: %q", buf.String())
		}
		buf.Reset()
		h(newGETCtx("/items"))
		if buf.Len() != 0 {
			t.Errorf("request without header logged: %s", buf.String())
		}
	})
	t.Run("WithStaticLoggingLevel ignores header", func(t *testing.T) {
		if out := logLines(NewVatel(WithStaticLoggingLevel()), GET("/items", ok), "exit"); out != "" {
			t.Errorf("header honoured under WithStaticLoggingLevel: %s", out)
		}
	})
	t.Run("WithRequestID", func(t *testing.T) {
		out := logLines(NewVatel(WithRequestID()), GET("/items", ok, WithLogOption(LogEnter|LogExit)), "")
		if strings.Count(out, "\n") != 2 {
			t.Fatalf("want enter and exit lines: %s", out)
		}
		checkKeyCount(t, out, "reqId", 1)
		if out := logLines(NewVatel(), GET("/items", ok, WithLogOption(LogExit)), ""); strings.Contains(out, "reqId") {
			t.Errorf("reqId logged without WithRequestID: %s", out)
		}
	})
	t.Run("message user value", func(t *testing.T) {
		out := logLines(NewVatel(), GET("/items", Func(func(ctx Context) error {
			ctx.Set("message", 42) // not a string: must not panic
			return nil
		}), WithLogOption(LogExit)), "")
		if !strings.Contains(out, `"message":"42"`) || strings.Count(out, `"message"`) != 1 {
			t.Errorf("want message 42 once: %s", out)
		}
	})
}

// ── masked body fields: JSON decode errors ───────────────────────────────────

type maskedPINBodyHandler struct {
	Body[struct {
		Name string `json:"name"`
		PIN  int64  `json:"pin" mask:"-"`
	}]
}

func (h *maskedPINBodyHandler) Handle(Context) error { return nil }

type plainPINBodyHandler struct {
	Body[struct {
		PIN int64 `json:"pin"`
	}]
}

func (h *plainPINBodyHandler) Handle(Context) error { return nil }

func TestLog_MaskedBodyDecodeError_NoLeak(t *testing.T) {
	cases := map[string]struct{ body, secret string }{
		"overflow": {`{"pin":99999999999999999999123}`, "99999999999999999999123"},
		"float":    {`{"pin":1234.5678}`, "1234.5678"},
		"string":   {`{"pin":"98765SECRET"}`, "98765SECRET"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			fctx := newPOSTCtx("/pin", []byte(tc.body))
			runLogged(t, NewVatel(WithVerboseError(true)), POST("/pin",
				func() Handler { return &maskedPINBodyHandler{} },
				WithLogOption(LogFullOnExit)), fctx, &buf)

			if got := responseStatus(fctx); got != 400 {
				t.Fatalf("status = %d, want 400", got)
			}
			if strings.Contains(buf.String(), tc.secret) {
				t.Errorf("masked body value leaked to log: %s", buf.String())
			}
			if strings.Contains(string(fctx.Response.Body()), tc.secret) {
				t.Errorf("masked body value leaked to verbose response: %s", fctx.Response.Body())
			}
			if !strings.Contains(buf.String(), `"field":"pin"`) {
				t.Errorf("log must name the field of the type mismatch: %s", buf.String())
			}
			if !strings.Contains(buf.String(), "VTL-0007") {
				t.Errorf("log must carry ErrInvalidRequestBody: %s", buf.String())
			}
		})
	}
}

func TestLog_UnmaskedBodyDecodeError_KeepsCause(t *testing.T) {
	var buf bytes.Buffer
	fctx := newPOSTCtx("/pin", []byte(`{"pin":1234.5678}`))
	runLogged(t, NewVatel(WithVerboseError(true)), POST("/pin",
		func() Handler { return &plainPINBodyHandler{} },
		WithLogOption(LogExit)), fctx, &buf)

	if got := responseStatus(fctx); got != 400 {
		t.Fatalf("status = %d, want 400", got)
	}
	if !strings.Contains(buf.String(), "1234.5678") {
		t.Errorf("the decode error of a body without mask rules should keep its cause: %s", buf.String())
	}
}
