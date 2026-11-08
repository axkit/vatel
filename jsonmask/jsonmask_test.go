package jsonmask

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func maskEmail(e string) string {
	return "e***"
}

func newMasker() *RawJsonMask {
	jm := New()
	jm.AddFunc("email", maskEmail)
	return jm
}

// maskValue marshals v, masks it with the rules of v's type and returns the
// result decoded into a generic value for comparison.
func maskValue(t *testing.T, jm *RawJsonMask, v any) any {
	t.Helper()
	src, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	res, err := jm.Mask(src, jm.Fields(v, "mask"))
	if err != nil {
		t.Fatalf("mask: %v", err)
	}
	var out any
	if err := json.Unmarshal(res, &out); err != nil {
		t.Fatalf("masked output is not valid JSON: %v (%s)", err, res)
	}
	return out
}

func decode(t *testing.T, s string) any {
	t.Helper()
	var out any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		t.Fatalf("bad expected JSON: %v", err)
	}
	return out
}

func assertJSON(t *testing.T, got any, want string) {
	t.Helper()
	if w := decode(t, want); !reflect.DeepEqual(got, w) {
		gb, _ := json.Marshal(got)
		t.Errorf("got  %s\nwant %s", gb, want)
	}
}

type Node struct {
	Name     string `json:"name"`
	Secret   string `json:"secret" mask:"secret"`
	Children []Node `json:"children"`
	Next     *Node  `json:"next"`
}

type Item struct {
	ID     int    `json:"id"`
	Secret string `json:"secret" mask:"secret"`
}

