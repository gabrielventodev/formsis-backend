package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gabrielventodev/formsis/api/internal/auth"
	"github.com/gabrielventodev/formsis/api/internal/db"
	"github.com/gabrielventodev/formsis/api/internal/forms"
)

// Runs against a real Postgres when TEST_DATABASE_URL is set, e.g.
// TEST_DATABASE_URL=postgres://formsis:formsis@localhost:5432/formsis?sslmode=disable
func testServer(t *testing.T) http.Handler {
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
	org, err := forms.DefaultOrganization(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	// Admin routes need a session: log in once and attach the cookie to every request.
	store := &auth.Store{DB: pool}
	if _, err := store.EnsureAdmin(ctx, "forms-test@example.com", "secreto-123", "Test", "Test"); err != nil {
		t.Fatal(err)
	}
	token, _, err := store.Login(ctx, "forms-test@example.com", "secreto-123")
	if err != nil {
		t.Fatal(err)
	}
	h := (&Server{DB: pool, WebOrigin: "http://localhost:3000", OrgID: org}).Routes()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
		h.ServeHTTP(w, r)
	})
}

func call(t *testing.T, h http.Handler, method, path string, body any, wantStatus int, out any) {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != wantStatus {
		t.Fatalf("%s %s: status %d, want %d: %s", method, path, rec.Code, wantStatus, rec.Body.String())
	}
	if out != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFormLifecycle(t *testing.T) {
	h := testServer(t)
	const base = "/api/v1/admin/forms"

	var f forms.Form
	call(t, h, "POST", base, map[string]string{"title": "Onboarding empresa"}, http.StatusCreated, &f)
	if f.Status != "draft" || f.Current != nil || !f.HasUnpublishedChanges {
		t.Fatalf("unexpected new form: %+v", f)
	}

	// The starter section has no fields, so publishing is blocked with problems.
	var errBody struct {
		Problems []map[string]string `json:"problems"`
	}
	call(t, h, "POST", base+"/"+f.ID+"/publish", nil, http.StatusUnprocessableEntity, &errBody)
	if len(errBody.Problems) == 0 {
		t.Fatal("expected problems")
	}

	schemaV1 := json.RawMessage(`{"sections":[{"key":"empresa","title":"Empresa","fields":[
		{"key":"razon_social","type":"text","label":"Razón social","required":true}]}]}`)
	call(t, h, "PATCH", base+"/"+f.ID, map[string]any{"schema": schemaV1}, http.StatusOK, &f)
	call(t, h, "POST", base+"/"+f.ID+"/publish", nil, http.StatusOK, &f)
	if f.Status != "published" || f.Current == nil || f.Current.Number != 1 || f.HasUnpublishedChanges {
		t.Fatalf("unexpected after publish: %+v", f)
	}
	call(t, h, "POST", base+"/"+f.ID+"/publish", nil, http.StatusConflict, nil)

	// Editing after publishing changes only the draft; version 1 stays frozen.
	schemaV2 := json.RawMessage(`{"sections":[{"key":"empresa","title":"Empresa","fields":[
		{"key":"razon_social","type":"text","label":"Razón social","required":true},
		{"key":"email","type":"email","label":"Email"}]}]}`)
	call(t, h, "PATCH", base+"/"+f.ID, map[string]any{"schema": schemaV2}, http.StatusOK, &f)
	if !f.HasUnpublishedChanges || f.Current.Number != 1 {
		t.Fatalf("draft edit should leave v1 current: %+v", f)
	}
	call(t, h, "POST", base+"/"+f.ID+"/publish", nil, http.StatusOK, &f)
	if f.Current.Number != 2 {
		t.Fatalf("want version 2, got %+v", f.Current)
	}
	var v1 forms.Version
	call(t, h, "GET", base+"/"+f.ID+"/versions/1", nil, http.StatusOK, &v1)
	if bytes.Contains(v1.Schema, []byte(`"email"`)) {
		t.Fatal("version 1 changed after a later publish")
	}
	var versions []forms.VersionInfo
	call(t, h, "GET", base+"/"+f.ID+"/versions", nil, http.StatusOK, &versions)
	if len(versions) != 2 {
		t.Fatalf("want 2 versions, got %d", len(versions))
	}

	// Published forms can't be deleted; archive and restore instead.
	call(t, h, "DELETE", base+"/"+f.ID, nil, http.StatusConflict, nil)
	call(t, h, "POST", base+"/"+f.ID+"/archive", nil, http.StatusOK, &f)
	call(t, h, "PATCH", base+"/"+f.ID, map[string]any{"title": "x"}, http.StatusConflict, nil)
	call(t, h, "POST", base+"/"+f.ID+"/restore", nil, http.StatusOK, &f)
	if f.Status != "published" {
		t.Fatalf("restore should return to published, got %s", f.Status)
	}

	var dup forms.Form
	call(t, h, "POST", base+"/"+f.ID+"/duplicate", nil, http.StatusCreated, &dup)
	if dup.Status != "draft" || dup.Current != nil || dup.Title != "Onboarding empresa (copia)" {
		t.Fatalf("unexpected duplicate: %+v", dup)
	}
	call(t, h, "DELETE", base+"/"+dup.ID, nil, http.StatusNoContent, nil)
	call(t, h, "GET", base+"/"+dup.ID, nil, http.StatusNotFound, nil)
	call(t, h, "GET", base+"/not-a-uuid", nil, http.StatusNotFound, nil)

	// Malformed schemas are rejected even for drafts.
	call(t, h, "PATCH", base+"/"+f.ID, map[string]any{"schema": map[string]any{"sections": "x"}}, http.StatusBadRequest, nil)
}
