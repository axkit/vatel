package vatel

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"strings"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
)

// ── PathParams[T] ─────────────────────────────────────────────────────────────

func TestPathParams_ImplementsParamer(t *testing.T) {
	type P struct {
		ID int `param:"id"`
	}
	pp := &PathParams[P]{}
	pp.Params.ID = 42

	got := pp.Param()
	if got == nil {
		t.Fatal("Param() returned nil")
	}
	concrete, ok := got.(*P)
	if !ok {
		t.Fatalf("Param() returned %T, want *P", got)
	}
	if concrete.ID != 42 {
		t.Errorf("want ID=42, got %d", concrete.ID)
	}
}

func TestPathParams_ReturnsSamePointer(t *testing.T) {
	type P struct {
		ID int `param:"id"`
	}
	pp := &PathParams[P]{}
	a := pp.Param()
	b := pp.Param()
	if a != b {
		t.Error("Param() must return the same pointer on each call")
	}
}

// ── Body[T] ──────────────────────────────────────────────────────────────────

func TestBody_ImplementsInputer(t *testing.T) {
	type I struct {
		Status string `param:"status"`
		Page   int    `param:"page"`
	}
	b := &Body[I]{}
	b.In.Status = "active"
	b.In.Page = 2

	got := b.Input()
	if got == nil {
		t.Fatal("Input() returned nil")
	}
	concrete, ok := got.(*I)
	if !ok {
		t.Fatalf("Input() returned %T, want *I", got)
	}
	if concrete.Status != "active" || concrete.Page != 2 {
		t.Errorf("unexpected values: %+v", concrete)
	}
}

func TestBody_ReturnsSamePointer(t *testing.T) {
	b := &Body[struct{ X int }]{}
	if b.Input() != b.Input() {
		t.Error("Input() must return the same pointer")
	}
}

// ── QueryParams[T] ───────────────────────────────────────────────────────────

func TestQueryParams_ImplementsQuerierNotInputer(t *testing.T) {
	type Q struct {
		Page int `param:"page"`
	}
	q := &QueryParams[Q]{}
	q.Query.Page = 3

	var qi any = q
	querier, ok := qi.(Querier)
	if !ok {
		t.Fatal("QueryParams must implement Querier")
	}
	got, ok := querier.QueryInput().(*Q)
	if !ok || got.Page != 3 {
		t.Fatalf("QueryInput() = %#v, want *Q with Page=3", querier.QueryInput())
	}
	if querier.QueryInput() != querier.QueryInput() {
		t.Error("QueryInput() must return the same pointer")
	}
	if _, ok := qi.(Inputer); ok {
		t.Error("QueryParams must not implement Inputer")
	}
}

// ── Output[T] ────────────────────────────────────────────────────────────────

func TestOutput_AlwaysReturnsPointer(t *testing.T) {
	type O struct{ Message string }
	out := &Output[O]{}
	if out.Result() == nil {
		t.Error("Result() must never return nil for Output[T]")
	}
	out.Out = O{Message: "hello"}
	got := out.Result()
	concrete, ok := got.(*O)
	if !ok {
		t.Fatalf("Result() returned %T, want *O", got)
	}
	if concrete.Message != "hello" {
		t.Errorf("want hello, got %s", concrete.Message)
	}
}

func TestOptionalOutput_NilByDefault(t *testing.T) {
	out := &OptionalOutput[struct{ V int }]{}
	if out.Result() != nil {
		t.Error("Result() should be nil when Out is unset")
	}
}

func TestOptionalOutput_ReturnsSetPointer(t *testing.T) {
	type O struct{ Message string }
	out := &OptionalOutput[O]{}
	val := O{Message: "hello"}
	out.Out = &val
	got := out.Result()
	if got == nil {
		t.Fatal("Result() returned nil after Out was set")
	}
	concrete, ok := got.(*O)
	if !ok {
		t.Fatalf("Result() returned %T, want *O", got)
	}
	if concrete.Message != "hello" {
		t.Errorf("want hello, got %s", concrete.Message)
	}
}

