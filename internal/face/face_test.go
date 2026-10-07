package face

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewChallenge(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		c := NewChallenge()
		if len(c) != 3 || c[0] != "center" || c[1] == c[2] || c[1] == "center" || c[2] == "center" {
			t.Fatalf("challenge %v", c)
		}
		seen[c[1]+","+c[2]] = true
	}
	if len(seen) != 6 {
		t.Fatalf("expected all 6 orders, got %v", seen)
	}
}

func TestLiveness(t *testing.T) {
	var got struct {
		steps, frameSteps string
		frames            int
		auth, ctype       string
	}
	answer := `{"decision":"pass","reasons":[],"best_frame":2,"scores":{"passive":0.9}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(1 << 20)
		got.steps, got.frameSteps = r.FormValue("steps"), r.FormValue("frame_steps")
		got.frames = len(r.MultipartForm.File["frames"])
		got.auth = r.Header.Get("Authorization")
		got.ctype = r.MultipartForm.File["frames"][0].Header.Get("Content-Type")
		_, _ = w.Write([]byte(answer))
	}))
	defer srv.Close()

	c := New(srv.URL+"/", "tok")
	res, err := c.Liveness(context.Background(), []string{"center", "left"}, []Frame{
		{Step: 0, Data: []byte("a"), ContentType: "image/jpeg"},
		{Step: 1, Data: []byte("b"), ContentType: "image/jpeg"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != Pass || *res.BestFrame != 2 || string(res.Raw) != answer {
		t.Fatalf("result %+v", res)
	}
	var steps []string
	_ = json.Unmarshal([]byte(got.steps), &steps)
	if len(steps) != 2 || got.frameSteps != "[0,1]" || got.frames != 2 || got.auth != "Bearer tok" || got.ctype != "image/jpeg" {
		t.Fatalf("request %+v", got)
	}

	answer = `{"decision":"maybe"}`
	if _, err := c.Liveness(context.Background(), []string{"center", "left"}, []Frame{{Data: []byte("a"), ContentType: "image/jpeg"}}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unknown decision: %v", err)
	}
	if New("", "x") != nil {
		t.Fatal("no URL means no client")
	}
}
