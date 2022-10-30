package vatel

import (
	"io"
	"testing"

	"github.com/axkit/date"
	"github.com/rs/zerolog"
	"github.com/valyala/fasthttp"
)

func TestDecodeURLQuery(t *testing.T) {
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

	if _, err := decodeURLQuery(&ctx, &a, l.With()); err != nil {
		t.Error(err)
	}

	if a.ID != 1 || a.DeletedOnly != true || a.Day != 0x20210901 || a.BB.G != 0.5 {
		t.Errorf("failed. expected: 1, true, 20210901, 0.5 got: %d, %t, %x %.02f", a.ID, a.DeletedOnly, a.Day, a.BB.G)
	}

}
