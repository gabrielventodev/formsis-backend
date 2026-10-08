package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLimiterWindow(t *testing.T) {
	now := time.Unix(0, 0)
	l := New(2, time.Minute)
	l.now = func() time.Time { return now }
	if !l.Allow("a") || !l.Allow("a") {
		t.Fatal("first two hits should pass")
	}
	if l.Allow("a") || !l.Blocked("a") {
		t.Fatal("third hit should be blocked")
	}
	if !l.Allow("b") {
		t.Fatal("other keys are independent")
	}
	now = now.Add(time.Minute)
	if !l.Allow("a") {
		t.Fatal("a new window starts over")
	}
}

func TestRealIP(t *testing.T) {
	cases := []struct {
		remote, xff, want string
	}{
		{"172.18.0.5:4000", "203.0.113.7", "203.0.113.7"},         // web container forwarding Caddy's header
		{"127.0.0.1:4000", "6.6.6.6, 203.0.113.7", "203.0.113.7"}, // spoofed first entry is ignored
		{"198.51.100.1:4000", "6.6.6.6", "198.51.100.1"},          // public peers cannot set their IP
		{"172.18.0.5:4000", "", "172.18.0.5"},                     // no header
		{"172.18.0.5:4000", "garbage", "172.18.0.5"},              // unparsable header
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		var got string
		RealIP(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = ClientIP(r) })).ServeHTTP(httptest.NewRecorder(), r)
		if got != c.want {
			t.Errorf("remote %s xff %q: got %s, want %s", c.remote, c.xff, got, c.want)
		}
	}
}
