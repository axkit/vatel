package vatel_test

import (
	"bytes"
	"fmt"
	"os"
	"testing"

	"github.com/axkit/aaa"
	"github.com/axkit/bitset"
	"github.com/axkit/errors"
	"github.com/axkit/vatel"
	"github.com/fasthttp/router"
	"github.com/rs/zerolog"
	"github.com/valyala/fasthttp"
)

var (
	va  *vatel.Vatel
	a   *aaa.BasicAAA
	cfg = aaa.DefaultConfig
)

type User struct {
	id       int
	login    string
	role     int
	isLocked bool
}

func (u *User) UserID() int       { return u.id }
func (u *User) UserLogin() string { return u.login }
func (u *User) UserRole() int     { return u.role }
func (u *User) UserLocked() bool  { return u.isLocked }

type us struct{}

func (*us) UserByCredentials(login, password string) (aaa.Userer, error) {
	if login == "gera" {
		return &User{id: 1, login: "gera", role: 1, isLocked: false}, nil
	}
	return nil, errors.NotFound("unknown user")
}

func (*us) UserByID(userID int) (aaa.Userer, error) {
	if userID == 1 {
		return &User{id: 1, login: "gera", role: 1, isLocked: false}, nil
	}
	return nil, errors.NotFound("unknown user id")
}

type rs struct{}

func (*rs) IsRoleExist(roleID int) bool {
	if roleID == 3 {
		return true
	}
	return false
}

func (*rs) RolePermissions(roleID int) ([]string, bitset.BitSet) {
	if roleID == 3 {
		return []string{"Ping", "Follow"}, *bitset.New(8).Set(1, 2)
	}
	return nil, bitset.BitSet{}
}

var (
	vrs rs
	vus us
)

func init() {
	cfg.EncryptionKey = "hello"
	va = vatel.NewVatel()
	a = aaa.New(cfg, &vus, &vrs)
}

type Ping struct {
	output struct {
		Message string `json:"message"`
	}
}

func (p *Ping) Result() interface{} {
	return &p.output
}

func (p *Ping) Handle(ctx vatel.WebsocketContext) error {
	fmt.Println("aaa")
	return nil
}

type Auth struct {
	va    *vatel.Vatel
	input struct {
		Login    string `json:"login"`
		Password string `json:"password"`
	}

	output *aaa.TokenSet
}

func (p *Auth) Input() interface{} {
	return &p.input
}

func (p *Auth) Result() interface{} {
	return &p.output
}

func (p *Auth) Handle(ctx vatel.Context) error {
	ts, err := a.SignIn(p.input.Login, p.input.Password)
	if err == nil {
		p.output = ts
	}
	return err
}

type ProtectedPing struct {
	output struct {
		Message string `json:"message"`
	}
}

func (p *ProtectedPing) Result() interface{} {
	return &p.output
}

func (p *ProtectedPing) Handle(ctx vatel.WebsocketContext) error {
	fmt.Println("aaa")
	return nil
}

type service struct {
}

func (service) Endpoints() []vatel.Endpoint {
	return []vatel.Endpoint{
		{
			Method:              "WS",
			Path:                "ping",
			WebsocketController: func() vatel.WebsocketHandler { return &Ping{} },
		},
		{
			Method:              "WS",
			Path:                "protected-ping",
			Perms:               []string{"Ping"},
			WebsocketController: func() vatel.WebsocketHandler { return &ProtectedPing{} },
		},
		{
			Method:     "POST",
			Path:       "/auth",
			Controller: func() vatel.Handler { return &Auth{va: va} },
		},
	}
}

type pm struct{}

func (*pm) PermissionBitPos(perm string) (uint, bool) {
	if perm == "Ping" {
		return 1, true
	}

	if perm == "FollowCalendar" {
		return 2, true
	}
	return 0, false
}

type au struct{}

func (*au) IsAllowed(requestPerms []byte, endpointPerms ...uint) (bool, error) {

	if n := len(requestPerms); n > 0 && n%2 != 0 {
		return false, errors.Forbidden().Set("reason", "invalid request permission set")
	}
	ok, err := bitset.AreSet(requestPerms, endpointPerms...)
	if err != nil {
		return false, errors.Catch(err).StatusCode(403)
	}
	return ok, nil
}

type td struct{}

func (*td) Decode(encodedToken []byte) (vatel.Tokener, error) {
	if bytes.Equal(encodedToken, []byte("abc")) {
		return nil, errors.Unauthorized()
	}
	return &aaa.Token{
		App: aaa.ApplicationPayload{
			UserID:           1,                                        //int                    `json:"user"`
			UserLogin:        "gera",                                   //string                 `json:"login"`
			RoleID:           2,                                        //int                    `json:"role"`
			PermissionBitSet: []byte(bitset.New(8).Set(1, 2).String()), //json.RawMessage        `json:"perms,omitempty"`
			IsDebug:          true,                                     //bool                   `json:"debug,omitempty"`
			ExtraPayload:     nil,                                      //map[string]interface{} `json:"extra,omitempty"`
		}}, nil
}

func TestWebsocketHandler(t *testing.T) {
	logger := zerolog.New(os.Stdout)
	//ws := vatel.NewWebsocketVatel()
	// TODO::: RETURN !!! va.AddWebsocketSupport(ws)
	va.SetPermissionManager(&pm{})
	va.SetAuthorizer(&au{})
	va.SetTokenDecoder(&td{})

	// go func() {
	// 	i := 0
	// 	for {
	// 		t.Log(i)
	// 		i++
	// 		ws.PingAll()
	// 		time.Sleep(time.Second)
	// 	}
	// }()

	var w service
	va.Add(&w)

	mux := router.New()
	va.MustBuildHandlers(mux, &logger)

	fs := fasthttp.Server{
		Handler:            mux.Handler,
		MaxRequestBodySize: 20 * 1024 * 1024,
		TCPKeepalive:       true,
		LogAllErrors:       true,
	}
	logger.Info().Str("listener", ":9999").Msg("starting http listener")

	fs.ListenAndServe(":9999")

}