func TestFields_NoPanic(t *testing.T) {
	jm := newMasker()
	var nilPtr *Item
	var nilSlice *[]Item
	cases := []struct {
		name string
		v    any
		want bool
	}{
		{"nil", nil, false},
		{"string", "hello", false},
		{"string pointer", new(string), false},
		{"int", 42, false},
		{"map", map[string]Item{}, false},
		{"slice of strings", []string{"a"}, false},
		{"recursive", Node{}, true},
		{"recursive pointer", &Node{}, true},
		{"typed nil pointer", nilPtr, true},
		{"typed nil slice pointer", nilSlice, true},
		{"struct without tags", struct{ A int }{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := jm.Fields(c.v, "mask")
			if got := f.HasRules(); got != c.want {
				t.Errorf("HasRules() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestMask_Recursive(t *testing.T) {
	jm := newMasker()
	n := Node{
		Name:     "root",
		Secret:   "s0",
		Children: []Node{{Name: "c", Secret: "s1", Children: []Node{{Name: "cc", Secret: "s3"}}}},
		Next:     &Node{Name: "n", Secret: "s2", Next: &Node{Name: "nn", Secret: "s4"}},
	}
	assertJSON(t, maskValue(t, jm, &n), `{
		"name":"root","secret":"***",
		"children":[{"name":"c","secret":"***","next":null,
			"children":[{"name":"cc","secret":"***","children":null,"next":null}]}],
		"next":{"name":"n","secret":"***","children":null,
			"next":{"name":"nn","secret":"***","children":null,"next":null}}}`)
}

// A and B are mutually recursive; only A has a tagged field, so B has rules
// only through A.
type A struct {
	Secret string `json:"secret" mask:"secret"`
	B      *B     `json:"b,omitempty"`
}

type B struct {
	A *A `json:"a,omitempty"`
}

func TestMask_MutuallyRecursive(t *testing.T) {
	jm := newMasker()
	if !jm.Fields(B{}, "mask").HasRules() {
		t.Fatal("B must have rules through A")
	}
	v := A{Secret: "s", B: &B{A: &A{Secret: "s"}}}
	assertJSON(t, maskValue(t, jm, v), `{"secret":"***","b":{"a":{"secret":"***"}}}`)

	type plain struct {
		Self *plain `json:"self"`
		Name string `json:"name"`
	}
	if jm.Fields(plain{}, "mask").HasRules() {
		t.Error("recursive type without tags must have no rules")
	}
}

func TestMask_RootSlice(t *testing.T) {
	jm := newMasker()
	items := []Item{{ID: 1, Secret: "a"}, {ID: 2, Secret: "b"}}
	want := `[{"id":1,"secret":"***"},{"id":2,"secret":"***"}]`

	t.Run("pointer to slice", func(t *testing.T) {
		assertJSON(t, maskValue(t, jm, &items), want)
	})
	t.Run("slice of pointers", func(t *testing.T) {
		ptrs := []*Item{&items[0], &items[1]}
		assertJSON(t, maskValue(t, jm, ptrs), want)
	})
	t.Run("array", func(t *testing.T) {
		arr := [2]Item{items[0], items[1]}
		assertJSON(t, maskValue(t, jm, arr), want)
	})
	t.Run("empty slice", func(t *testing.T) {
		assertJSON(t, maskValue(t, jm, []Item{}), `[]`)
	})
	t.Run("slice of slices", func(t *testing.T) {
		assertJSON(t, maskValue(t, jm, [][]Item{items}), `[`+want+`]`)
	})
}

type Inner struct {
	Login    string `json:"login"`
	Password string `json:"password" mask:"secret"`
	Email    string `json:"email" mask:"email"`
}

func TestMask_Traversal(t *testing.T) {
	jm := newMasker()
	in := Inner{Login: "bob", Password: "p", Email: "bob@example.com"}
	masked := `{"login":"bob","password":"***","email":"e***"}`

	type withValue struct {
		ID int   `json:"id"`
		In Inner `json:"in"`
	}
	type withPtr struct {
		ID int    `json:"id"`
		In *Inner `json:"in"`
	}
	type withSlice struct {
		ID int     `json:"id"`
		In []Inner `json:"in"`
	}
	type withPtrSlice struct {
		ID int      `json:"id"`
		In []*Inner `json:"in,omitempty"`
	}
	type withEmbedded struct {
		ID int `json:"id"`
		*Inner
	}
	type withAnonymous struct {
		ID int `json:"id"`
		In struct {
			Token string `json:"token" mask:"-"`
			Kept  string `json:"kept"`
		} `json:"in"`
	}

	t.Run("struct", func(t *testing.T) {
		assertJSON(t, maskValue(t, jm, withValue{1, in}), `{"id":1,"in":`+masked+`}`)
	})
	t.Run("pointer to struct", func(t *testing.T) {
		assertJSON(t, maskValue(t, jm, withPtr{1, &in}), `{"id":1,"in":`+masked+`}`)
	})
	t.Run("nil pointer to struct", func(t *testing.T) {
		assertJSON(t, maskValue(t, jm, withPtr{1, nil}), `{"id":1,"in":null}`)
	})
	t.Run("slice of structs", func(t *testing.T) {
		assertJSON(t, maskValue(t, jm, withSlice{1, []Inner{in, in}}), `{"id":1,"in":[`+masked+`,`+masked+`]}`)
	})
	t.Run("slice of pointers", func(t *testing.T) {
		assertJSON(t, maskValue(t, jm, withPtrSlice{1, []*Inner{&in, &in}}), `{"id":1,"in":[`+masked+`,`+masked+`]}`)
	})
	t.Run("omitted slice", func(t *testing.T) {
		assertJSON(t, maskValue(t, jm, withPtrSlice{ID: 1}), `{"id":1}`)
	})
	t.Run("embedded pointer", func(t *testing.T) {
		assertJSON(t, maskValue(t, jm, withEmbedded{1, &in}), `{"id":1,"login":"bob","password":"***","email":"e***"}`)
	})
	t.Run("anonymous struct", func(t *testing.T) {
		v := withAnonymous{ID: 1}
		v.In.Token, v.In.Kept = "t", "k"
		assertJSON(t, maskValue(t, jm, v), `{"id":1,"in":{"kept":"k"}}`)
	})
}

type Credentials struct {
	Login    string `json:"login"`
	Password string `json:"password"`
}

func TestMask_Leaf(t *testing.T) {
	jm := newMasker()
	type leafs struct {
		ID    int         `json:"id"`
		At    time.Time   `json:"at" mask:"secret"`
		Creds Credentials `json:"creds" mask:"-"`
		Ptr   *Inner      `json:"ptr" mask:"secret"`
	}
	v := leafs{
		ID:    1,
		At:    time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Creds: Credentials{"bob", "p"},
		Ptr:   &Inner{Login: "x"},
	}
	assertJSON(t, maskValue(t, jm, v), `{"id":1,"at":"***","ptr":"***"}`)

	// The tag of an embedded struct applies to each of its promoted fields.
	type embeddedTagged struct {
		ID          int `json:"id"`
		Credentials `mask:"-"`
	}
	assertJSON(t, maskValue(t, jm, embeddedTagged{1, Credentials{"bob", "p"}}), `{"id":1}`)
}

func TestMask_AbsentFieldNotAdded(t *testing.T) {
	jm := newMasker()
	type opt struct {
		ID     int    `json:"id"`
		Secret string `json:"secret,omitempty" mask:"secret"`
		Email  string `json:"email,omitempty" mask:"email"`
		Gone   string `json:"gone,omitempty" mask:"-"`
		In     *Inner `json:"in,omitempty"`
	}
	assertJSON(t, maskValue(t, jm, opt{ID: 1}), `{"id":1}`)
}

func TestMask_Rules(t *testing.T) {
	jm := newMasker()
	type rules struct {
		ID       int    `json:"id"`
		Unknown  string `json:"unknown" mask:"no-such-func"`
		Email    string `json:"email" mask:"email"`
		Notice   string `json:"notice,omitempty" mask:"-"`
		Document string `json:"document"`
		NoJSON   string `mask:"secret"`
		Skipped  string `json:"-" mask:"secret"`
		Dotted   string `json:"a.b" mask:"secret"`
		private  string `mask:"secret"`
	}
	v := rules{ID: 1, Unknown: "u", Email: "bob@example.com", Notice: "n", Document: "passport", NoJSON: "x", Skipped: "s", Dotted: "d", private: "p"}
	assertJSON(t, maskValue(t, jm, v),
		`{"id":1,"unknown":"***","email":"e***","document":"passport","NoJSON":"***","a.b":"***"}`)
}

func TestMask_NoRulesKeepsInput(t *testing.T) {
	jm := newMasker()
	src := []byte(`{"a":1}`)
	res, err := jm.Mask(src, jm.Fields(struct{ A int }{}, "mask"))
	if err != nil {
		t.Fatal(err)
	}
	if string(res) != string(src) {
		t.Errorf("got %s, want %s", res, src)
	}
}
