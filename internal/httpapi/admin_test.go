package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gabrielventodev/formsis/api/internal/auth"
	"github.com/gabrielventodev/formsis/api/internal/db"
	"github.com/gabrielventodev/formsis/api/internal/mailer"
	"github.com/gabrielventodev/formsis/api/internal/storage"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Integration tests run against a real Postgres when TEST_DATABASE_URL is set.
// Each run creates its own organization, so existing data is left alone.
// fakeMail records emails; sends happen in a goroutine, so tests read from the channel.
type fakeMail chan mailer.Message

func (f fakeMail) Send(_ context.Context, m mailer.Message) error {
	f <- m
	return nil
}

func (f fakeMail) next(t *testing.T) mailer.Message {
	t.Helper()
	select {
	case m := <-f:
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("no email sent")
		return mailer.Message{}
	}
}

func adminTestServer(t *testing.T) (*httptest.Server, *pgxpool.Pool, fakeMail) {
	t.Helper()
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
	store, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, "estatutos.pdf", strings.NewReader("%PDF-1.4 test"), 13, "application/pdf"); err != nil {
		t.Fatal(err)
	}
	mail := make(fakeMail, 10)
	srv := httptest.NewServer((&Server{DB: pool, OrgID: "unused", WebOrigin: "http://localhost:3000", Files: store,
		Mail: mail, WebURL: "https://forms.example.com", Uploads: store}).Routes())
	t.Cleanup(srv.Close)
	return srv, pool, mail
}

type client struct {
	t    *testing.T
	base string
	http *http.Client
}

func newClient(t *testing.T, base string) *client {
	jar, _ := cookiejar.New(nil)
	return &client{t: t, base: base, http: &http.Client{Jar: jar}}
}

func (c *client) do(method, path string, body any) (int, []byte) {
	c.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, rdr)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, out
}

func seed(t *testing.T, pool *pgxpool.Pool) (email, subID, fileID string) {
	t.Helper()
	ctx := context.Background()
	var orgID, userID, formID, versionID string
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	email = "admin-" + suffix + "@example.com"
	hash, err := auth.HashPassword("secreto-123")
	must(err)
	must(pool.QueryRow(ctx, `INSERT INTO organizations (name, slug) VALUES ('Acme', $1) RETURNING id`, "acme-"+suffix).Scan(&orgID))
	must(pool.QueryRow(ctx, `INSERT INTO users (email, name, password_hash) VALUES ($1, 'Ana Admin', $2) RETURNING id`, email, hash).Scan(&userID))
	_, err = pool.Exec(ctx, `INSERT INTO memberships (user_id, organization_id, role) VALUES ($1, $2, 'owner')`, userID, orgID)
	must(err)
	must(pool.QueryRow(ctx, `INSERT INTO forms (organization_id, title, status) VALUES ($1, 'Onboarding empresa', 'published') RETURNING id`, orgID).Scan(&formID))
	schema := `{"sections":[{"key":"empresa","title":"Empresa","fields":[
		{"key":"razon_social","type":"text","label":"Razón social"},
		{"key":"socios","type":"repeater","label":"Socios","fields":[{"key":"nombre","type":"text","label":"Nombre"}]},
		{"key":"docs","type":"heading","label":"Documentos"},
		{"key":"estatutos","type":"file","label":"Estatutos"}]}]}`
	must(pool.QueryRow(ctx, `INSERT INTO form_versions (form_id, version_number, schema) VALUES ($1, 1, $2) RETURNING id`, formID, schema).Scan(&versionID))
	must(pool.QueryRow(ctx, `
		INSERT INTO submissions (organization_id, form_id, form_version_id, applicant_email, applicant_name,
		                         access_token_hash, status, data, submitted_at)
		VALUES ($1, $2, $3, 'pedro@cliente.cl', 'Pedro Pérez', $4, 'submitted',
		        '{"razon_social":"=Cliente SpA","socios":[{"nombre":"Pedro"}]}', now())
		RETURNING id`, orgID, formID, versionID, "h1-"+suffix).Scan(&subID))
	_, err = pool.Exec(ctx, `
		INSERT INTO submissions (organization_id, form_id, form_version_id, applicant_email, access_token_hash)
		VALUES ($1, $2, $3, 'borrador@cliente.cl', $4)`, orgID, formID, versionID, "h2-"+suffix)
	must(err)
	must(pool.QueryRow(ctx, `
		INSERT INTO submission_files (submission_id, field_key, storage_key, filename, mime_type, size_bytes)
		VALUES ($1, 'estatutos', 'estatutos.pdf', 'Estatutos 2024.pdf', 'application/pdf', 13) RETURNING id`, subID).Scan(&fileID))
	return email, subID, fileID
}