// ── Embedded carriers in a controller struct ──────────────────────────────────

type orderParam struct {
	ID int `param:"id"`
}

type orderInput struct {
	Status string `param:"status"`
}

type orderOutput struct {
	Name string `json:"name"`
}

type getOrderController struct {
	PathParams[orderParam]
	Body[orderInput]
	Output[orderOutput]
}

func (c *getOrderController) Handle(ctx Context) error {
	c.Out = orderOutput{Name: "Order-" + string(rune(c.Params.ID+'0'))}
	return nil
}

func TestEmbeddedCarriers_InterfacesSatisfied(t *testing.T) {
	c := &getOrderController{}

	// Check all three optional interfaces are implemented.
	if _, ok := any(c).(Paramer); !ok {
		t.Error("embedded PathParams should implement Paramer")
	}
	if _, ok := any(c).(Inputer); !ok {
		t.Error("embedded Body should implement Inputer")
	}
	if _, ok := any(c).(Resulter); !ok {
		t.Error("embedded Output should implement Resulter")
	}
}

func TestEmbeddedCarriers_EndToEnd(t *testing.T) {
	v := NewVatel()
	fctx := newGETCtx("/orders/5?status=active")
	fctx.SetUserValue("id", "5")

	ctrl := &getOrderController{}
	run(v, GET("/orders/{id}", func() Handler { return ctrl }), fctx)

	if responseStatus(fctx) != 200 {
		t.Errorf("want 200, got %d", responseStatus(fctx))
	}
	body := fctx.Response.Body()
	if len(body) == 0 {
		t.Fatal("expected response body")
	}
}

// ── Builder functions ─────────────────────────────────────────────────────────

func TestBuilderFunctions(t *testing.T) {
	ctrl := func() Handler { return &plainHandler{} }
	tests := []struct {
		name   string
		ep     Endpoint
		method string
		path   string
	}{
		{"GET", GET("/a", ctrl), "GET", "/a"},
		{"POST", POST("/b", ctrl), "POST", "/b"},
		{"PUT", PUT("/c", ctrl), "PUT", "/c"},
		{"PATCH", PATCH("/d", ctrl), "PATCH", "/d"},
		{"DELETE", DELETE("/e", ctrl), "DELETE", "/e"},
		{"HEAD", HEAD("/f", ctrl), "HEAD", "/f"},
		{"OPTIONS", OPTIONS("/g", ctrl), "OPTIONS", "/g"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.ep.Method != tc.method {
				t.Errorf("Method: want %s, got %s", tc.method, tc.ep.Method)
			}
			if tc.ep.Path != tc.path {
				t.Errorf("Path: want %s, got %s", tc.path, tc.ep.Path)
			}
			if tc.ep.Controller == nil {
				t.Error("Controller must not be nil")
			}
		})
	}
}

func TestWS_Builder(t *testing.T) {
	ctrl := func() WebsocketHandler { return nil }
	ep := WS("/ws", ctrl, WithLogOption(LogSilent))
	if ep.Method != "WS" {
		t.Errorf("want WS method, got %s", ep.Method)
	}
	if ep.LogOptions != LogSilent {
		t.Errorf("LogSilent not applied")
	}
}

// ── EndpointOption helpers ────────────────────────────────────────────────────

