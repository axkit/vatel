package vatel

import (
	stderrors "errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/axkit/date"
	"github.com/axkit/errors"
	"github.com/fasthttp/router"
	"github.com/rs/zerolog"
	"github.com/valyala/fasthttp"
)

// ── decodeURLQuery ────────────────────────────────────────────────────────────

func TestDecodeURLQuery_BasicTypes(t *testing.T) {
	ctx := fasthttp.RequestCtx{}

	type AA struct {
		G float64 `param:"g"`
	}
	a := struct {
		ID          int       `param:"id"`
		DeletedOnly bool      `param:"deletedOnly"`
		Day         date.Date `param:"day"`
		BB          AA
		B           struct {
			G float64 `param:"g"`
		}
	}{}

	ctx.QueryArgs().Add("id", "1")
	ctx.QueryArgs().Add("deletedOnly", "true")
	ctx.QueryArgs().Add("day", "2021-09-01")
	ctx.QueryArgs().Add("g", "0.5")

	l := zerolog.New(io.Discard)
	fields := mustFields(t, &a)
	if _, err := decodeURLQuery(&ctx, &a, fields, l.With()); err != nil {
		t.Fatal(err)
	}

	if a.ID != 1 {
		t.Errorf("ID: want 1, got %d", a.ID)
	}
	if !a.DeletedOnly {
		t.Error("DeletedOnly: want true")
	}
	if a.Day != 0x20210901 {
		t.Errorf("Day: want 0x20210901, got %x", a.Day)
	}
	if a.BB.G != 0.5 {
		t.Errorf("BB.G: want 0.5, got %f", a.BB.G)
	}
}

func TestDecodeURLQuery_MissingParam(t *testing.T) {
	ctx := fasthttp.RequestCtx{}

	a := struct {
		ID int `param:"id"`
	}{}

	// Don't add "id" to query — it should be silently skipped (optional params).
	fields := mustFields(t, &a)
	l := zerolog.New(io.Discard)
	if _, err := decodeURLQuery(&ctx, &a, fields, l.With()); err != nil {
		t.Fatalf("unexpected error for missing optional param: %v", err)
	}
	if a.ID != 0 {
		t.Errorf("expected zero value, got %d", a.ID)
	}
}

func TestDecodeURLQuery_InvalidInt(t *testing.T) {
	ctx := fasthttp.RequestCtx{}
	ctx.QueryArgs().Add("id", "not-a-number")

	a := struct {
		ID int `param:"id"`
	}{}

	fields := mustFields(t, &a)
	l := zerolog.New(io.Discard)
	_, err := decodeURLQuery(&ctx, &a, fields, l.With())
	if err == nil {
		t.Error("expected error for invalid int")
	}
}

func TestDecodeURLQuery_Float(t *testing.T) {
	ctx := fasthttp.RequestCtx{}
	ctx.QueryArgs().Add("price", "9.99")

	a := struct {
		Price float64 `param:"price"`
	}{}

	fields := mustFields(t, &a)
	l := zerolog.New(io.Discard)
	if _, err := decodeURLQuery(&ctx, &a, fields, l.With()); err != nil {
		t.Fatal(err)
	}
	if a.Price != 9.99 {
		t.Errorf("want 9.99, got %f", a.Price)
	}
}

func TestDecodeURLQuery_String(t *testing.T) {
	ctx := fasthttp.RequestCtx{}
	ctx.QueryArgs().Add("status", "active")

	a := struct {
		Status string `param:"status"`
	}{}

	fields := mustFields(t, &a)
	l := zerolog.New(io.Discard)
	if _, err := decodeURLQuery(&ctx, &a, fields, l.With()); err != nil {
		t.Fatal(err)
	}
	if a.Status != "active" {
		t.Errorf("want active, got %s", a.Status)
	}
}

// ── decodeParams ─────────────────────────────────────────────────────────────

func TestDecodeParams_Int(t *testing.T) {
	ctx := &fasthttp.RequestCtx{}
	ctx.SetUserValue("id", "42")

	type p struct {
		ID int `param:"id"`
	}
	var param p
	fields := mustFields(t, &param)

	l := zerolog.New(io.Discard)
	if _, err := decodeParams(ctx, &param, fields, l.With()); err != nil {
		t.Fatal(err)
	}
	if param.ID != 42 {
		t.Errorf("want 42, got %d", param.ID)
	}
}

