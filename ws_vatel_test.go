package vatel

import (
	"bytes"
	stderrors "errors"
	"testing"
	"time"

	"github.com/axkit/errors"
	"github.com/rs/zerolog"
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

func TestWsWriteErrorResponse_LogSilent(t *testing.T) {
	cases := []struct {
		name    string
		lo      LogOption
		err     error
		wantLog bool
	}{
		{"silent 4xx", LogSilent, ErrValidationFailed.New(), false},
		{"silent 4xx with other bits", LogSilent | LogExit, ErrInvalidRequestBody.New(), false},
		{"silent 5xx", LogSilent, ErrInternalError.New(), true},
		{"silent plain error is 5xx", LogSilent, stderrors.New("boom"), true},
		{"silent error without status is 5xx", LogSilent, errors.Template("no status").New(), true},
		{"not silent 4xx", LogExit, ErrValidationFailed.New(), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var logBuf, out bytes.Buffer
			zc := zerolog.New(&logBuf).With()
			e := &Endpoint{EndpointMeta: EndpointMeta{Method: "WS", Path: "x"}}

			wsWriteErrorResponse(e, &out, c.lo, false, &zc, time.Now(), c.err)

			if out.Len() == 0 {
				t.Error("error must be sent to the client")
			}
			if got := logBuf.Len() != 0; got != c.wantLog {
				t.Errorf("logged = %v, want %v (%s)", got, c.wantLog, logBuf.String())
			}
		})
	}
}