func TestAdminReviewFlow(t *testing.T) {
	srv, pool, mail := adminTestServer(t)
	email, subID, fileID := seed(t, pool)
	c := newClient(t, srv.URL)

	if code, _ := c.do("GET", "/api/v1/admin/submissions", nil); code != http.StatusUnauthorized {
		t.Fatalf("anonymous list: got %d, want 401", code)
	}
	if code, _ := c.do("GET", "/api/v1/admin/forms", nil); code != http.StatusUnauthorized {
		t.Fatalf("anonymous form builder: got %d, want 401", code)
	}
	if code, _ := c.do("POST", "/api/v1/auth/login", map[string]string{"email": email, "password": "mala"}); code != http.StatusUnauthorized {
		t.Fatalf("bad password: got %d", code)
	}
	if code, body := c.do("POST", "/api/v1/auth/login", map[string]string{"email": strings.ToUpper(email), "password": "secreto-123"}); code != http.StatusOK {
		t.Fatalf("login: %d %s", code, body)
	}
	code, body := c.do("GET", "/api/v1/auth/me", nil)
	var me auth.Identity
	_ = json.Unmarshal(body, &me)
	if code != http.StatusOK || me.Role != "owner" || me.Name != "Ana Admin" {
		t.Fatalf("me: %d %s", code, body)
	}

	// Inbox hides drafts by default and finds by applicant name.
	var list struct {
		Items  []submissionRow `json:"items"`
		Total  int             `json:"total"`
		Counts map[string]int  `json:"counts"`
	}
	_, body = c.do("GET", "/api/v1/admin/submissions?q=p%C3%A9rez", nil)
	_ = json.Unmarshal(body, &list)
	if list.Total != 1 || list.Items[0].ID != subID || list.Items[0].FormTitle != "Onboarding empresa" {
		t.Fatalf("list: %s", body)
	}
	if list.Counts["draft"] != 1 || list.Counts["submitted"] != 1 {
		t.Fatalf("counts: %v", list.Counts)
	}
	if code, _ := c.do("GET", "/api/v1/admin/submissions?status=bogus", nil); code != http.StatusBadRequest {
		t.Fatalf("bad status filter: %d", code)
	}

	// Approving needs no comment, but going straight from submitted to an
	// unknown status is refused; changes_requested needs a reason.
	if code, _ := c.do("POST", "/api/v1/admin/submissions/"+subID+"/transition", map[string]string{"to": "draft"}); code != http.StatusConflict {
		t.Fatalf("invalid transition: %d", code)
	}
	if code, _ := c.do("POST", "/api/v1/admin/submissions/"+subID+"/transition", map[string]string{"to": "changes_requested"}); code != http.StatusBadRequest {
		t.Fatalf("changes without reason: %d", code)
	}

	if code, body := c.do("POST", "/api/v1/admin/submissions/"+subID+"/transition", map[string]string{"to": "in_review", "from": "submitted"}); code != http.StatusOK {
		t.Fatalf("start review: %d %s", code, body)
	}
	if m := mail.next(t); m.To != "pedro@cliente.cl" || !strings.HasPrefix(m.Subject, "Estamos revisando") {
		t.Fatalf("review started email: %+v", m)
	}
	// Stale UI: the client thinks it is still "submitted".
	if code, _ := c.do("POST", "/api/v1/admin/submissions/"+subID+"/transition", map[string]string{"to": "approved", "from": "submitted"}); code != http.StatusConflict {
		t.Fatalf("stale transition: %d", code)
	}
	// Display blocks collect no answer, so they cannot be flagged for correction.
	if code, _ := c.do("POST", "/api/v1/admin/submissions/"+subID+"/transition", map[string]any{
		"to": "changes_requested", "field_comments": []fieldComment{{FieldKey: "docs", Body: "Corrige el título"}},
	}); code != http.StatusBadRequest {
		t.Fatalf("changes on a display block: %d", code)
	}
	if code, _ := c.do("POST", "/api/v1/admin/submissions/"+subID+"/comments", map[string]string{"body": "Ojo", "field_key": "docs"}); code != http.StatusBadRequest {
		t.Fatalf("comment on a display block: %d", code)
	}
	code, body = c.do("POST", "/api/v1/admin/submissions/"+subID+"/transition", map[string]any{
		"to": "changes_requested", "comment": "Faltan documentos",
		"field_comments": []fieldComment{{FieldKey: "estatutos", Body: "Sube la versión firmada"}, {FieldKey: "x", Body: " "}},
	})
	if code != http.StatusOK {
		t.Fatalf("request changes: %d %s", code, body)
	}
	// The applicant gets an email naming the field and a fresh magic link.
	m := mail.next(t)
	if m.To != "pedro@cliente.cl" || !strings.Contains(m.Text, "Estatutos: Sube la versión firmada") ||
		!strings.Contains(m.Text, "https://forms.example.com/s/") || !strings.Contains(m.Text, "Faltan documentos") {
		t.Fatalf("changes email: %+v", m)
	}
	var hash string
	if err := pool.QueryRow(context.Background(), `SELECT access_token_hash FROM submissions WHERE id = $1`, subID).Scan(&hash); err != nil || strings.HasPrefix(hash, "h1-") {
		t.Fatalf("access token not rotated: %q %v", hash, err)
	}

	if code, _ := c.do("POST", "/api/v1/admin/submissions/"+subID+"/comments", map[string]string{"body": "Llamé al cliente"}); code != http.StatusCreated {
		t.Fatalf("comment: %d", code)
	}
	if code, _ := c.do("PUT", "/api/v1/admin/submissions/"+subID+"/assignee", map[string]any{"user_id": nil}); code != http.StatusNoContent {
		t.Fatalf("unassign: %d", code)
	}
	if code, _ := c.do("PUT", "/api/v1/admin/submissions/"+subID+"/assignee", map[string]any{"user_id": me.UserID}); code != http.StatusNoContent {
		t.Fatalf("assign: %d", code)
	}

	// Applicant resubmits (portal side), then the reviewer approves.
	if _, err := pool.Exec(context.Background(), `UPDATE submissions SET status = 'submitted' WHERE id = $1`, subID); err != nil {
		t.Fatal(err)
	}
	if code, body := c.do("POST", "/api/v1/admin/submissions/"+subID+"/transition", map[string]string{"to": "approved"}); code != http.StatusOK {
		t.Fatalf("approve: %d %s", code, body)
	}
	if m := mail.next(t); !strings.HasPrefix(m.Subject, "Solicitud aprobada") {
		t.Fatalf("approve email: %+v", m)
	}

	var detail struct {
		Submission submissionRow `json:"submission"`
		Comments   []commentRow  `json:"comments"`
		Events     []eventRow    `json:"events"`
		Files      []fileRow     `json:"files"`
		Allowed    []string      `json:"allowed_transitions"`
		Schema     formSchema    `json:"schema"`
	}
	code, body = c.do("GET", "/api/v1/admin/submissions/"+subID, nil)
	if code != http.StatusOK {
		t.Fatalf("detail: %d %s", code, body)
	}
	_ = json.Unmarshal(body, &detail)
	if detail.Submission.Status != "approved" || detail.Submission.DecidedAt == nil {
		t.Fatalf("status after approve: %+v", detail.Submission)
	}
	if detail.Submission.AssignedTo == nil || detail.Submission.AssignedTo.ID != me.UserID {
		t.Fatalf("assignee: %+v", detail.Submission.AssignedTo)
	}
	if len(detail.Comments) != 3 || detail.Comments[1].FieldKey == nil || *detail.Comments[1].FieldKey != "estatutos" {
		t.Fatalf("comments: %s", body)
	}
	var actions []string
	for _, e := range detail.Events {
		actions = append(actions, e.Action)
	}
	if got := strings.Join(actions, ","); got != "review_started,changes_requested,commented,assigned,assigned,approved" {
		t.Fatalf("audit trail: %s", got)
	}
	if detail.Events[0].ActorName != "Ana Admin" {
		t.Fatalf("actor name: %q", detail.Events[0].ActorName)
	}
	if strings.Join(detail.Allowed, ",") != "in_review" || len(detail.Files) != 1 || len(detail.Schema.Sections) != 1 {
		t.Fatalf("detail extras: %s", body)
	}

	// Resolve the field comment.
	fieldCommentID := detail.Comments[1].ID
	if code, _ := c.do("POST", "/api/v1/admin/submissions/"+subID+"/comments/"+fieldCommentID+"/resolve", nil); code != http.StatusNoContent {
		t.Fatalf("resolve: %d", code)
	}
	if code, _ := c.do("POST", "/api/v1/admin/submissions/"+subID+"/comments/"+fieldCommentID+"/resolve", nil); code != http.StatusNotFound {
		t.Fatalf("resolve twice: %d", code)
	}

	// File download.
	code, body = c.do("GET", "/api/v1/admin/submissions/"+subID+"/files/"+fileID, nil)
	if code != http.StatusOK || string(body) != "%PDF-1.4 test" {
		t.Fatalf("download: %d %q", code, body)
	}

	// CSV export flattens the form's fields and neutralizes formulas.
	code, body = c.do("GET", "/api/v1/admin/submissions/export.csv?form_id="+detail.Submission.FormID, nil)
	csvText := string(body)
	if code != http.StatusOK || !strings.Contains(csvText, "Razón social") || !strings.Contains(csvText, "'=Cliente SpA") ||
		!strings.Contains(csvText, "Aprobado") || strings.Contains(csvText, "borrador@cliente.cl") {
		t.Fatalf("csv: %d %s", code, csvText)
	}

	// Logout invalidates the session.
	if code, _ := c.do("POST", "/api/v1/auth/logout", nil); code != http.StatusNoContent {
		t.Fatalf("logout: %d", code)
	}
	if code, _ := c.do("GET", "/api/v1/auth/me", nil); code != http.StatusUnauthorized {
		t.Fatalf("me after logout: %d", code)
	}
}

