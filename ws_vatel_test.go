package vatel

import (
	"testing"

	"github.com/axkit/errors"
)

func TestDecodeWSInput_Invalid(t *testing.T) {
	type input struct {
		Name string `json:"name"`
	}
	cases := map[string][]byte{
		"malformed":  []byte(`{"name":`),
		"wrong type": []byte(`{"name":42}`),
		"empty":      nil,
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			var in input
			err := decodeWSInput(data, &in)
			if err == nil {
				t.Fatal("expected error")
			}
			if !errors.Is(err, ErrInvalidRequestBody) {
				t.Errorf("want ErrInvalidRequestBody, got %v", err)
			}
			se := errors.Serialize(err, errors.WithAttributes(errors.ServerOutputFormat))
			if se.StatusCode != 400 {
				t.Errorf("want status 400, got %d", se.StatusCode)
			}
			if se.Code != "VTL-0007" {
				t.Errorf("want code VTL-0007, got %q", se.Code)
			}
		})
	}
}

func TestDecodeWSInput_Valid(t *testing.T) {
	type input struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}
	var in input
	if err := decodeWSInput([]byte(`{"name":"bob","age":7}`), &in); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if in.Name != "bob" || in.Age != 7 {
		t.Errorf("want {bob 7}, got %+v", in)
	}
}