func TestEndpointOptions(t *testing.T) {
	ctrl := func() Handler { return &plainHandler{} }

	ep := GET("/x", ctrl,
		Perms("read", "write"),
		WithCompress(),
		WithLogOption(LogFull),
		WithSuccessStatusCode(201),
		WithContentType("text/plain"),
		WithManualStatusCode(),
	)

	if len(ep.Perms) != 2 || ep.Perms[0] != "read" || ep.Perms[1] != "write" {
		t.Errorf("Perms: %v", ep.Perms)
	}
	if !ep.Compress {
		t.Error("Compress should be true")
	}
	if ep.LogOptions != LogFull {
		t.Errorf("LogOptions: %v", ep.LogOptions)
	}
	if ep.SuccessStatusCode != 201 {
		t.Errorf("SuccessStatusCode: want 201, got %d", ep.SuccessStatusCode)
	}
	if ep.ResponseContentType != "text/plain" {
		t.Errorf("ResponseContentType: %s", ep.ResponseContentType)
	}
	if !ep.ManualStatusCode {
		t.Error("ManualStatusCode should be true")
	}
}

// ── Endpoints slice (Endpointer) ──────────────────────────────────────────────

func TestEndpoints_ImplementsEndpointer(t *testing.T) {
	ctrl := func() Handler { return &plainHandler{} }
	list := Endpoints{
		GET("/a", ctrl),
		POST("/b", ctrl),
	}

	got := list.Endpoints()
	if len(got) != 2 {
		t.Errorf("want 2 endpoints, got %d", len(got))
	}
}

func TestEndpoints_UsedWithVatelAdd(t *testing.T) {
	ctrl := func() Handler { return &plainHandler{} }
	v := NewVatel()

	v.Add(Endpoints{
		GET("/a", ctrl),
		GET("/b", ctrl),
	})

	// Built-in GET "/" is always present, plus our 2.
	eps := v.Endpoints()
	if len(eps) < 3 {
		t.Errorf("want at least 3 endpoints, got %d", len(eps))
	}
}

// ── Upload ────────────────────────────────────────────────────────────────────

// uploadMockCtx implements Context for Upload tests — only FormFile and
// SaveMultipartFile are wired; all other methods are no-ops.
type uploadMockCtx struct {
	formFileResult *multipart.FileHeader
	formFileErr    error
	savedPath      string
}

func (c *uploadMockCtx) FormFile(_ string) (*multipart.FileHeader, error) {
	return c.formFileResult, c.formFileErr
}
func (c *uploadMockCtx) SaveMultipartFile(_ *multipart.FileHeader, path string) error {
	c.savedPath = path
	return nil
}
func (c *uploadMockCtx) BodyWriter() io.Writer             { return nil }
func (c *uploadMockCtx) SetContentType([]byte) Context     { return c }
func (c *uploadMockCtx) SetStatusCode(int) Context         { return c }
func (c *uploadMockCtx) SetHeader(_, _ []byte) Context     { return c }
func (c *uploadMockCtx) Header(string) []byte              { return nil }
func (c *uploadMockCtx) RemoteAddr() string                { return "" }
func (c *uploadMockCtx) RequestTime() time.Time            { return time.Time{} }
func (c *uploadMockCtx) RequestBody() []byte               { return nil }
func (c *uploadMockCtx) PathValue(string) string           { return "" }
func (c *uploadMockCtx) QueryValue(string) []byte          { return nil }
func (c *uploadMockCtx) FormValue(string) []byte           { return nil }
func (c *uploadMockCtx) TokenPayload() any                 { return nil }
func (c *uploadMockCtx) SetTokenPayload(any)               {}
func (c *uploadMockCtx) VerboseError() bool                { return false }
func (c *uploadMockCtx) SetVerboseError(bool)              {}
func (c *uploadMockCtx) Set(string, any) Context           { return c }
func (c *uploadMockCtx) Get(string) any                    { return nil }
func (c *uploadMockCtx) VisitUserValues(func([]byte, any)) {}
func (c *uploadMockCtx) Context() context.Context          { return context.Background() }

// makeFileHeader builds a real *multipart.FileHeader containing content.
func makeFileHeader(filename, content string) *multipart.FileHeader {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", filename)
	_, _ = fw.Write([]byte(content))
	mw.Close()
	mr := multipart.NewReader(&buf, mw.Boundary())
	form, _ := mr.ReadForm(1 << 20)
	return form.File["file"][0]
}

