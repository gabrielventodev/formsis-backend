// Package ratelimit counts attempts per key (an IP or an email) in a fixed time
// window, in memory. It is enough for a single API instance; several instances
// would each keep their own counts.
package ratelimit

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// Limiter allows up to Limit hits per key in each Window.
type Limiter struct {
	limit  int
	window time.Duration
	now    func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
	swept   time.Time
}

type bucket struct {
	start time.Time
	count int
}

func New(limit int, window time.Duration) *Limiter {
	return &Limiter{limit: limit, window: window, now: time.Now, buckets: map[string]*bucket{}}
}

// Allow records a hit for key and reports whether it is within the limit.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.get(key)
	if b.count >= l.limit {
		return false
	}
	b.count++
	return true
}

// Blocked reports whether key already used up its hits, without recording one.
func (l *Limiter) Blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.get(key).count >= l.limit
}

// Hit records a hit for key without checking it, e.g. a failed login.
func (l *Limiter) Hit(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.get(key).count++
}

// get returns the live bucket for key; the caller holds mu.
func (l *Limiter) get(key string) *bucket {
	now := l.now()
	if now.Sub(l.swept) > l.window {
		for k, b := range l.buckets {
			if now.Sub(b.start) >= l.window {
				delete(l.buckets, k)
			}
		}
		l.swept = now
	}
	b := l.buckets[key]
	if b == nil || now.Sub(b.start) >= l.window {
		b = &bucket{start: now}
		l.buckets[key] = b
	}
	return b
}

// ClientIP is the request's remote IP (RealIP has already resolved proxies).
func ClientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// RealIP replaces r.RemoteAddr with the client's IP from X-Forwarded-For, but
// only when the request comes from a private or loopback address (the web
// container or Caddy in front of the API). It takes the last entry, the one
// added by the nearest proxy, so a client cannot choose its own IP by sending
// the header itself. Requests from public addresses keep their RemoteAddr.
func RealIP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ip := forwardedIP(r); ip != "" {
			r.RemoteAddr = ip
		}
		next.ServeHTTP(w, r)
	})
}

func forwardedIP(r *http.Request) string {
	peer, err := netip.ParseAddr(ClientIP(r))
	if err != nil || !(peer.IsLoopback() || peer.IsPrivate()) {
		return ""
	}
	xff := r.Header.Values("X-Forwarded-For")
	if len(xff) == 0 {
		return ""
	}
	parts := strings.Split(xff[len(xff)-1], ",")
	ip, err := netip.ParseAddr(strings.TrimSpace(parts[len(parts)-1]))
	if err != nil {
		return ""
	}
	return ip.Unmap().String()
}