func TestDecodeParams_String(t *testing.T) {
	ctx := &fasthttp.RequestCtx{}
	ctx.SetUserValue("slug", "hello-world")

	type p struct {
		Slug string `param:"slug"`
	}
	var param p
	fields := mustFields(t, &param)

	l := zerolog.New(io.Discard)
	if _, err := decodeParams(ctx, &param, fields, l.With()); err != nil {
		t.Fatal(err)
	}
	if param.Slug != "hello-world" {
		t.Errorf("want hello-world, got %s", param.Slug)
	}
}

func TestDecodeParams_MissingValue(t *testing.T) {
	ctx := &fasthttp.RequestCtx{}
	// "id" not set as user value

	type p struct {
		ID int `param:"id"`
	}
	var param p
	fields := mustFields(t, &p{})

	l := zerolog.New(io.Discard)
	_, err := decodeParams(ctx, &param, fields, l.With())
	if err == nil {
		t.Error("expected error for missing path param")
	}
}

func TestDecodeParams_InvalidInt(t *testing.T) {
	ctx := &fasthttp.RequestCtx{}
	ctx.SetUserValue("id", "abc")

	type p struct {
		ID int `param:"id"`
	}
	var param p
	fields := mustFields(t, &param)

	l := zerolog.New(io.Discard)
	_, err := decodeParams(ctx, &param, fields, l.With())
	if err == nil {
		t.Error("expected error for invalid int path param")
	}
}

// ── decodeBody ───────────────────────────────────────────────────────────────

func TestDecodeBody_Success(t *testing.T) {
	type in struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}
	var dest in
	if _, err := decodeBody([]byte(`{"name":"Alice","age":30}`), &dest, false, false); err != nil {
		t.Fatal(err)
	}
	if dest.Name != "Alice" || dest.Age != 30 {
		t.Errorf("got %+v", dest)
	}
}

func TestDecodeBody_EmptyBody(t *testing.T) {
	var dest struct{}
	for _, body := range [][]byte{nil, {}, []byte(" \n\t ")} {
		if _, err := decodeBody(body, &dest, false, false); err == nil {
			t.Errorf("body %q: expected error", body)
		}
	}
}

func TestDecodeBody_OptionalEmptyBody(t *testing.T) {
	for _, body := range [][]byte{nil, {}, []byte(" \n\t ")} {
		dest := struct{ Name string }{Name: "keep"}
		decoded, err := decodeBody(body, &dest, true, false)
		if err != nil {
			t.Fatalf("body %q: unexpected error: %v", body, err)
		}
		if decoded {
			t.Errorf("body %q: decoded must be false", body)
		}
		if dest.Name != "keep" {
			t.Errorf("body %q: dest must be untouched, got %+v", body, dest)
		}
	}
}

func TestDecodeBody_InvalidJSON(t *testing.T) {
	var dest struct{ Name string }
	for _, optional := range []bool{false, true} {
		if _, err := decodeBody([]byte(`not json`), &dest, optional, false); err == nil {
			t.Errorf("optional=%v: expected error for invalid JSON", optional)
		}
	}
}

func TestDecodeBody_DoesNotValidate(t *testing.T) {
	var dest validInput
	decoded, err := decodeBody([]byte(`{"name":""}`), &dest, false, false)
	if err != nil {
		t.Fatalf("decodeBody must not call Validate, got %v", err)
	}
	if !decoded {
		t.Error("decoded must be true")
	}
}

// validInput implements Validator and accepts non-empty Name.
type validInput struct {
	Name string `json:"name"`
}

func (v *validInput) Validate() error {
	if v.Name == "" {
		return ErrValidationFailed.New().Msg("name is required")
	}
	return nil
}

func TestDecodeBody_ValidatorSuccess(t *testing.T) {
	if err := validate(&validInput{Name: "Alice"}); err != nil {
		t.Fatal(err)
	}
	// Inputs that do not implement Validator are always valid.
	if err := validate(&struct{ Name string }{}); err != nil {
		t.Fatal(err)
	}
}

