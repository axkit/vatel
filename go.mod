module github.com/axkit/vatel

go 1.24

toolchain go1.24.1

//replace github.com/axkit/errors => /home/gera/go/src/github.com/axkit/errors
//replace github.com/regorov/websocket => /home/gera/go/src/github.com/regorov/websocket
//replace github.com/axkit/bitset => /Users/gera/go/src/github.com/axkit/bitset

require (
	github.com/axkit/aaa v0.1.0
	github.com/axkit/bitset v1.0.4
	github.com/axkit/date v0.3.1
	github.com/axkit/errors v1.0.2
	github.com/axkit/fasthttp-realip v1.0.1
	github.com/axkit/tinymap v0.0.2
	github.com/fasthttp/router v1.4.4
	github.com/google/uuid v1.3.0
	github.com/regorov/websocket v0.1.3
	github.com/rs/zerolog v1.26.0
	github.com/tidwall/gjson v1.14.2
	github.com/tidwall/sjson v1.2.5
	github.com/valyala/fasthttp v1.31.0
)

require (
	github.com/andybalholm/brotli v1.0.4 // indirect
	github.com/gbrlsnchs/jwt/v3 v3.0.0 // indirect
	github.com/klauspost/compress v1.13.6 // indirect
	github.com/magefile/mage v1.9.0 // indirect
	github.com/savsgio/gotils v0.0.0-20210921075833-21a6215cb0e4 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.0 // indirect
	github.com/valyala/bytebufferpool v1.0.0 // indirect
	golang.org/x/crypto v0.0.0-20210513164829-c07d793c2f9a // indirect
	golang.org/x/xerrors v0.0.0-20200804184101-5ec99f83aff1 // indirect
)
