package vatel

import (
	"io"
	"reflect"
	"testing"

	"github.com/axkit/date"
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
	fields := buildQueryFields(reflect.TypeOf(a))
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
	fields := buildQueryFields(reflect.TypeOf(a))
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

	fields := buildQueryFields(reflect.TypeOf(a))
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

	fields := buildQueryFields(reflect.TypeOf(a))
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

	fields := buildQueryFields(reflect.TypeOf(a))
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
	fields := buildParamFields(reflect.TypeOf(param))

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
	fields := buildParamFields(reflect.TypeOf(param))

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
	fields := buildParamFields(reflect.TypeOf(p{}))

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
	fields := buildParamFields(reflect.TypeOf(param))

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
	if err := decodeBody([]byte(`{"name":"Alice","age":30}`), &dest); err != nil {
		t.Fatal(err)
	}
	if dest.Name != "Alice" || dest.Age != 30 {
		t.Errorf("got %+v", dest)
	}
}

func TestDecodeBody_EmptyBody(t *testing.T) {
	var dest struct{}
	if err := decodeBody(nil, &dest); err == nil {
		t.Error("expected error for nil body")
	}
	if err := decodeBody([]byte{}, &dest); err == nil {
		t.Error("expected error for empty body")
	}
}

func TestDecodeBody_InvalidJSON(t *testing.T) {
	var dest struct{ Name string }
	if err := decodeBody([]byte(`not json`), &dest); err == nil {
		t.Error("expected error for invalid JSON")
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
	var dest validInput
	if err := decodeBody([]byte(`{"name":"Alice"}`), &dest); err != nil {
		t.Fatal(err)
	}
}

func TestDecodeBody_ValidatorFailure(t *testing.T) {
	var dest validInput
	if err := decodeBody([]byte(`{"name":""}`), &dest); err == nil {
		t.Error("expected validation error")
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
	fields := buildQueryFields(reflect.TypeOf(input{}))
	if len(fields) != 2 {
		t.Errorf("want 2 fields, got %d: %+v", len(fields), fields)
	}
}

func TestBuildParamFields_IgnoresUntagged(t *testing.T) {
	type params struct {
		ID    int    `param:"id"`
		NoTag string // must be skipped
	}
	fields := buildParamFields(reflect.TypeOf(params{}))
	if len(fields) != 1 {
		t.Errorf("want 1 field, got %d", len(fields))
	}
	if fields[0].tag != "id" {
		t.Errorf("want tag=id, got %s", fields[0].tag)
	}
}

// ── compile validation ────────────────────────────────────────────────────────

func TestCompile_ParamerPathMismatch(t *testing.T) {
	v := noAuthVatel()
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
	v := noAuthVatel()
	e := &Endpoint{
		EndpointMeta: EndpointMeta{Method: "GET", Path: "/items"},
		Controller:   func() Handler { return &pathHandler{} },
	}
	if err := e.compile(v); err == nil {
		t.Error("expected error: controller implements Paramer but path has no params")
	}
}

func TestCompile_UnknownHTTPMethod(t *testing.T) {
	v := noAuthVatel()
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

func TestMiddlewareSetSize(t *testing.T) {
	const wantSize = 6
	var ms middlewareSet
	if len(ms) != wantSize {
		t.Errorf("middlewareSet length: want %d, got %d", wantSize, len(ms))
	}
	// Ensure OnErrorResponse (index 5) is accessible without panic.
	_ = ms[OnErrorResponse]
}
