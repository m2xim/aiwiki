package server

import (
	"net/http"
	"net/url"
	"testing"
)

func TestAuthExplicitAccountSwitch(t *testing.T) {
	x := newAuthHarness(t, nil)
	for _, tc := range []struct{ path, prompt string }{
		{"/auth/login", ""}, {"/auth/login?reauth=1", "login"}, {"/auth/login?reauth=other&prompt=none&redirect_uri=https://attacker.example.org", ""},
	} {
		r := x.do("GET", tc.path, "", nil)
		if r.Code != http.StatusFound {
			t.Fatal(r.Code)
		}
		u, err := url.Parse(r.Header().Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		if q.Get("prompt") != tc.prompt || q.Get("redirect_uri") != authTestRedirect || q.Get("nonce") == "" || q.Get("state") == "" || q.Get("code_challenge_method") != "S256" {
			t.Fatal("invalid account-switch authorization request")
		}
	}
}
