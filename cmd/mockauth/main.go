// Command mockauth is a LOCAL TESTING stand-in for the SKYMCM website's
// login, so the chatbot's sign-in requirement can be tried without the live
// site. It is never deployed and holds no real secret: tokens are signed
// with a random key made at startup, and every account is fake.
//
// It serves:
//
//	GET  /api/v1/users/profile   what the chatbot backend asks (Bearer token):
//	                             200 for a valid session, 401 otherwise
//	GET  /mock-login             a page to sign in as a test account
//	POST /mock-login             sets the website's login cookie (maynstay_token)
//	GET  /mock-logout            revokes the account's tokens and clears the cookie
//	GET  /mock/tokens            fresh test tokens: valid, expired, revoked, invalid
//	POST /mock/revoke?account=N  revokes every token of a test account
//
// Run: go run ./cmd/mockauth   (listens on localhost:8090 only)
//
// Open the login page as http://localhost:8090/... (not 127.0.0.1) so the
// cookie belongs to "localhost", the host the chat frontend and backend use.
package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const cookieName = "maynstay_token"

type account struct {
	ID    int64
	Name  string
	Email string
}

// Fake accounts only.
var accounts = []account{
	{9001, "Test Guest A", "guest.a@example.test"},
	{9002, "Test Guest B", "guest.b@example.test"},
}

func findAccount(id int64) (account, bool) {
	for _, a := range accounts {
		if a.ID == id {
			return a, true
		}
	}
	return account{}, false
}

type server struct {
	key  []byte
	now  func() time.Time
	home string // where the login page returns to by default

	mu      sync.Mutex
	revoked map[string]bool    // token -> revoked
	issued  map[int64][]string // account -> its tokens
}

func newServer(home string) (*server, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return &server{key: key, now: time.Now, home: home, revoked: map[string]bool{}, issued: map[int64][]string{}}, nil
}

var b64 = base64.RawURLEncoding

func (s *server) sign(data string) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(data))
	return b64.EncodeToString(m.Sum(nil))
}

// issue makes a JWT-shaped token like the website's (claims signup_id,
// email, role, full_name, iat, exp), signed with the mock key.
func (s *server) issue(a account, ttl time.Duration) string {
	now := s.now()
	nonce := make([]byte, 8)
	_, _ = rand.Read(nonce)
	claims, _ := json.Marshal(map[string]any{
		"signup_id": a.ID, "email": a.Email, "role": "user", "full_name": a.Name,
		"iat": now.Unix(), "exp": now.Add(ttl).Unix(), "jti": b64.EncodeToString(nonce),
	})
	head := b64.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + b64.EncodeToString(claims)
	tok := head + "." + s.sign(head)
	s.mu.Lock()
	s.issued[a.ID] = append(s.issued[a.ID], tok)
	s.mu.Unlock()
	return tok
}

var errInvalid = errors.New("invalid token")

// verify checks the signature, expiry and revocation, like the website.
func (s *server) verify(tok string) (account, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 || !hmac.Equal([]byte(s.sign(parts[0]+"."+parts[1])), []byte(parts[2])) {
		return account{}, errInvalid
	}
	raw, err := b64.DecodeString(parts[1])
	if err != nil {
		return account{}, errInvalid
	}
	var c struct {
		SignupID int64 `json:"signup_id"`
		Exp      int64 `json:"exp"`
	}
	if json.Unmarshal(raw, &c) != nil || c.Exp <= s.now().Unix() {
		return account{}, errInvalid
	}
	s.mu.Lock()
	revoked := s.revoked[tok]
	s.mu.Unlock()
	a, ok := findAccount(c.SignupID)
	if revoked || !ok {
		return account{}, errInvalid
	}
	return a, nil
}

// revokeAccount logs an account out everywhere (the website's logout).
func (s *server) revokeAccount(id int64) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.issued[id] {
		s.revoked[t] = true
	}
	n := len(s.issued[id])
	s.issued[id] = nil
	return n
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// profile is GET /api/v1/users/profile, as the website answers it.
func (s *server) profile(w http.ResponseWriter, r *http.Request) {
	h := r.Header.Get("Authorization")
	tok := ""
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		tok = strings.TrimSpace(h[7:])
	}
	a, err := s.verify(tok)
	if err != nil {
		log.Printf("profile: 401")
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
		return
	}
	log.Printf("profile: 200 account %d", a.ID)
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": "ok", "data": map[string]any{
		"signup_id": a.ID, "full_name": a.Name, "email": a.Email, "phone": "0000000000", "gender": "",
	}})
}

// safeRedirect allows returning only to a page on this machine.
func (s *server) safeRedirect(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return s.home
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1":
		return u.String()
	}
	return s.home
}

