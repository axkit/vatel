package vatel

import (
	"bytes"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"reflect"
	"strconv"

	"github.com/axkit/date"
	"github.com/axkit/errors"
	"github.com/axkit/vatel/jsonmask"
	"github.com/rs/zerolog"
	"github.com/valyala/fasthttp"
)

// fieldInfo holds precomputed reflection metadata for a single struct field,
// enabling allocation-free decoding at request time.
type fieldInfo struct {
	indices []int        // field index path supporting nested structs
	tag     string       // param tag value (the query/path key name)
	kind    reflect.Kind // field kind after pointer dereferencing
	isDate  bool         // true for github.com/axkit/date.Date
	isPtr   bool         // true if the field is a pointer type
	mask    string       // mask tag value: "" logs the value, "-" omits it, any other masks it
}

// logParam adds the path or query parameter val to zc under its tag, honouring
// the field's mask tag: "-" omits it, any other non-empty value logs
// jsonmask.DefaultMask. Masking functions are not applied to parameters.
func logParam(zc zerolog.Context, fi *fieldInfo, val string) zerolog.Context {
	switch fi.mask {
	case "":
		return zc.Str(fi.tag, val)
	case "-":
		return zc
	default:
		return zc.Str(fi.tag, jsonmask.DefaultMask)
	}
}

// paramError returns the error of a param value that cannot be parsed. The
// parse error quotes the value, so it is wrapped only for an unmasked param:
// a masked value must reach neither the log nor a verbose response.
func paramError(fi *fieldInfo, err error) error {
	if fi.mask != "" {
		return ErrValidationFailed.New().Set("param", fi.tag).Msg("invalid param value")
	}
	return ErrValidationFailed.Wrap(err).Set("param", fi.tag)
}

// setParam parses the path or query parameter val into the field sf according
// to fi. kind names the param source ("path" or "query") in errors.
func setParam(sf reflect.Value, fi *fieldInfo, val, kind string) error {
	if fi.isPtr {
		if sf.IsNil() {
			sf.Set(reflect.New(sf.Type().Elem()))
		}
		sf = sf.Elem()
	}

	if fi.isDate {
		d, err := date.Parse(val)
		if err != nil {
			return paramError(fi, err)
		}
		sf.SetUint(uint64(d))
		return nil
	}

	switch fi.kind {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			return paramError(fi, err)
		}
		sf.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(val, 10, 64)
		if err != nil {
			return paramError(fi, err)
		}
		sf.SetUint(n)
	case reflect.String:
		sf.SetString(val)
	case reflect.Float32:
		f, err := strconv.ParseFloat(val, 32)
		if err != nil {
			return paramError(fi, err)
		}
		sf.SetFloat(f)
	case reflect.Float64:
		f, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return paramError(fi, err)
		}
		sf.SetFloat(f)
	case reflect.Bool:
		b, err := strconv.ParseBool(val)
		if err != nil {
			return paramError(fi, err)
		}
		sf.SetBool(b)
	default:
		return ErrValidationFailed.New().
			Msg("unsupported "+kind+" param type").
			Set("param", fi.tag).
			Set("kind", fi.kind.String())
	}
	return nil
}

// buildParamFields reflects on the value returned by Param() and returns
// precomputed field metadata. It uses the query layout, so path params support
// the same field types as query params (pointers, date.Date, nested structs).
func buildParamFields(param interface{}) ([]fieldInfo, error) {
	return buildQueryFields(param, "Param()")
}

// buildQueryFields reflects on in, the value returned by QueryInput() or
// Input() (named by method in errors), and returns precomputed field metadata,
// flattening nested structs. A mask tag on a nested struct applies to its
// fields that have no mask tag of their own. in must be a non-nil pointer to a
// struct. A nested struct whose type is already being flattened (a cycle such
// as a Parent *Filter field) is skipped.
func buildQueryFields(in interface{}, method string) ([]fieldInfo, error) {
	t := reflect.TypeOf(in)
	if t == nil || t.Kind() != reflect.Ptr || t.Elem().Kind() != reflect.Struct {
		return nil, fmt.Errorf("%s must return a pointer to a struct, got %v", method, t)
	}
	var fields []fieldInfo
	flattenQueryFields(t.Elem(), nil, "", map[reflect.Type]bool{}, &fields)
	return fields, nil
}

var dateType = reflect.TypeOf(date.Date(0))

func flattenQueryFields(t reflect.Type, path []int, parentMask string, visiting map[reflect.Type]bool, fields *[]fieldInfo) {
	visiting[t] = true
	defer delete(visiting, t)
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		mask := f.Tag.Get("mask")
		if mask == "" {
			mask = parentMask
		}

		ft := f.Type
		isPtr := ft.Kind() == reflect.Ptr
		if isPtr {
			ft = ft.Elem()
		}

		indices := make([]int, len(path)+1)
		copy(indices, path)
		indices[len(path)] = i

		if ft == dateType {
			tag := f.Tag.Get("param")
			if tag != "" {
				*fields = append(*fields, fieldInfo{
					indices: indices,
					tag:     tag,
					isDate:  true,
					isPtr:   isPtr,
					mask:    mask,
				})
			}
			continue
		}

		if ft.Kind() == reflect.Struct {
			// Recurse into nested struct (embedded or named field without a tag).
			if !visiting[ft] {
				flattenQueryFields(ft, indices, mask, visiting, fields)
			}
			continue
		}

		tag := f.Tag.Get("param")
		if tag == "" {
			continue
		}

		*fields = append(*fields, fieldInfo{
			indices: indices,
			tag:     tag,
			kind:    ft.Kind(),
			isPtr:   isPtr,
			mask:    mask,
		})
	}
}

