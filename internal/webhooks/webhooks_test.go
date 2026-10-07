package webhooks

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSignVerify(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{"type":"ping"}`)
	h := Sign("whsec_x", now, body)
	if h != Sign("whsec_x", now, body) || len(h) < 20 {
		t.Fatalf("unstable signature %q", h)
	}
	if !Verify("whsec_x", h, body, 5*time.Minute, now.Add(time.Minute)) {
		t.Fatal("valid signature rejected")
	}
	for name, ok := range map[string]bool{
		"other secret": Verify("whsec_y", h, body, 5*time.Minute, now),
		"other body":   Verify("whsec_x", h, []byte(`{}`), 5*time.Minute, now),
		"too old":      Verify("whsec_x", h, body, 5*time.Minute, now.Add(10*time.Minute)),
		"garbage":      Verify("whsec_x", "v1=abc", body, 5*time.Minute, now),
	} {
		if ok {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestValidateURL(t *testing.T) {
	good := []string{"https://hooks.example.com/formsis", "https://1.1.1.1/x?a=b"}
	for _, u := range good {
		if _, err := ValidateURL(u, false); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	bad := []string{
		"http://hooks.example.com", "ftp://example.com", "https://", "notaurl",
		"https://user:pw@example.com", "https://localhost/x", "https://127.0.0.1/x",
		"https://10.0.0.5/x", "https://192.168.1.1", "https://169.254.169.254/latest/meta-data",
		"https://[::1]/x", "https://[fd00::1]/", "https://100.64.0.1/", "https://metadata.google.internal/",
		"https://[::ffff:127.0.0.1]/",
	}
	for _, u := range bad {
		if _, err := ValidateURL(u, false); err == nil {
			t.Errorf("%s: accepted", u)
		}
	}
	if _, err := ValidateURL("http://127.0.0.1:8080/x", true); err != nil {
		t.Errorf("insecure mode refused local http: %v", err)
	}
}

// The dial-time guard stops a public-looking URL that resolves to a private address.
func TestClientRefusesPrivateAddresses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	_, err := NewClient(false).Get(srv.URL)
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("got %v, want ErrBlockedAddress", err)
	}
	if res, err := NewClient(true).Get(srv.URL); err != nil {
		t.Fatalf("insecure client: %v", err)
	} else {
		res.Body.Close()
	}
}

func TestBackoff(t *testing.T) {
	if backoff(1) != time.Minute || backoff(7) != 64*time.Minute {
		t.Fatal("unexpected backoff")
	}
}