var loginPage = template.Must(template.New("login").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Mock SKYMCM login (local testing)</title>
<style>body{font-family:system-ui,sans-serif;max-width:420px;margin:48px auto;padding:0 16px;color:#1f2937}
.note{background:#fef3c7;padding:8px 12px;border-radius:8px;font-size:13px}button{display:block;width:100%;margin:8px 0;padding:12px;
border:0;border-radius:999px;background:#1e3a8a;color:#fff;font-weight:700;cursor:pointer}label{font-size:14px}</style></head>
<body><h1>Mock SKYMCM login</h1>
<p class="note">Local testing only. Fake accounts; nothing here touches the live website.</p>
{{if .Signed}}<p>Signed in as <b>{{.Signed}}</b>. <a href="/mock-logout?redirect={{.Redirect}}">Log out</a></p>{{end}}
<form method="post" action="/mock-login">
<input type="hidden" name="redirect" value="{{.Redirect}}">
<label><input type="radio" name="ttl" value="86400" checked> 24-hour session</label><br>
<label><input type="radio" name="ttl" value="60"> 1-minute session (to test expiry)</label>
{{range .Accounts}}<button name="account" value="{{.ID}}">Log in as {{.Name}}</button>{{end}}
</form></body></html>`))

func (s *server) loginForm(w http.ResponseWriter, r *http.Request) {
	signed := ""
	if c, err := r.Cookie(cookieName); err == nil {
		if a, err := s.verify(c.Value); err == nil {
			signed = a.Name
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = loginPage.Execute(w, map[string]any{"Accounts": accounts, "Redirect": s.safeRedirect(r.URL.Query().Get("redirect")), "Signed": signed})
}

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	id, _ := strconv.ParseInt(r.PostForm.Get("account"), 10, 64)
	a, ok := findAccount(id)
	if !ok {
		http.Error(w, "unknown test account", http.StatusBadRequest)
		return
	}
	ttl := 24 * time.Hour
	if secs, err := strconv.Atoi(r.PostForm.Get("ttl")); err == nil && secs >= 10 && secs <= 86400 {
		ttl = time.Duration(secs) * time.Second
	}
	// The same cookie the website sets (HttpOnly, SameSite=Lax, path /).
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: s.issue(a, ttl), Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: int(ttl / time.Second)})
	log.Printf("login: account %d, session %s", a.ID, ttl)
	http.Redirect(w, r, s.safeRedirect(r.PostForm.Get("redirect")), http.StatusSeeOther)
}

func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		if a, err := s.verify(c.Value); err == nil {
			log.Printf("logout: account %d, %d tokens revoked", a.ID, s.revokeAccount(a.ID))
		}
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1})
	http.Redirect(w, r, s.safeRedirect(r.URL.Query().Get("redirect")), http.StatusSeeOther)
}

// tokens hands out fresh test tokens for scripted tests.
func (s *server) tokens(w http.ResponseWriter, r *http.Request) {
	a, b := accounts[0], accounts[1]
	revoked := s.issue(a, time.Hour)
	s.mu.Lock()
	s.revoked[revoked] = true
	s.mu.Unlock()
	expired := s.issue(b, -time.Minute)
	valid := s.issue(a, time.Hour)
	forged := strings.Join(strings.Split(valid, ".")[:2], ".") + "." + b64.EncodeToString([]byte("not-the-mock-signature-at-all!!"))
	writeJSON(w, http.StatusOK, map[string]string{
		"valid_a": valid, "valid_b": s.issue(b, time.Hour), "short_a": s.issue(a, 15*time.Second),
		"expired": expired, "revoked": revoked, "invalid": forged,
	})
}

func (s *server) revoke(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.URL.Query().Get("account"), 10, 64)
	if _, ok := findAccount(id); !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown test account"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"revoked": s.revokeAccount(id)})
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/users/profile", s.profile)
	mux.HandleFunc("GET /mock-login", s.loginForm)
	mux.HandleFunc("POST /mock-login", s.login)
	mux.HandleFunc("GET /mock-logout", s.logout)
	mux.HandleFunc("POST /mock-logout", s.logout)
	mux.HandleFunc("GET /mock/tokens", s.tokens)
	mux.HandleFunc("POST /mock/revoke", s.revoke)
	return mux
}

func main() {
	port := os.Getenv("MOCK_AUTH_PORT")
	if port == "" {
		port = "8090"
	}
	home := os.Getenv("MOCK_AUTH_HOME")
	if home == "" {
		home = "http://localhost:3000/"
	}
	s, err := newServer(home)
	if err != nil {
		log.Fatal(err)
	}
	h := s.routes()
	// Loopback only, IPv4 and IPv6 ("localhost" may resolve to either).
	var listeners []net.Listener
	for _, host := range []string{"127.0.0.1", "[::1]"} {
		l, err := net.Listen("tcp", host+":"+port)
		if err != nil {
			log.Printf("not listening on %s:%s: %v", host, port, err)
			continue
		}
		listeners = append(listeners, l)
	}
	if len(listeners) == 0 {
		log.Fatal("mockauth: could not listen on localhost")
	}
	fmt.Printf("mock SKYMCM auth (LOCAL TESTING ONLY) on http://localhost:%s - login page http://localhost:%s/mock-login\n", port, port)
	errs := make(chan error, len(listeners))
	for _, l := range listeners {
		go func(l net.Listener) {
			errs <- (&http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}).Serve(l)
		}(l)
	}
	log.Fatal(<-errs)
}
