package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestReviewerSeesLivenessAttempts(t *testing.T) {
	srv, pool, _ := adminTestServer(t)
	email, subID, _ := seed(t, pool)
	ctx := context.Background()

	// Frames live in storage like uploaded files; the test store already has "estatutos.pdf",
	// which is good enough to check that the bytes are streamed from the stored key.
	var checkID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO liveness_checks (submission_id, field_key, steps, decision, reasons, result, frames, best_frame,
		                             expires_at, completed_at)
		VALUES ($1, 'vida', '{center,left,closer}', 'review', '{passive_uncertain}', '{"scores":{"passive":0.6}}',
		        '[{"step":0,"key":"estatutos.pdf","contentType":"image/jpeg"},{"step":1,"key":"nope.jpg","contentType":"image/jpeg"}]',
		        0, now(), now())
		RETURNING id`, subID).Scan(&checkID); err != nil {
		t.Fatal(err)
	}
	// A challenge still waiting for frames is not shown.
	if _, err := pool.Exec(ctx, `
		INSERT INTO liveness_checks (submission_id, field_key, steps, expires_at)
		VALUES ($1, 'vida', '{center,right,left}', now())`, subID); err != nil {
		t.Fatal(err)
	}

	c := newClient(t, srv.URL)
	if code, _ := c.do("GET", "/api/v1/admin/submissions/"+subID+"/liveness/"+checkID+"/frames/0", nil); code != http.StatusUnauthorized {
		t.Fatalf("anonymous frame: %d", code)
	}
	if code, body := c.do("POST", "/api/v1/auth/login", map[string]string{"email": email, "password": "secreto-123"}); code != http.StatusOK {
		t.Fatalf("login: %d %s", code, body)
	}

	code, body := c.do("GET", "/api/v1/admin/submissions/"+subID, nil)
	var detail struct {
		Liveness []livenessRow `json:"liveness"`
	}
	_ = json.Unmarshal(body, &detail)
	if code != http.StatusOK || len(detail.Liveness) != 1 {
		t.Fatalf("detail: %d %s", code, body)
	}
	l := detail.Liveness[0]
	if *l.Decision != "review" || l.Reasons[0] != "passive_uncertain" || len(l.FrameSteps) != 2 || l.FrameSteps[1] != 1 ||
		*l.BestFrame != 0 || !strings.Contains(string(l.Result), "0.6") {
		t.Fatalf("liveness row: %+v", l)
	}
	if strings.Contains(string(body), "estatutos.pdf\"") && strings.Contains(string(body), "\"key\"") {
		t.Fatalf("storage keys must not leak: %s", body)
	}

	code, body = c.do("GET", "/api/v1/admin/submissions/"+subID+"/liveness/"+checkID+"/frames/0", nil)
	if code != http.StatusOK || string(body) != "%PDF-1.4 test" {
		t.Fatalf("frame: %d %q", code, body)
	}
	for _, path := range []string{"/frames/1", "/frames/2", "/frames/x"} {
		if code, _ := c.do("GET", "/api/v1/admin/submissions/"+subID+"/liveness/"+checkID+path, nil); code != http.StatusNotFound {
			t.Fatalf("%s: %d", path, code)
		}
	}
	// Another organization's submission id with this check: not found.
	if code, _ := c.do("GET", "/api/v1/admin/submissions/00000000-0000-0000-0000-000000000000/liveness/"+checkID+"/frames/0", nil); code != http.StatusNotFound {
		t.Fatalf("wrong submission: %d", code)
	}
}