func TestDecodeBody_ValidatorFailure(t *testing.T) {
	err := validate(&validInput{})
	if err == nil {
		t.Fatal("expected validation error")
	}
	xe, ok := err.(*errors.Error)
	if !ok {
		t.Fatalf("want *errors.Error, got %T", err)
	}
	if sc := errors.Serialize(xe, errors.WithAttributes(errors.ServerOutputFormat)).StatusCode; sc != 400 {
		t.Errorf("want status 400, got %d", sc)
	}
}

// returnedErrInput returns err from Validate().
type returnedErrInput struct{ err error }

func (r *returnedErrInput) Validate() error { return r.err }

// plainInvalidInput rejects every value with a plain error.
type plainInvalidInput struct{}

func (plainInvalidInput) Validate() error { return stderrors.New("always invalid") }

type plainValidateErrHandler struct{ Body[plainInvalidInput] }

func (h *plainValidateErrHandler) Handle(Context) error { return nil }

// A Validate() error is ErrValidationFailed (400, VTL-0006); an own code of
// the returned error replaces VTL-0006, its fields are kept, its status is
// replaced by 400.
func TestValidate_ErrorCodes(t *testing.T) {
	appErr := errors.Template("order is locked").Code("APP-0042").StatusCode(409)
	tests := []struct {
		name      string
		err       error
		wantCode  string
		wantField string
	}{
		{"plain error", stderrors.New("name is required"), "VTL-0006", ""},
		{"errors.Wrap of a plain error", errors.Wrap(stderrors.New("x"), "bad name"), "VTL-0006", ""},
		{"ErrValidationFailed with a field", ErrValidationFailed.New().Set("reason", "r"), "VTL-0006", "reason"},
		{"own code", appErr.New().Set("orderId", 7), "APP-0042", "orderId"},
		{"own code under fmt.Errorf", fmt.Errorf("check: %w", appErr.New()), "APP-0042", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validate(&returnedErrInput{err: tt.err})
			if !errors.Is(err, ErrValidationFailed) {
				t.Errorf("error %v is not ErrValidationFailed", err)
			}
			se := errors.Serialize(err, errors.WithAttributes(errors.AddFields))
			if se.Code != tt.wantCode || se.StatusCode != 400 || se.Message != "validation failed" {
				t.Errorf("got code %q status %d msg %q, want %q 400 \"validation failed\"", se.Code, se.StatusCode, se.Message, tt.wantCode)
			}
			if tt.wantField != "" {
				if _, ok := se.Fields[tt.wantField]; !ok {
					t.Errorf("field %q lost: %v", tt.wantField, se.Fields)
				}
			}
		})
	}

	// Through the pipeline: a plain Validate() error gives the client 400
	// VTL-0006.
	fctx := newPOSTCtx("/items", []byte(`{}`))
	run(NewVatel(), POST("/items", func() Handler { return &plainValidateErrHandler{} }), fctx)
	if got := responseStatus(fctx); got != 400 || !strings.Contains(string(fctx.Response.Body()), `"code":"VTL-0006"`) {
		t.Errorf("pipeline: status %d body %s, want 400 VTL-0006", got, fctx.Response.Body())
	}
}

// ── buildQueryFields / buildParamFields ───────────────────────────────────────

func TestBuildQueryFields_CountsTaggedFields(t *testing.T) {
	type input struct {
		Tagged   int    `param:"id"`
		Untagged string // no param tag — must be skipped
		Nested   struct {
			Also int `param:"also"`
		}
	}
	fields := mustFields(t, &input{})
	if len(fields) != 2 {
		t.Errorf("want 2 fields, got %d: %+v", len(fields), fields)
	}
}

func TestBuildParamFields_IgnoresUntagged(t *testing.T) {
	type params struct {
		ID    int    `param:"id"`
		NoTag string // must be skipped
	}
	fields := mustFields(t, &params{})
	if len(fields) != 1 {
		t.Errorf("want 1 field, got %d", len(fields))
	}
	if fields[0].tag != "id" {
		t.Errorf("want tag=id, got %s", fields[0].tag)
	}
}

