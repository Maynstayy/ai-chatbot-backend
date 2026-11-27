package httpx

import (
	"net/http/httptest"
	"testing"
)

func TestClientIP(t *testing.T) {
	defer SetTrustedProxies(nil)
	SetTrustedProxies([]string{"10.0.0.0/8"})

	cases := []struct {
		name, remote, realIP, xff, want string
	}{
		{"direct client ignores headers", "203.0.113.7:5000", "198.51.100.1", "198.51.100.2", "203.0.113.7"},
		{"local proxy uses X-Real-IP", "127.0.0.1:5000", "198.51.100.1", "", "198.51.100.1"},
		{"trusted proxy uses right-most untrusted hop", "10.1.2.3:5000", "", "6.6.6.6, 198.51.100.9, 10.0.0.2", "198.51.100.9"},
		{"proxy without headers is unknown", "127.0.0.1:5000", "", "", ""},
		{"garbage header is unknown", "127.0.0.1:5000", "not-an-ip", "", ""},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		if c.realIP != "" {
			r.Header.Set("X-Real-IP", c.realIP)
		}
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := ClientIP(r); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestSetTrustedProxiesReportsInvalid(t *testing.T) {
	defer SetTrustedProxies(nil)
	if bad := SetTrustedProxies([]string{"10.0.0.0/8", "192.168.1.5", "nope"}); len(bad) != 1 || bad[0] != "nope" {
		t.Fatalf("invalid = %v", bad)
	}
}