func TestReviewerCannotReopen(t *testing.T) {
	if got := allowedTransitions("approved", "reviewer"); len(got) != 0 {
		t.Fatalf("reviewer reopen: %v", got)
	}
	if got := allowedTransitions("approved", "admin"); len(got) != 1 || got[0] != "in_review" {
		t.Fatalf("admin reopen: %v", got)
	}
}

func TestPasswordHash(t *testing.T) {
	h, err := auth.HashPassword("clave")
	if err != nil {
		t.Fatal(err)
	}
	if !auth.VerifyPassword("clave", h) || auth.VerifyPassword("otra", h) || auth.VerifyPassword("clave", "basura") {
		t.Fatal("password verification mismatch")
	}
}

func TestLoginLockoutPerEmail(t *testing.T) {
	srv, pool, _ := adminTestServer(t)
	email, _, _ := seed(t, pool)
	c := newClient(t, srv.URL)
	for i := 0; i < 10; i++ {
		if code, _ := c.do("POST", "/api/v1/auth/login", map[string]string{"email": email, "password": "mala"}); code != http.StatusUnauthorized {
			t.Fatalf("failed login %d: got %d", i, code)
		}
	}
	if code, _ := c.do("POST", "/api/v1/auth/login", map[string]string{"email": email, "password": "secreto-123"}); code != http.StatusTooManyRequests {
		t.Fatalf("login after 10 failures: got %d, want 429", code)
	}
}