// ── compile validation ────────────────────────────────────────────────────────

func TestCompile_ParamerPathMismatch(t *testing.T) {
	v := NewVatel()
	// Endpoint has {id} but controller does not implement Paramer.
	e := &Endpoint{
		EndpointMeta: EndpointMeta{Method: "GET", Path: "/items/{id}"},
		Controller:   func() Handler { return &plainHandler{} },
	}
	if err := e.compile(v); err == nil {
		t.Error("expected error: path has param but controller lacks Paramer")
	}
}

func TestCompile_ParamerWithoutPathParam(t *testing.T) {
	v := NewVatel()
	e := &Endpoint{
		EndpointMeta: EndpointMeta{Method: "GET", Path: "/items"},
		Controller:   func() Handler { return &pathHandler{} },
	}
	if err := e.compile(v); err == nil {
		t.Error("expected error: controller implements Paramer but path has no params")
	}
}

func TestCompile_UnknownHTTPMethod(t *testing.T) {
	v := NewVatel()
	e := &Endpoint{
		EndpointMeta: EndpointMeta{Method: "TRACE", Path: "/items"},
		Controller:   func() Handler { return &plainHandler{} },
	}
	if err := e.compile(v); err == nil {
		t.Error("expected error for unknown HTTP method")
	}
}

func TestCompile_PermsFieldIsWsOnly(t *testing.T) {
	// Endpoint.Perms is resolved only by compileWS for WebSocket endpoints.
	// HTTP endpoints carry no compile-time perm resolution; auth is middleware.
	v := NewVatel()
	e := &Endpoint{
		EndpointMeta: EndpointMeta{Method: "GET", Path: "/secret", Perms: []string{"admin"}},
		Controller:   func() Handler { return &plainHandler{} },
	}
	// compile must succeed regardless of Perms — HTTP auth is middleware-driven.
	if err := e.compile(v); err != nil {
		t.Fatalf("unexpected compile error: %v", err)
	}
}

// ── parseLogLevelHeader ──────────────────────────────────────────────────────

func TestParseLogLevelHeader(t *testing.T) {
	tests := []struct {
		input string
		want  LogOption
	}{
		{"silent", LogSilent},
		{"enter", LogEnter},
		{"exit", LogExit},
		{"enter,exit", LogEnter | LogExit},
		{"req-body, resp-body", LogReqBody | LogRespBody},
		{"unknown", 0},
		{"", 0},
	}

	for _, tc := range tests {
		got := parseLogLevelHeader([]byte(tc.input))
		if got != tc.want {
			t.Errorf("input %q: want %b, got %b", tc.input, tc.want, got)
		}
	}
}

// ── middlewareSet size ────────────────────────────────────────────────────────

// mustFields returns the query field layout of in, a pointer to a struct.
func mustFields(t *testing.T, in any) []fieldInfo {
	t.Helper()
	fields, err := buildQueryFields(in, "Input()")
	if err != nil {
		t.Fatal(err)
	}
	return fields
}

// ── BuildHandlers: router panics become errors ───────────────────────────────

func TestBuildHandlers_RouterConflicts_ReturnError(t *testing.T) {
	cases := []struct {
		name string
		eps  Endpoints
	}{
		{"path without leading slash", Endpoints{GET("orders", Func(func(Context) error { return nil }))}},
		{"duplicate route", Endpoints{
			GET("/a", Func(func(Context) error { return nil })),
			GET("/a", Func(func(Context) error { return nil })),
		}},
		{"own GET /", Endpoints{GET("/", Func(func(Context) error { return nil }))}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := NewVatel()
			v.Add(tc.eps)
			mux := router.New()
			l := zerolog.Nop()

			var err error
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("BuildHandlers panicked: %v", r)
					}
				}()
				err = v.BuildHandlers(mux, &l)
			}()
			if err == nil {
				t.Fatal("BuildHandlers: want an error")
			}
			if err2 := v.BuildHandlers(mux, &l); err2 == nil || !strings.Contains(err2.Error(), "previous build failed") {
				t.Errorf("second BuildHandlers = %v, want 'previous build failed'", err2)
			}
		})
	}
}

