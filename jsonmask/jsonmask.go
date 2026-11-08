// Package jsonmask masks sensitive values in JSON documents according to
// struct tags.
//
// A field tagged `mask:"-"` is removed from the document. A field tagged with
// any other non-empty value is replaced by the result of the function
// registered under that name with AddFunc, or by "***" when no such function
// is registered. A tagged field is a leaf: when its type is a struct, a
// pointer or a slice, the whole value is masked or removed, its own fields
// are not visited.
//
// Untagged fields of struct, pointer to struct, slice and array types are
// visited, so tags of nested structures and of array elements are honoured.
// Recursive types are supported. Maps and interface values are not visited.
// Rules are applied only to values present in the document: a field omitted
// by `omitempty` is not added.
package jsonmask

import (
	"reflect"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// DefaultMask replaces values tagged with a function name that is not
// registered with AddFunc.
const DefaultMask = "***"

// Field is a masking rule applied to the JSON value found at a path.
type Field struct {
	name string  // escaped path relative to the parent value; "" — the parent value itself
	tag  string  // non-empty: leaf rule ("-" or a function name)
	obj  *object // non-nil: the value is an object, obj.fields apply to it
	elem *Field  // non-nil: the value is an array, elem applies to each element
}

// Fields holds masking rules of a type, built by RawJsonMask.Fields.
type Fields []Field

// HasRules reports whether masking with these rules can change a document.
func (f Fields) HasRules() bool {
	return len(f) > 0
}

// object holds rules of a struct type. Objects are shared between all
// references to the type, which makes rules of recursive types finite.
type object struct {
	fields Fields
	live   bool // the type has at least one rule, directly or through nested types
}

type RawJsonMask struct {
	// funcname
	fn map[string]func(string) string
}

func New() *RawJsonMask {
	return &RawJsonMask{
		fn: make(map[string](func(string) string)),
	}
}

// AddFunc adds masking function associated with name.
func (jm *RawJsonMask) AddFunc(name string, f func(string) string) {
	jm.fn[name] = f
}

// Fields returns masking rules of the type of src, read from the struct tag
// tag. src may be any value, including nil, a typed nil pointer, a struct, a
// slice or a pointer to a slice; types without tagged fields give empty rules.
func (jm *RawJsonMask) Fields(src interface{}, tag string) Fields {
	t := reflect.TypeOf(src)
	if t == nil {
		return nil
	}
	b := builder{tag: tag, objs: make(map[reflect.Type]*object)}
	r := b.rule(t)
	b.resolve()
	if r == nil || !r.isLive() {
		return nil
	}
	if r.obj != nil {
		return r.obj.fields
	}
	return Fields{*r}
}

// builder collects rules of struct types reachable from a root type.
type builder struct {
	tag  string
	objs map[reflect.Type]*object
	all  []*object
}

func deref(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t
}

// rule returns the rule for a value of type t, or nil when the type cannot
// contain tagged fields.
func (b *builder) rule(t reflect.Type) *Field {
	t = deref(t)
	switch t.Kind() {
	case reflect.Struct:
		return &Field{obj: b.object(t)}
	case reflect.Slice, reflect.Array:
		if e := b.rule(t.Elem()); e != nil {
			return &Field{elem: e}
		}
	}
	return nil
}

func (b *builder) object(t reflect.Type) *object {
	if o, ok := b.objs[t]; ok {
		return o
	}
	o := &object{}
	b.objs[t] = o
	b.all = append(b.all, o)
	b.collect(t, o, "", map[reflect.Type]bool{t: true})
	return o
}

// collect adds rules of the fields of struct type t to o. Fields of embedded
// structs without a json name are promoted, as encoding/json does; inherited
// is the mask tag of such an embedded field, applied to each promoted field
// without a tag of its own.
func (b *builder) collect(t reflect.Type, o *object, inherited string, embedded map[reflect.Type]bool) {
	for i := 0; i < t.NumField(); i++ {
		tf := t.Field(i)
		ft := deref(tf.Type)

		name, ok := jsonName(tf)
		if !ok {
			continue
		}

		tag := tf.Tag.Get(b.tag)
		if tag == "" {
			tag = inherited
		}

		if name == "" { // promoted fields of an embedded struct
			if !embedded[ft] {
				embedded[ft] = true
				b.collect(ft, o, tag, embedded)
				delete(embedded, ft)
			}
			continue
		}

		if tag != "" {
			o.fields = append(o.fields, Field{name: name, tag: tag})
			continue
		}
		if r := b.rule(ft); r != nil {
			r.name = name
			o.fields = append(o.fields, *r)
		}
	}
}

// jsonName returns the escaped JSON key of a struct field, "" for an embedded
// struct whose fields are promoted, and false when encoding/json skips it.
func jsonName(tf reflect.StructField) (string, bool) {
	ft := deref(tf.Type)
	if !tf.IsExported() && !(tf.Anonymous && ft.Kind() == reflect.Struct) {
		return "", false
	}
	tag := tf.Tag.Get("json")
	if tag == "-" {
		return "", false
	}
	name, _, _ := strings.Cut(tag, ",")
	if name != "" {
		return escape(name), true
	}
	if tf.Anonymous && ft.Kind() == reflect.Struct {
		return "", true
	}
	if !tf.IsExported() {
		return "", false
	}
	return escape(tf.Name), true
}

// escape escapes characters that have special meaning in gjson/sjson paths.
func escape(s string) string {
	const special = `\.*?|#@!=<>%:"~`
	if !strings.ContainsAny(s, special) {
		return s
	}
	var sb strings.Builder
	for _, c := range s {
		if strings.ContainsRune(special, c) {
			sb.WriteByte('\\')
		}
		sb.WriteRune(c)
	}
	return sb.String()
}

// resolve marks objects that have rules (directly or through nested types)
// and drops rules that can never change a document.
func (b *builder) resolve() {
	for changed := true; changed; {
		changed = false
		for _, o := range b.all {
			if o.live {
				continue
			}
			for i := range o.fields {
				if o.fields[i].isLive() {
					o.live, changed = true, true
					break
				}
			}
		}
	}
	for _, o := range b.all {
		var live Fields
		for _, f := range o.fields {
			if f.isLive() {
				live = append(live, f)
			}
		}
		o.fields = live
	}
}

func (f *Field) isLive() bool {
	switch {
	case f.tag != "":
		return true
	case f.obj != nil:
		return f.obj.live
	case f.elem != nil:
		return f.elem.isLive()
	}
	return false
}

// Mask returns a copy of src with fields masked according to rules.
func (jm *RawJsonMask) Mask(src []byte, fields Fields) ([]byte, error) {
	dst := make([]byte, len(src))
	copy(dst, src)
	return jm.mask(dst, "", fields)
}

func join(prefix, name string) string {
	switch {
	case prefix == "":
		return name
	case name == "":
		return prefix
	}
	return prefix + "." + name
}

func get(buf []byte, path string) gjson.Result {
	if path == "" {
		return gjson.ParseBytes(buf)
	}
	return gjson.GetBytes(buf, path)
}

func (jm *RawJsonMask) mask(buf []byte, prefix string, fields Fields) ([]byte, error) {
	var err error
	for i := range fields {
		if buf, err = jm.apply(buf, join(prefix, fields[i].name), &fields[i]); err != nil {
			return nil, err
		}
	}
	return buf, nil
}

// apply applies rule f to the value at path.
func (jm *RawJsonMask) apply(buf []byte, path string, f *Field) ([]byte, error) {
	v := get(buf, path)
	if !v.Exists() {
		return buf, nil
	}
	var err error
	switch {
	case f.tag == "-":
		return sjson.DeleteBytes(buf, path)
	case f.tag != "":
		mv := DefaultMask
		if fn, ok := jm.fn[f.tag]; ok {
			mv = fn(v.String())
		}
		return sjson.SetBytes(buf, path, mv)
	case f.obj != nil:
		if v.IsObject() {
			return jm.mask(buf, path, f.obj.fields)
		}
	case f.elem != nil:
		if !v.IsArray() {
			return buf, nil
		}
		n := len(v.Array())
		for j := 0; j < n; j++ {
			if buf, err = jm.apply(buf, join(path, strconv.Itoa(j)), f.elem); err != nil {
				return nil, err
			}
		}
	}
	return buf, nil
}
