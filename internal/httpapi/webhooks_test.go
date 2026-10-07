package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/gabrielventodev/formsis/api/internal/db"
	"github.com/gabrielventodev/formsis/api/internal/webhooks"
)

type received struct {
	event, delivery, signature string
	body                       []byte
}

// receiver records requests and answers with the status in code.
type receiver struct {
	mu   sync.Mutex
	got  []received
	code int
}

func (rc *receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.got = append(rc.got, received{r.Header.Get("Formsis-Event"), r.Header.Get("Formsis-Delivery"), r.Header.Get("Formsis-Signature"), body})
	w.WriteHeader(rc.code)
	_, _ = w.Write([]byte("nope"))
}

func (rc *receiver) take() []received {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	out := rc.got
	rc.got = nil
	return out
}

func TestWebhooks(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	// Other tests may have left deliveries behind; this test drives the worker itself.
	if _, err := pool.Exec(ctx, `UPDATE webhook_deliveries SET status = 'failed' WHERE status = 'pending'`); err != nil {
		t.Fatal(err)
	}
	worker := webhooks.NewWorker(pool, true)
	srv := httptest.NewServer((&Server{DB: pool, OrgID: "unused", WebOrigin: "http://localhost:3000",
		WebURL: "https://forms.example.com", Webhooks: worker}).Routes())
	t.Cleanup(srv.Close)
	rc := &receiver{code: http.StatusOK}
	hookSrv := httptest.NewServer(rc)
	t.Cleanup(hookSrv.Close)

	ownerEmail, subID, _ := seed(t, pool)
	owner := newClient(t, srv.URL)
	if code, _ := owner.do("POST", "/api/v1/auth/login", map[string]string{"email": ownerEmail, "password": "secreto-123"}); code != http.StatusOK {
		t.Fatal("owner login")
	}
	rita, _ := addMember(t, pool, srv.URL, subID, "Rita", "reviewer")
	if code, _ := rita.do("GET", "/api/v1/admin/webhooks", nil); code != http.StatusForbidden {
		t.Fatalf("reviewer lists webhooks: %d", code)
	}

	// Validation.
	for _, bad := range []map[string]any{
		{},
		{"url": "ftp://x.example.com"},
		{"url": hookSrv.URL, "events": []string{"submission.bogus"}},
		{"url": hookSrv.URL, "events": []string{}},
	} {
		if code, body := owner.do("POST", "/api/v1/admin/webhooks", bad); code != http.StatusBadRequest {
			t.Fatalf("create %v: %d %s", bad, code, body)
		}
	}

	// Create: the secret is shown once.
	code, body := owner.do("POST", "/api/v1/admin/webhooks", map[string]any{
		"url": hookSrv.URL + "/hook", "description": "CRM",
		"events": []string{"submission.in_review", "submission.approved", "submission.approved"},
	})
	var hook webhookOut
	_ = json.Unmarshal(body, &hook)
	if code != http.StatusCreated || len(hook.Secret) < 20 || len(hook.Events) != 2 || !hook.Active {
		t.Fatalf("create: %d %s", code, body)
	}
	secret := hook.Secret
	_, body = owner.do("GET", "/api/v1/admin/webhooks", nil)
	var list struct {
		Webhooks []webhookOut `json:"webhooks"`
		Events   []string     `json:"events"`
	}
	_ = json.Unmarshal(body, &list)
	if len(list.Webhooks) != 1 || list.Webhooks[0].Secret != "" || list.Webhooks[0].SecretHint == "" || len(list.Events) != 6 {
		t.Fatalf("list: %s", body)
	}

	// Status changes queue signed deliveries; unsubscribed ones don't.
	transition := func(to string) {
		t.Helper()
		if code, body := owner.do("POST", "/api/v1/admin/submissions/"+subID+"/transition", map[string]string{"to": to}); code != http.StatusOK {
			t.Fatalf("transition %s: %d %s", to, code, body)
		}
	}
	transition("in_review")
	transition("approved")
	if _, err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got := rc.take()
	if len(got) != 2 {
		t.Fatalf("deliveries: %d", len(got))
	}
	var p struct {
		Type string `json:"type"`
		Data struct {
			FromStatus string         `json:"from_status"`
			Submission map[string]any `json:"submission"`
			Applicant  map[string]any `json:"applicant"`
			Answers    map[string]any `json:"answers"`
		} `json:"data"`
	}
	seen := map[string]string{}
	for _, g := range got {
		if !webhooks.Verify(secret, g.signature, g.body, time.Minute, time.Now()) {
			t.Fatalf("bad signature on %s", g.event)
		}
		_ = json.Unmarshal(g.body, &p)
		if p.Type != g.event || p.Data.Submission["id"] != subID || p.Data.Applicant["email"] != "pedro@cliente.cl" || p.Data.Answers != nil {
			t.Fatalf("payload: %s", g.body)
		}
		if p.Data.Submission["admin_url"] != "https://forms.example.com/admin/envios/"+subID {
			t.Fatalf("admin_url: %v", p.Data.Submission["admin_url"])
		}
		seen[p.Type] = p.Data.FromStatus
	}
	if seen["submission.in_review"] != "submitted" || seen["submission.approved"] != "in_review" {
		t.Fatalf("events: %v", seen)
	}

	// include_data adds the answers; a failing receiver is retried later.
	hookURL := "/api/v1/admin/webhooks/" + hook.ID
	if code, body := owner.do("PATCH", hookURL, map[string]any{"include_data": true}); code != http.StatusOK {
		t.Fatalf("patch: %d %s", code, body)
	}
	rc.code = http.StatusInternalServerError
	transition("in_review") // reopen
	if _, err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got = rc.take()
	if len(got) != 1 {
		t.Fatalf("reopen deliveries: %d", len(got))
	}
	_ = json.Unmarshal(got[0].body, &p)
	if p.Data.Answers["razon_social"] != "=Cliente SpA" {
		t.Fatalf("answers missing: %s", got[0].body)
	}
	var deliveries []deliveryOut
	_, body = owner.do("GET", hookURL+"/deliveries", nil)
	_ = json.Unmarshal(body, &deliveries)
	if len(deliveries) != 3 || deliveries[0].Status != "pending" || deliveries[0].Attempts != 1 ||
		deliveries[0].NextAttemptAt == nil || deliveries[0].LastError != "HTTP 500: nope" {
		t.Fatalf("after failure: %s", body)
	}
	if n, _ := worker.RunOnce(ctx); n != 0 {
		t.Fatalf("retried before backoff: %d", n)
	}
	// Failed for good after the last attempt; then a manual retry succeeds.
	failedID := deliveries[0].ID
	if _, err := pool.Exec(ctx, `UPDATE webhook_deliveries SET attempts = $2, next_attempt_at = now() WHERE id = $1`, failedID, webhooks.MaxAttempts-1); err != nil {
		t.Fatal(err)
	}
	_, _ = worker.RunOnce(ctx)
	rc.take()
	var d deliveryOut
	_, body = owner.do("GET", hookURL+"/deliveries/"+failedID, nil)
	_ = json.Unmarshal(body, &d)
	if d.Status != "failed" || d.NextAttemptAt != nil || len(d.Payload) == 0 {
		t.Fatalf("final failure: %s", body)
	}
	rc.code = http.StatusNoContent
	if code, _ := owner.do("POST", hookURL+"/deliveries/"+failedID+"/retry", nil); code != http.StatusNoContent {
		t.Fatalf("retry: %d", code)
	}
	if code, _ := owner.do("POST", hookURL+"/deliveries/"+failedID+"/retry", nil); code != http.StatusConflict {
		t.Fatalf("retry pending: %d", code)
	}
	_, _ = worker.RunOnce(ctx)
	if got := rc.take(); len(got) != 1 || got[0].delivery != failedID {
		t.Fatalf("manual retry: %v", got)
	}

	// Test ping, secret rotation, deactivation and deletion.
	if code, _ := owner.do("POST", hookURL+"/test", nil); code != http.StatusAccepted {
		t.Fatalf("test: %d", code)
	}
	code, body = owner.do("POST", hookURL+"/rotate-secret", nil)
	var rotated webhookOut
	_ = json.Unmarshal(body, &rotated)
	if code != http.StatusOK || rotated.Secret == "" || rotated.Secret == secret {
		t.Fatalf("rotate: %d %s", code, body)
	}
	_, _ = worker.RunOnce(ctx)
	got = rc.take()
	if len(got) != 1 || got[0].event != "ping" || !webhooks.Verify(rotated.Secret, got[0].signature, got[0].body, time.Minute, time.Now()) {
		t.Fatalf("ping: %v", got)
	}
	owner.do("PATCH", hookURL, map[string]any{"active": false})
	if code, _ := owner.do("POST", hookURL+"/test", nil); code != http.StatusBadRequest {
		t.Fatalf("test inactive: %d", code)
	}
	transition("approved")
	_, _ = worker.RunOnce(ctx)
	if got := rc.take(); len(got) != 0 {
		t.Fatalf("inactive hook delivered %d", len(got))
	}
	if code, _ := owner.do("DELETE", hookURL, nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	if code, _ := owner.do("DELETE", hookURL, nil); code != http.StatusNotFound {
		t.Fatalf("delete again: %d", code)
	}
	var actions []string
	rows, _ := pool.Query(ctx, `SELECT action FROM audit_events WHERE action LIKE 'webhook.%' AND metadata->>'webhook_id' = $1 ORDER BY id`, hook.ID)
	for rows.Next() {
		var a string
		_ = rows.Scan(&a)
		actions = append(actions, a)
	}
	rows.Close()
	if len(actions) != 5 || actions[0] != "webhook.created" || actions[4] != "webhook.deleted" {
		t.Fatalf("audit: %v", actions)
	}
}
