// Package httpx holds HTTP helpers shared by the modules.
package httpx

import (
	"net"
	"net/http"
	"strings"
	"sync"
)

// localAddrs holds every IP assigned to this machine: a reverse proxy on the
// same host (nginx, Caddy) connects from one of them. From MaynStay's
// internal/auth/ratelimit.go.
var localAddrs = func() map[string]bool {
	m := map[string]bool{}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return m
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok {
			m[ipnet.IP.String()] = true
		}
	}
	return m
}()

var (
	proxyMu     sync.RWMutex
	trustedNets []*net.IPNet
)

// SetTrustedProxies adds proxy networks (CIDRs or single IPs) whose
// X-Real-IP / X-Forwarded-For headers are believed, for hosts where the
// proxy is not on this machine (a load balancer, a PaaS router). Invalid
// entries are returned so startup can report them.
func SetTrustedProxies(list []string) (invalid []string) {
	var nets []*net.IPNet
	for _, raw := range list {
		v := strings.TrimSpace(raw)
		if v == "" {
			continue
		}
		if !strings.Contains(v, "/") {
			if ip := net.ParseIP(v); ip != nil {
				if ip.To4() != nil {
					v += "/32"
				} else {
					v += "/128"
				}
			}
		}
		_, n, err := net.ParseCIDR(v)
		if err != nil {
			invalid = append(invalid, raw)
			continue
		}
		nets = append(nets, n)
	}
	proxyMu.Lock()
	trustedNets = nets
	proxyMu.Unlock()
	return invalid
}

func isProxy(ip net.IP) bool {
	if ip.IsLoopback() || localAddrs[ip.String()] {
		return true
	}
	proxyMu.RLock()
	defer proxyMu.RUnlock()
	for _, n := range trustedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// ClientIP returns the caller's address. A direct connection is its own
// address; a connection from a trusted proxy uses the forwarded header. With
// no usable header the IP is unknown (""), and callers decide what to do
// (the chatbot gives unknown callers one shared, larger bucket). A client
// can't spoof the header unless it connects through a trusted proxy.
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && !isProxy(ip) {
		return host
	}
	// X-Real-IP is set (overwritten) by the proxy itself, e.g. nginx.
	if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); v != "" {
		return usable(v)
	}
	// X-Forwarded-For is appended to by each proxy, so the left-most entries
	// are whatever the client sent: walk from the right and take the first
	// address that isn't one of our proxies.
	hops := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		v := strings.TrimSpace(hops[i])
		ip := net.ParseIP(v)
		if ip == nil {
			return ""
		}
		if !isProxy(ip) {
			return v
		}
	}
	return ""
}

func usable(v string) string {
	if ip := net.ParseIP(v); ip == nil || ip.IsLoopback() || localAddrs[ip.String()] {
		return ""
	}
	return v
}
