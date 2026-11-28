package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func newTest(t *testing.T) (*server, *httptest.Server) {
	s, err := newServer("http://localhost:3000/")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.routes())
	t.Cleanup(srv.Close)
	return s, srv
}

func profileStatus(t *testing.T, base, tok string) int {
	req, _ := http.NewRequest(http.MethodGet, base+"/api/v1/users/profile", nil)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

func TestProfileAcceptsOnlyValidTokens(t *testing.T) {
	_, srv := newTest(t)
	res, err := http.Get(srv.URL + "/mock/tokens")
	if err != nil {
		t.Fatal(err)
	}
	var toks map[string]string
	if err := json.NewDecoder(res.Body).Decode(&toks); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	want := map[string]int{"valid_a": 200, "valid_b": 200, "short_a": 200, "expired": 401, "revoked": 401, "invalid": 401}
	for name, status := range want {
		if got := profileStatus(t, srv.URL, toks[name]); got != status {
			t.Errorf("%s: %d, want %d", name, got, status)
		}
	}
	if profileStatus(t, srv.URL, "") != 401 {
		t.Error("no token accepted")
	}
	// Revoking the account logs out its live tokens.
	if res, err := http.Post(srv.URL+"/mock/revoke?account=9001", "", nil); err != nil || res.StatusCode != 200 {
		t.Fatal("revoke failed")
	}
	if profileStatus(t, srv.URL, toks["valid_a"]) != 401 || profileStatus(t, srv.URL, toks["valid_b"]) != 200 {
		t.Error("revocation must apply to that account only")
	}
}

func TestLoginSetsTheWebsiteCookieAndLogoutClearsIt(t *testing.T) {
	s, srv := newTest(t)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	form := url.Values{"account": {"9002"}, "ttl": {"60"}, "redirect": {"http://localhost:3000/embed"}}
	res, err := client.PostForm(srv.URL+"/mock-login", form)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "http://localhost:3000/embed" {
		t.Fatalf("login redirect: %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	var c *http.Cookie
	for _, k := range res.Cookies() {
		if k.Name == cookieName {
			c = k
		}
	}
	if c == nil || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.MaxAge != 60 {
		t.Fatalf("cookie: %+v", c)
	}
	if a, err := s.verify(c.Value); err != nil || a.ID != 9002 {
		t.Fatalf("issued token: %v %v", a, err)
	}
	// Logout revokes and clears.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/mock-logout?redirect=https://evil.example/", nil)
	req.AddCookie(c)
	res, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.Header.Get("Location") != "http://localhost:3000/" {
		t.Fatalf("an outside redirect must fall back to the local home: %q", res.Header.Get("Location"))
	}
	if !strings.Contains(res.Header.Get("Set-Cookie"), "Max-Age=0") {
		t.Fatalf("cookie not cleared: %q", res.Header.Get("Set-Cookie"))
	}
	if _, err := s.verify(c.Value); err == nil {
		t.Fatal("token still valid after logout")
	}
}

func TestExpiryIsEnforced(t *testing.T) {
	s, _ := newTest(t)
	tok := s.issue(accounts[0], time.Minute)
	s.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if _, err := s.verify(tok); err == nil {
		t.Fatal("expired token accepted")
	}
}
