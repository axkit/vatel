package vatel

import (
	"io"
	"mime/multipart"
)

// PathParams is an embeddable generic type that provides path parameter binding
// for struct-based controllers. Embed it and access parameters via the Params field.
//
//	type GetOrder struct {
//	    api *API
//	    vatel.PathParams[struct {
//	        ID int `param:"id"`
//	    }]
//	}
//	// c.Params.ID
type PathParams[T any] struct {
	Params T
}

// Param implements the Paramer interface for path parameter binding.
func (p *PathParams[T]) Param() interface{} { return &p.Params }

// QueryParams is an embeddable generic type that provides URL query string
// variable binding for struct-based controllers. Embed it and access query
// variables via the Query field. It implements Querier, so the query string is
// decoded for every HTTP method, including POST/PUT/PATCH, and can be combined
// with Body to receive both query variables and a JSON body.
//
//	type ListOrders struct {
//	    api *API
//	    vatel.PathParams[struct{ CustomerID int `param:"customerId"` }]
//	    vatel.QueryParams[struct {
//	        Status string `param:"status"`
//	        Page   int    `param:"page"`
//	    }]
//	}
//	// c.Params.CustomerID, c.Query.Status, c.Query.Page
//
// If T implements Validator, Validate() is called after decoding, before the
// validation of Body input. Declare Validate() on a named type: an anonymous
// struct cannot have methods.
type QueryParams[T any] struct {
	Query T
}

// QueryInput implements the Querier interface for URL query string binding.
func (q *QueryParams[T]) QueryInput() interface{} { return &q.Query }

// Body is an embeddable generic type that implements Inputer. Embed it and
// access input data via the In field. For POST/PUT/PATCH the JSON request body
// is decoded into In; for GET/DELETE/HEAD/OPTIONS the URL query string is decoded into the
// fields of In tagged `param:"name"` (prefer QueryParams for query input).
// The name Body avoids the naming conflict that would arise from an embedded
// type named Input having a promoted method also named Input().
//
//	type CreateOrder struct {
//	    api *API
//	    vatel.Body[struct {
//	        Product  string `json:"product"`
//	        Quantity int    `json:"quantity"`
//	    }]
//	}
//	// c.In.Product, c.In.Quantity
//
// If T implements Validator, Validate() is called after decoding.
type Body[T any] struct {
	In T
}

// Input implements the Inputer interface for query/body binding.
func (b *Body[T]) Input() interface{} { return &b.In }

// Output is an embeddable generic type that provides response serialization
// for struct-based controllers. Assign Out directly — no pointer needed.
// The controller struct is already heap-allocated per request, so Out lives
// inside it at no extra allocation cost.
//
//	type GetOrder struct {
//	    vatel.Output[Order]
//	}
//	func (c *GetOrder) Handle(_ vatel.Context) error {
//	    c.Out = order
//	    return nil
//	}
type Output[T any] struct {
	Out T
}

// Result implements the Resulter interface. Always returns a non-nil pointer
// to Out so the framework serializes the response body.
func (o *Output[T]) Result() interface{} { return &o.Out }

// OptionalOutput is an embeddable generic type for controllers that may or may
// not produce a response body depending on runtime logic. Set Out to a non-nil
// pointer to include a body; leave it nil to produce no body (e.g. 204/304).
//
//	type FindController struct {
//	    vatel.OptionalOutput[SearchResult]
//	}
//	func (c *FindController) Handle(_ vatel.Context) error {
//	    if result, ok := search(); ok {
//	        c.Out = &result
//	    }
//	    return nil
//	}
type OptionalOutput[T any] struct {
	Out *T
}

// Result implements the Resulter interface. Returns nil when Out is unset,
// signalling to the framework that no response body should be written.
func (o *OptionalOutput[T]) Result() interface{} {
	if o.Out == nil {
		return nil
	}
	return o.Out
}

// Upload is an embeddable type that provides multipart file upload handling
// for struct-based controllers. Unlike PathParams/Body, binding is not
// automatic — call BindFile(ctx) at the start of Handle().
//
//	type ImportHandler struct {
//	    vatel.Upload  // Upload: vatel.Upload{Key: "file"}
//	    vatel.Output[ImportResult]
//	}
//
//	func (h *ImportHandler) Handle(ctx vatel.Context) error {
//	    if err := h.BindFile(ctx); err != nil { return err }
//	    f, err := h.Open()
//	    if err != nil { return err }
//	    defer f.Close()
//	    _, err = io.Copy(s3Writer, f)
//	    return err
//	}
type Upload struct {
	Key    string                // multipart form field name
	Header *multipart.FileHeader // populated by BindFile
}

// BindFile reads the uploaded file from the request and stores it in Header.
// Returns an error if the field is missing or the form cannot be parsed.
func (u *Upload) BindFile(ctx Context) error {
	fh, err := ctx.FormFile(u.Key)
	if err != nil {
		return err
	}
	u.Header = fh
	return nil
}

// SaveTo saves the uploaded file to the given filesystem path.
// Must be called after BindFile.
func (u *Upload) SaveTo(ctx Context, path string) error {
	return ctx.SaveMultipartFile(u.Header, path)
}

// Open returns the uploaded file as a multipart.File for streaming.
// The caller is responsible for closing it. Must be called after BindFile.
func (u *Upload) Open() (multipart.File, error) {
	return u.Header.Open()
}

// StreamTo copies the uploaded file to w. Must be called after BindFile.
// Returns the number of bytes written.
func (u *Upload) StreamTo(w io.Writer) (int64, error) {
	f, err := u.Header.Open()
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return io.Copy(w, f)
}

// funcHandler wraps a plain function as a Handler.
type funcHandler struct {
	f func(Context) error
}

func (h *funcHandler) Handle(ctx Context) error { return h.f(ctx) }

// Func wraps a plain function as a per-request Handler factory, suitable for
// simple stateless endpoints that do not need a full controller struct.
//
//	vatel.DELETE("/orders/{id}", vatel.Func(func(ctx vatel.Context) error {
//	    id := ctx.PathValue("id")
//	    return repo.Delete(id)
//	}), vatel.Perms("orders:write"))
func Func(f func(Context) error) func() Handler {
	return func() Handler { return &funcHandler{f: f} }
}
