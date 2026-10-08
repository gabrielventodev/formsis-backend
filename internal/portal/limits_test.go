package portal

import (
	"fmt"
	"net/http"
	"testing"
)

func TestStartAndResumeAreRateLimited(t *testing.T) {
	srv, _, link := setup(t)
	for i := 0; i < 5; i++ {
		if code, body := call(t, srv, "POST", "/links/"+link+"/start", "", map[string]string{"email": "ana@empresa.cl"}); code != http.StatusCreated {
			t.Fatalf("start %d: %d %v", i, code, body)
		}
	}
	if code, _ := call(t, srv, "POST", "/links/"+link+"/start", "", map[string]string{"email": "ANA@empresa.cl"}); code != http.StatusTooManyRequests {
		t.Fatalf("sixth start for the same email: got %d, want 429", code)
	}
	if code, _ := call(t, srv, "POST", "/links/"+link+"/start", "", map[string]string{"email": "otra@empresa.cl"}); code != http.StatusCreated {
		t.Fatalf("other email: got %d", code)
	}

	for i := 0; i < 3; i++ {
		if code, _ := call(t, srv, "POST", "/resume", "", map[string]string{"email": "ana@empresa.cl"}); code != http.StatusAccepted {
			t.Fatalf("resume %d: %d", i, code)
		}
	}
	if code, _ := call(t, srv, "POST", "/resume", "", map[string]string{"email": "ana@empresa.cl"}); code != http.StatusTooManyRequests {
		t.Fatalf("fourth resume for the same email: got %d, want 429", code)
	}
}

func TestFileCapPerSubmission(t *testing.T) {
	srv, _, link := setup(t)
	_, body := call(t, srv, "POST", "/links/"+link+"/start", "", map[string]string{"email": "ana@empresa.cl"})
	token := body["accessToken"].(string)
	for i := 0; i < maxFilesPerSubmission; i++ {
		if code, out := upload(t, srv, token, "doc", fmt.Sprintf("d%d.pdf", i), "application/pdf", pdf); code != http.StatusCreated {
			t.Fatalf("upload %d: %d %v", i, code, out)
		}
	}
	if code, _ := upload(t, srv, token, "doc", "extra.pdf", "application/pdf", pdf); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("upload over the cap: got %d, want 413", code)
	}
}
