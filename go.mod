module github.com/axkit/vatel

go 1.26.0

require (
	github.com/axkit/bitset v1.0.4
	github.com/axkit/date v1.0.1
	github.com/axkit/errors v1.1.0
	github.com/axkit/fasthttp-realip v1.0.1
	github.com/axkit/tinymap v0.0.2
	github.com/fasthttp/router v1.5.4
	github.com/google/uuid v1.6.0
	github.com/regorov/websocket v0.1.4
	github.com/rs/zerolog v1.26.0
	github.com/tidwall/gjson v1.14.2
	github.com/tidwall/sjson v1.2.5
	github.com/valyala/fasthttp v1.77.0
)

require (
	github.com/klauspost/compress v1.20.3 // indirect
	github.com/molecule-man/go-brrr v1.3.0 // indirect
	github.com/savsgio/gotils v0.0.0-20240704082632-aef3928b8a38 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.0 // indirect
	github.com/valyala/bytebufferpool v1.0.0 // indirect
)

retract v1.24.2 // published by mistake, use v1.25.0 or later
