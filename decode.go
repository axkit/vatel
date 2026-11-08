package vatel

import (
	"encoding/json"
	"reflect"
	"strconv"

	"github.com/axkit/date"
	"github.com/axkit/errors"
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
}

// buildParamFields reflects on a path-param struct type and returns precomputed
// field metadata. Path param structs are expected to be flat (no nesting).
func buildParamFields(t reflect.Type) []fieldInfo {
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	var fields []fieldInfo
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("param")
		if tag == "" {
			continue
		}
		fields = append(fields, fieldInfo{
			indices: []int{i},
			tag:     tag,
			kind:    f.Type.Kind(),
		})
	}
	return fields
}

// buildQueryFields reflects on a query/body input struct type and returns
// precomputed field metadata, flattening nested structs.
func buildQueryFields(t reflect.Type) []fieldInfo {
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	var fields []fieldInfo
	flattenQueryFields(t, nil, &fields)
	return fields
}

var dateType = reflect.TypeOf(date.Date(0))

func flattenQueryFields(t reflect.Type, path []int, fields *[]fieldInfo) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
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
				})
			}
			continue
		}

		if ft.Kind() == reflect.Struct {
			// Recurse into nested struct (embedded or named field without a tag).
			flattenQueryFields(ft, indices, fields)
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
func decodeParams(fctx *fasthttp.RequestCtx, param interface{}, fields []fieldInfo, zc zerolog.Context) (zerolog.Context, error) {
	s := reflect.ValueOf(param).Elem()

	for _, fi := range fields {
		uv := fctx.UserValue(fi.tag)
		if uv == nil {
			return zc, ErrInternalError.New().Set("param", fi.tag).Msg("path param not found")
		}
		val, ok := uv.(string)
		if !ok {
			return zc, ErrInternalError.New().Set("param", fi.tag).Msg("path param is not a string")
		}
		zc = zc.Str(fi.tag, val)

		sf := s.Field(fi.indices[0])

		switch fi.kind {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			n, err := strconv.ParseInt(val, 10, 64)
			if err != nil {
				return zc, ErrValidationFailed.Wrap(err).Set("param", fi.tag)
			}
			sf.SetInt(n)
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			n, err := strconv.ParseUint(val, 10, 64)
			if err != nil {
				return zc, ErrValidationFailed.Wrap(err).Set("param", fi.tag)
			}
			sf.SetUint(n)
		case reflect.String:
			sf.SetString(val)
		case reflect.Bool:
			b, err := strconv.ParseBool(val)
			if err != nil {
				return zc, ErrValidationFailed.Wrap(err).Set("param", fi.tag)
			}
			sf.SetBool(b)
		case reflect.Float32, reflect.Float64:
			f, err := strconv.ParseFloat(val, 64)
			if err != nil {
				return zc, ErrValidationFailed.Wrap(err).Set("param", fi.tag)
			}
			sf.SetFloat(f)
		default:
			return zc, ErrValidationFailed.New().
				Msg("unsupported path param type").
				Set("param", fi.tag).
				Set("kind", fi.kind.String())
		}
	}
	return zc, nil
}

// decodeURLQuery fills query string parameters into the struct pointed to by input
// using precomputed field infos.
func decodeURLQuery(fctx *fasthttp.RequestCtx, input interface{}, fields []fieldInfo, zc zerolog.Context) (zerolog.Context, error) {
	s := reflect.ValueOf(input).Elem()

	for _, fi := range fields {
		val := fctx.QueryArgs().Peek(fi.tag)
		zc = zc.Bytes(fi.tag, val)
		if val == nil {
			continue
		}

		sf := fieldByIndices(s, fi.indices)

		if fi.isDate {
			d, err := date.Parse(string(val))
			if err != nil {
				return zc, ErrValidationFailed.Wrap(err).Set("param", fi.tag)
			}
			sf.SetUint(uint64(d))
			continue
		}

		if fi.isPtr {
			if sf.IsNil() {
				sf.Set(reflect.New(sf.Type().Elem()))
			}
			sf = sf.Elem()
		}

		switch fi.kind {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			n, err := strconv.ParseInt(string(val), 10, 64)
			if err != nil {
				return zc, ErrValidationFailed.Wrap(err).Set("param", fi.tag)
			}
			sf.SetInt(n)
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			n, err := strconv.ParseUint(string(val), 10, 64)
			if err != nil {
				return zc, ErrValidationFailed.Wrap(err).Set("param", fi.tag)
			}
			sf.SetUint(n)
		case reflect.String:
			sf.SetString(string(val))
		case reflect.Float32:
			f, err := strconv.ParseFloat(string(val), 32)
			if err != nil {
				return zc, ErrValidationFailed.Wrap(err).Set("param", fi.tag)
			}
			sf.SetFloat(f)
		case reflect.Float64:
			f, err := strconv.ParseFloat(string(val), 64)
			if err != nil {
				return zc, ErrValidationFailed.Wrap(err).Set("param", fi.tag)
			}
			sf.SetFloat(f)
		case reflect.Bool:
			b, err := strconv.ParseBool(string(val))
			if err != nil {
				return zc, ErrValidationFailed.Wrap(err).Set("param", fi.tag)
			}
			sf.SetBool(b)
		default:
			return zc, ErrValidationFailed.New().
				Msg("unsupported query param type").
				Set("param", fi.tag).
				Set("kind", fi.kind.String())
		}
	}
	return zc, nil
}

// decodeBody unmarshals the request body JSON into dest.
// If dest implements Validator, Validate() is called after successful unmarshal.
func decodeBody(body []byte, dest interface{}) error {
	if len(body) == 0 {
		return ErrInvalidRequestBody.New().Msg("empty request body, JSON expected")
	}
	if err := json.Unmarshal(body, dest); err != nil {
		return ErrInvalidRequestBody.Wrap(err).Msg("request body is not valid JSON")
	}
	if v, ok := dest.(Validator); ok {
		if err := v.Validate(); err != nil {
			return errors.Wrap(err, "validation failed").StatusCode(400)
		}
	}
	return nil
}

// Validator is an optional interface for input structs. When Input() returns
// a value implementing Validator, the framework calls Validate() automatically
// after decoding and before invoking Handle(). Return a non-nil error to reject
// the request with a 400 status.
type Validator interface {
	Validate() error
}