// ── BuildHandlers: nil controller factories ──────────────────────────────────

func TestBuildHandlers_NilController_ReturnsError(t *testing.T) {
	cases := []struct {
		name string
		opts []func(*Option)
		ep   Endpoint
	}{
		{"nil HTTP factory", nil, GET("/x", nil)},
		{"HTTP factory returning nil", nil, GET("/x", func() Handler { return nil })},
		{"HTTP factory returning a typed nil", nil, GET("/x", func() Handler { return (*plainHandler)(nil) })},
		{"Func(nil)", nil, GET("/x", Func(nil))},
		{"nil WS factory", []func(*Option){WithWebsocketSupport(NewWebsocketVatel())}, WS("x", nil)},
		{"WS factory returning a typed nil", []func(*Option){WithWebsocketSupport(NewWebsocketVatel())},
			WS("x", func() WebsocketHandler { return (*plainWsHandler)(nil) })},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := NewVatel(tc.opts...)
			v.Add(Endpoints{tc.ep})
			l := zerolog.Nop()
			var err error
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("BuildHandlers panicked: %v", r)
					}
				}()
				err = v.BuildHandlers(router.New(), &l)
			}()
			if err == nil || !strings.Contains(err.Error(), "x") {
				t.Fatalf("BuildHandlers = %v, want an error naming the endpoint", err)
			}
		})
	}
}

// ── BuildHandlers: unsupported param and query input types ──────────────────

type mapQueryHandler struct {
	QueryParams[map[string]string]
}

func (h *mapQueryHandler) Handle(Context) error { return nil }

type mapGetInputHandler struct {
	Body[map[string]string]
}

func (h *mapGetInputHandler) Handle(Context) error { return nil }

type intParamHandler struct{ id int }

func (h *intParamHandler) Param() any           { return &h.id }
func (h *intParamHandler) Handle(Context) error { return nil }

type nilParamHandler struct{}

func (h *nilParamHandler) Param() any           { return nil }
func (h *nilParamHandler) Handle(Context) error { return nil }

func TestBuildHandlers_NonStructParamInput_ReturnsError(t *testing.T) {
	cases := map[string]Endpoint{
		"QueryParams of a map": GET("/q", func() Handler { return &mapQueryHandler{} }),
		"GET Body of a map":    GET("/q", func() Handler { return &mapGetInputHandler{} }),
		"Param of an int":      GET("/p/{id}", func() Handler { return &intParamHandler{} }),
		"nil Param":            GET("/p/{id}", func() Handler { return &nilParamHandler{} }),
	}
	for name, e := range cases {
		t.Run(name, func(t *testing.T) {
			v := NewVatel()
			v.Add(Endpoints{e})
			var err error
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("BuildHandlers panicked: %v", r)
					}
				}()
				err = v.BuildHandlers(router.New(), discardLogger())
			}()
			if err == nil {
				t.Fatal("BuildHandlers = nil, want an error")
			}
			if !strings.Contains(err.Error(), "struct") {
				t.Errorf("error = %v, want it to name the struct requirement", err)
			}
		})
	}
}

type cyclicFilter struct {
	Name   string        `param:"name"`
	Parent *cyclicFilter // ignored: a cycle
	Range  struct {
		Min int `param:"min"`
	}
}

type cyclicQueryHandler struct {
	QueryParams[cyclicFilter]
}

func (h *cyclicQueryHandler) Handle(Context) error { return nil }

func TestBuildHandlers_CyclicQueryType(t *testing.T) {
	h := &cyclicQueryHandler{}
	fctx, _, _ := serveLogged(t, NewVatel(), Endpoints{
		GET("/q", func() Handler { return h }, WithLogOption(LogExit)),
	}, "GET", "/q?name=a&min=3")
	if got := responseStatus(fctx); got != 200 {
		t.Fatalf("status = %d, want 200: %s", got, fctx.Response.Body())
	}
	if h.Query.Name != "a" || h.Query.Range.Min != 3 || h.Query.Parent != nil {
		t.Errorf("query = %+v, want name a, min 3 and no parent", h.Query)
	}
}
