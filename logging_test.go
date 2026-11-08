package vatel

import (
	"bytes"
	"encoding/json"
	"errors"
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
func serveLogged(t *testing.T, v *Vatel, eps Endpoints, method, uri string) (*fasthttp.RequestCtx, []map[string]interface{}, string) {
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

	var lines []map[string]interface{}
	for _, raw := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		if len(raw) == 0 {
			continue
		}
		var m map[string]interface{}
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
		{"error", LogExit, errors.New("boom"), 500},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &secretPathHandler{fail: tc.fail}
			fctx, lines, raw := serveLogged(t, noAuthVatel(), Endpoints{
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

func TestLog_PathParamDecodeError_Masked(t *testing.T) {
	h := &secretPathHandler{}
	fctx, lines, raw := serveLogged(t, noAuthVatel(), Endpoints{
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

func (h *secretLegacyQueryHandler) Input() interface{} { return &h.in }
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
		{"Querier error", querierCtrl, errors.New("boom"), 500},
		{"Inputer exit", legacyCtrl, nil, 200},
		{"Inputer error", legacyCtrl, errors.New("boom"), 500},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, seen := tc.ctrl(tc.fail)
			fctx, lines, raw := serveLogged(t, noAuthVatel(), Endpoints{
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
	_, lines, raw := serveLogged(t, noAuthVatel(), Endpoints{
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
	_, lines, raw := serveLogged(t, noAuthVatel(), Endpoints{
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
