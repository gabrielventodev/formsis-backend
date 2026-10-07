package webhooks

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// Endpoints are URLs typed by an admin, so the API must not become a way to
// reach internal services (SSRF): only HTTPS to public addresses, checked
// again at connect time so DNS can't swap in a private IP after validation.
// AllowInsecure (development) also allows plain HTTP and private addresses.

var ErrBlockedAddress = errors.New("la URL apunta a una red privada o local")

var extraBlocked = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"), // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"), // NAT64 can reach private IPv4
}

func blockedIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return true
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	addr = addr.Unmap()
	for _, p := range extraBlocked {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// ValidateURL checks an endpoint URL before saving it and returns it normalized.
func ValidateURL(raw string, allowInsecure bool) (string, error) {
	raw = strings.TrimSpace(raw)
	if len(raw) > 2000 {
		return "", errors.New("la URL es demasiado larga")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Opaque != "" {
		return "", errors.New("URL inválida")
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && allowInsecure:
	default:
		return "", errors.New("la URL debe empezar con https://")
	}
	if u.User != nil {
		return "", errors.New("la URL no puede llevar usuario ni contraseña")
	}
	u.Fragment = ""
	if !allowInsecure {
		host := strings.ToLower(u.Hostname())
		if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".local") {
			return "", ErrBlockedAddress
		}
		if ip := net.ParseIP(host); ip != nil && blockedIP(ip) {
			return "", ErrBlockedAddress
		}
	}
	return u.String(), nil
}

// NewClient returns the HTTP client deliveries use: no redirects, no
// environment proxy, short timeouts and (unless allowInsecure) a dial-time
// check that refuses private addresses.
func NewClient(allowInsecure bool) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	if !allowInsecure {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(host); ip == nil || blockedIP(ip) {
				return ErrBlockedAddress
			}
			return nil
		}
	}
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.DialContext(ctx, network, addr)
			},
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
			MaxIdleConnsPerHost:   2,
			IdleConnTimeout:       30 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}