func TestUpload_BindFile_SetsHeader(t *testing.T) {
	fh := &multipart.FileHeader{Filename: "test.txt"}
	ctx := &uploadMockCtx{formFileResult: fh}
	u := Upload{Key: "file"}

	if err := u.BindFile(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u.Header != fh {
		t.Error("want Header set after BindFile")
	}
}

func TestUpload_BindFile_Error(t *testing.T) {
	ctx := &uploadMockCtx{formFileErr: errors.New("missing field")}
	u := Upload{Key: "file"}

	if err := u.BindFile(ctx); err == nil {
		t.Error("expected error from BindFile")
	}
	if u.Header != nil {
		t.Error("want Header to remain nil on error")
	}
}

type uploadHandler struct{ Upload }

func (h *uploadHandler) Handle(ctx Context) error { return h.BindFile(ctx) }

// A request without the file field is a client error: 400 VTL-0007 with the
// field name, no alarm.
func TestUpload_BindFile_MissingField_400(t *testing.T) {
	var form bytes.Buffer
	mw := multipart.NewWriter(&form)
	_ = mw.WriteField("other", "x")
	mw.Close()

	for name, setup := range map[string]func(*fasthttp.RequestCtx){
		"multipart without the field": func(fctx *fasthttp.RequestCtx) {
			fctx.Request.Header.SetContentType(mw.FormDataContentType())
			fctx.Request.SetBody(form.Bytes())
		},
		"not a multipart form": func(fctx *fasthttp.RequestCtx) {
			fctx.Request.Header.SetContentType("application/json")
			fctx.Request.SetBody([]byte(`{}`))
		},
	} {
		t.Run(name, func(t *testing.T) {
			fctx := &fasthttp.RequestCtx{}
			fctx.Request.Header.SetMethod("POST")
			fctx.Request.SetRequestURI("/import")
			setup(fctx)
			ala := &mockAlarmer{}
			v := NewVatel(WithAlarmer(ala), WithVerboseError(true))

			run(v, POST("/import", func() Handler { return &uploadHandler{Upload{Key: "file"}} }), fctx)

			if got := responseStatus(fctx); got != fasthttp.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body %s", got, fctx.Response.Body())
			}
			body := string(fctx.Response.Body())
			if !strings.Contains(body, "VTL-0007") || !strings.Contains(body, `"field":"file"`) {
				t.Errorf("body = %s, want VTL-0007 with field file", body)
			}
			if ala.alarmed {
				t.Error("a missing upload field must not raise an alarm")
			}
		})
	}
}

func TestUpload_SaveTo_DelegatesToContext(t *testing.T) {
	fh := &multipart.FileHeader{Filename: "test.txt"}
	ctx := &uploadMockCtx{}
	u := Upload{Key: "file", Header: fh}

	if err := u.SaveTo(ctx, "/tmp/test.txt"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ctx.savedPath != "/tmp/test.txt" {
		t.Errorf("want savedPath=/tmp/test.txt, got %q", ctx.savedPath)
	}
}

func TestUpload_StreamTo(t *testing.T) {
	const content = "hello upload"
	u := Upload{Key: "file", Header: makeFileHeader("data.txt", content)}

	var out bytes.Buffer
	n, err := u.StreamTo(&out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.String() != content {
		t.Errorf("want %q, got %q", content, out.String())
	}
	if n != int64(len(content)) {
		t.Errorf("want n=%d, got %d", len(content), n)
	}
}

func TestUpload_Open_ReturnsReader(t *testing.T) {
	const content = "open test"
	u := Upload{Key: "file", Header: makeFileHeader("data.txt", content)}

	f, err := u.Open()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer f.Close()

	got, _ := io.ReadAll(f)
	if string(got) != content {
		t.Errorf("want %q, got %q", content, string(got))
	}
}
