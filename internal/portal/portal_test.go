package portal

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/gabrielventodev/formflow/api/internal/db"
	"github.com/gabrielventodev/formflow/api/internal/mailer"
	"github.com/gabrielventodev/formflow/api/internal/storage"
	"github.com/jackc/pgx/v5/pgxpool"
)

const testSchema = `{"sections":[
 {"key":"a","title":"A","fields":[
   {"key":"nombre","type":"text","label":"Nombre","required":true},
   {"key":"tipo","type":"select","label":"Tipo","options":["SpA","Persona natural"]},
   {"key":"doc","type":"file","label":"Doc","required":true,"accept":["application/pdf"],"maxMb":1,
    "showIf":{"field":"tipo","op":"neq","value":"Persona natural"}}
 ]},
 {"key":"b","title":"B","fields":[{"key":"monto","type":"number","label":"Monto","min":0}]}
]}`

type memMail struct {
	mu   sync.Mutex
	sent []mailer.Message
}

func (m *memMail) Send(_ context.Context, msg mailer.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, msg)
	return nil
}

// setup needs TEST_DATABASE_URL pointing at a throwaway database.
func setup(t *testing.T) (*httptest.Server, *pgxpool.Pool, string) {
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
	var orgID, formID, versionID string
	slug := "t-" + strings.ReplaceAll(t.Name(), "/", "-")
	must(t, pool.QueryRow(ctx, `INSERT INTO organizations (name, slug) VALUES ('T', $1 || gen_random_uuid()) RETURNING id`, slug).Scan(&orgID))
	must(t, pool.QueryRow(ctx, `INSERT INTO forms (organization_id, title, status) VALUES ($1, 'Test', 'published') RETURNING id`, orgID).Scan(&formID))
	must(t, pool.QueryRow(ctx, `INSERT INTO form_versions (form_id, version_number, schema) VALUES ($1, 1, $2) RETURNING id`, formID, testSchema).Scan(&versionID))
	_, err = pool.Exec(ctx, `UPDATE forms SET current_version_id = $2 WHERE id = $1`, formID, versionID)
	must(t, err)
	link := "link-" + orgID
	_, err = pool.Exec(ctx, `INSERT INTO form_links (form_id, token) VALUES ($1, $2)`, formID, link)
	must(t, err)

	h := &Handler{DB: pool, Store: store, Mail: &memMail{}, WebURL: "http://web", MaxUploadMB: 5}
	srv := httptest.NewServer(h.Routes())
	t.Cleanup(srv.Close)
	return srv, pool, link
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func call(t *testing.T, srv *httptest.Server, method, path, token string, body any) (int, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, srv.URL+path, r)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	must(t, err)
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func upload(t *testing.T, srv *httptest.Server, token, field, name, ctype string, content []byte) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("fieldKey", field)
	hdr := textproto.MIMEHeader{}
	hdr.Set("Content-Disposition", `form-data; name="file"; filename="`+name+`"`)
	hdr.Set("Content-Type", ctype)
	fw, _ := mw.CreatePart(hdr)
	_, _ = fw.Write(content)
	mw.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/submission/files", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	must(t, err)
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

var pdf = []byte("%PDF-1.4\n1 0 obj<<>>endobj\ntrailer<<>>\n%%EOF")

func TestPortalFlow(t *testing.T) {
	srv, pool, link := setup(t)
	ctx := context.Background()

	code, body := call(t, srv, "GET", "/links/"+link, "", nil)
	if code != 200 || body["form"].(map[string]any)["title"] != "Test" {
		t.Fatalf("getLink: %d %v", code, body)
	}
	if code, _ := call(t, srv, "GET", "/links/nope", "", nil); code != 404 {
		t.Fatalf("unknown link: %d", code)
	}
	if code, _ := call(t, srv, "POST", "/links/"+link+"/start", "", map[string]string{"email": "malo"}); code != 422 {
		t.Fatalf("bad email: %d", code)
	}
	code, body = call(t, srv, "POST", "/links/"+link+"/start", "", map[string]string{"email": "ana@empresa.cl", "name": "Ana"})
	if code != 201 {
		t.Fatalf("start: %d %v", code, body)
	}
	tok := body["accessToken"].(string)

	if code, _ := call(t, srv, "GET", "/submission", "wrong", nil); code != 401 {
		t.Fatalf("bad token: %d", code)
	}

	// Autosave drops unknown keys.
	code, _ = call(t, srv, "PUT", "/submission/data", tok, map[string]any{"data": map[string]any{"tipo": "SpA", "x": 1}})
	if code != 200 {
		t.Fatalf("save: %d", code)
	}
	_, body = call(t, srv, "GET", "/submission", tok, nil)
	if d := body["data"].(map[string]any); d["tipo"] != "SpA" || d["x"] != nil {
		t.Fatalf("saved data: %v", d)
	}

	// Submitting with missing answers lists them.
	code, body = call(t, srv, "POST", "/submission/submit", tok, nil)
	if code != 422 {
		t.Fatalf("submit incomplete: %d %v", code, body)
	}
	errs := body["errors"].(map[string]any)
	if errs["nombre"] == nil || errs["doc"] == nil {
		t.Fatalf("errors: %v", errs)
	}

	// Files: wrong type and wrong field are rejected; a PDF is accepted.
	if code, _ := upload(t, srv, tok, "doc", "x.png", "image/png", []byte("\x89PNG\r\n\x1a\n0000")); code != 415 {
		t.Fatalf("png upload: %d", code)
	}
	if code, _ := upload(t, srv, tok, "nombre", "x.pdf", "application/pdf", pdf); code != 400 {
		t.Fatalf("non-file field: %d", code)
	}
	code, body = upload(t, srv, tok, "doc", "estatutos.pdf", "application/pdf", pdf)
	if code != 201 {
		t.Fatalf("upload: %d %v", code, body)
	}
	fileID := body["id"].(string)

	req, _ := http.NewRequest("GET", srv.URL+"/submission/files/"+fileID, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	res, err := http.DefaultClient.Do(req)
	must(t, err)
	got, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !bytes.Equal(got, pdf) || res.Header.Get("Content-Type") != "application/pdf" || res.Header.Get("Content-Security-Policy") != "sandbox" {
		t.Fatalf("download: %q %v", got, res.Header)
	}

	code, body = call(t, srv, "POST", "/submission/validate", tok, map[string]string{"section": "a"})
	if code != 200 || body["valid"] != false {
		t.Fatalf("validate: %v", body)
	}

	code, body = call(t, srv, "POST", "/submission/submit", tok, map[string]any{"data": map[string]any{"nombre": "Acme", "tipo": "SpA", "monto": 10}})
	if code != 200 {
		t.Fatalf("submit: %d %v", code, body)
	}
	if code, _ := call(t, srv, "PUT", "/submission/data", tok, map[string]any{"data": map[string]any{}}); code != 409 {
		t.Fatalf("edit after submit: %d", code)
	}
	if code, _ := call(t, srv, "POST", "/submission/submit", tok, nil); code != 409 {
		t.Fatalf("double submit: %d", code)
	}

	// Reviewer asks to fix only "monto": other answers stay put.
	var subID string
	must(t, pool.QueryRow(ctx, `UPDATE submissions SET status = 'changes_requested' WHERE access_token_hash = $1 RETURNING id`, hashToken(tok)).Scan(&subID))
	var userID string
	must(t, pool.QueryRow(ctx, `INSERT INTO users (email, password_hash) VALUES ($1 || '@x.cl', 'x') RETURNING id`, subID).Scan(&userID))
	_, err = pool.Exec(ctx, `INSERT INTO review_comments (submission_id, author_user_id, field_key, body) VALUES ($1, $2, 'monto', 'Revisa el monto')`, subID, userID)
	must(t, err)

	_, body = call(t, srv, "GET", "/submission", tok, nil)
	if ef := body["editableFields"].([]any); len(ef) != 1 || ef[0] != "monto" || len(body["comments"].([]any)) != 1 {
		t.Fatalf("changes_requested view: %v", body)
	}
	call(t, srv, "PUT", "/submission/data", tok, map[string]any{"data": map[string]any{"nombre": "Otro", "monto": 99}})
	if code, _ := upload(t, srv, tok, "doc", "otro.pdf", "application/pdf", pdf); code != 409 {
		t.Fatalf("upload to locked field: %d", code)
	}
	code, _ = call(t, srv, "POST", "/submission/submit", tok, nil)
	if code != 200 {
		t.Fatalf("resubmit: %d", code)
	}
	var data map[string]any
	var open int
	must(t, pool.QueryRow(ctx, `SELECT data FROM submissions WHERE id = $1`, subID).Scan(&data))
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM review_comments WHERE submission_id = $1 AND resolved_at IS NULL`, subID).Scan(&open))
	if data["nombre"] != "Acme" || data["monto"] != float64(99) || open != 0 {
		t.Fatalf("after resubmit: %v open=%d", data, open)
	}

	var actions []string
	rows, _ := pool.Query(ctx, `SELECT action FROM audit_events WHERE submission_id = $1 ORDER BY id`, subID)
	for rows.Next() {
		var a string
		_ = rows.Scan(&a)
		actions = append(actions, a)
	}
	want := "submission.created,file.uploaded,submission.submitted,submission.resubmitted"
	if strings.Join(actions, ",") != want {
		t.Fatalf("audit: %v", actions)
	}

	// Resume rotates the token: the old one stops working.
	_, err = pool.Exec(ctx, `UPDATE submissions SET status = 'draft' WHERE id = $1`, subID)
	must(t, err)
	if code, _ := call(t, srv, "POST", "/resume", "", map[string]string{"email": "ANA@empresa.cl"}); code != 202 {
		t.Fatalf("resume: %d", code)
	}
	if code, _ := call(t, srv, "GET", "/submission", tok, nil); code != 401 {
		t.Fatalf("old token after resume: %d", code)
	}
}

func TestUnpublishedAndExpiredLinks(t *testing.T) {
	srv, pool, link := setup(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `UPDATE form_links SET expires_at = now() - interval '1 day' WHERE token = $1`, link)
	must(t, err)
	if code, _ := call(t, srv, "GET", "/links/"+link, "", nil); code != 410 {
		t.Fatalf("expired: %d", code)
	}
	_, err = pool.Exec(ctx, `UPDATE form_links SET expires_at = NULL WHERE token = $1`, link)
	must(t, err)
	_, err = pool.Exec(ctx, `UPDATE forms SET status = 'archived' WHERE id = (SELECT form_id FROM form_links WHERE token = $1)`, link)
	must(t, err)
	if code, _ := call(t, srv, "POST", "/links/"+link+"/start", "", map[string]string{"email": "a@b.cl"}); code != 410 {
		t.Fatalf("archived: %d", code)
	}
}

func TestAccepts(t *testing.T) {
	cases := []struct {
		accept   []string
		name, mt string
		want     bool
	}{
		{nil, "a.exe", "application/octet-stream", true},
		{[]string{"application/pdf"}, "a.pdf", "application/pdf", true},
		{[]string{"image/*"}, "a.jpg", "image/jpeg", true},
		{[]string{".docx"}, "a.DOCX", "application/zip", true},
		{[]string{"image/*", "application/pdf"}, "a.html", "text/html", false},
	}
	for _, c := range cases {
		if got := accepts(c.accept, c.name, c.mt); got != c.want {
			t.Errorf("accepts(%v, %s, %s) = %v", c.accept, c.name, c.mt, got)
		}
	}
}