// fieldByIndices navigates a reflect.Value along a precomputed index path,
// allocating pointer fields as needed.
func fieldByIndices(v reflect.Value, indices []int) reflect.Value {
	for _, idx := range indices {
		if v.Kind() == reflect.Ptr {
			if v.IsNil() {
				v.Set(reflect.New(v.Type().Elem()))
			}
			v = v.Elem()
		}
		v = v.Field(idx)
	}
	return v
}

// decodeParams fills path parameters into the struct pointed to by param
// using precomputed field infos. Path params arrive as fasthttp user values.
// Each param is added to zc, masked by its mask tag; this is the only place
// path params are logged.
func decodeParams(fctx *fasthttp.RequestCtx, param interface{}, fields []fieldInfo, zc zerolog.Context) (zerolog.Context, error) {
	s := reflect.ValueOf(param).Elem()

	for i := range fields {
		fi := &fields[i]
		uv := fctx.UserValue(fi.tag)
		if uv == nil {
			return zc, ErrInternalError.New().Set("param", fi.tag).Msg("path param not found")
		}
		val, ok := uv.(string)
		if !ok {
			return zc, ErrInternalError.New().Set("param", fi.tag).Msg("path param is not a string")
		}
		zc = logParam(zc, fi, val)
		if err := setParam(fieldByIndices(s, fi.indices), fi, val, "path"); err != nil {
			return zc, err
		}
	}
	return zc, nil
}

// decodeURLQuery fills query string parameters into the struct pointed to by input
// using precomputed field infos. Each variable is added to zc, masked by its
// mask tag.
func decodeURLQuery(fctx *fasthttp.RequestCtx, input interface{}, fields []fieldInfo, zc zerolog.Context) (zerolog.Context, error) {
	s := reflect.ValueOf(input).Elem()

	for i := range fields {
		fi := &fields[i]
		val := fctx.QueryArgs().Peek(fi.tag)
		zc = logParam(zc, fi, string(val))
		if val == nil {
			continue
		}
		if err := setParam(fieldByIndices(s, fi.indices), fi, string(val), "query"); err != nil {
			return zc, err
		}
	}
	return zc, nil
}

// decodeBody unmarshals the request body JSON into dest. A body that is empty
// or contains only whitespace is rejected with ErrInvalidRequestBody unless
// optional is true, in which case dest is left untouched and decoded is false.
// masked reports whether the type of dest has mask rules (see jsonDecodeError).
// Validation is a separate step (see validate).
func decodeBody(body []byte, dest interface{}, optional, masked bool) (decoded bool, err error) {
	if len(bytes.TrimSpace(body)) == 0 {
		if optional {
			return false, nil
		}
		return false, ErrInvalidRequestBody.New().Msg("empty request body, JSON expected")
	}
	if err := json.Unmarshal(body, dest); err != nil {
		return false, jsonDecodeError(err, masked, "request body is not valid JSON")
	}
	return true, nil
}

// jsonDecodeError returns ErrInvalidRequestBody with message msg for the
// json.Unmarshal error err. The text of err may quote the input (a number
// that does not fit the field, a time.Time or ",string" value, a custom
// UnmarshalJSON error), so when the target type has mask rules err is not
// wrapped: a masked value must reach neither the log nor a verbose response.
// Only value-free details are kept then: the field, its Go type and the
// offset of a type mismatch, the offset of a syntax error.
func jsonDecodeError(err error, masked bool, msg string) error {
	if !masked {
		return ErrInvalidRequestBody.Wrap(err).Msg(msg)
	}
	e := ErrInvalidRequestBody.New().Msg(msg)
	var te *json.UnmarshalTypeError
	var se *json.SyntaxError
	switch {
	case stderrors.As(err, &te):
		e = e.Set("field", te.Field).Set("offset", te.Offset)
		if te.Type != nil {
			e = e.Set("type", te.Type.String())
		}
	case stderrors.As(err, &se):
		e = e.Set("offset", se.Offset)
	}
	return e
}

// validate calls Validate() if in implements Validator. A validation error is
// returned as ErrValidationFailed (400, VTL-0006) wrapping it; the fields of
// an *errors.Error in its chain are kept, and so is its code, if it has one.
func validate(in interface{}) error {
	v, ok := in.(Validator)
	if !ok {
		return nil
	}
	err := v.Validate()
	if err == nil {
		return nil
	}
	ve := ErrValidationFailed.Wrap(err)
	if xe := vatelError(err); xe != nil {
		if code := errors.Serialize(xe).Code; code != "" {
			ve = ve.Code(code)
		}
	}
	return ve
}

// Validator is an optional interface for input structs. When QueryInput() or
// Input() returns a value implementing Validator, the framework calls Validate()
// automatically after all input (path, query, body) is decoded and before
// invoking Handle(): first on the Querier input, then on the Inputer input.
// The first non-nil error rejects the request with ErrValidationFailed (400,
// VTL-0006) wrapping it; the code of a returned *errors.Error, if it has one,
// replaces VTL-0006. Declare
// Validate() on a named type: an anonymous struct cannot have methods.
//
// Validate() is not called when the endpoint uses WithOptionalBody and the
// request body is empty: the input stays zero, so a Validate() that fills in
// default values does not run.
type Validator interface {
	Validate() error
}
