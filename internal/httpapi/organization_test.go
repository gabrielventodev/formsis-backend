package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gabrielventodev/formflow/api/internal/storage"
)

func (c *client) upload(path, filename string, data []byte) (int, []byte) {
	c.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", filename)
	_, _ = fw.Write(data)
	_ = mw.Close()
	req, _ := http.NewRequest("POST", c.base+path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	res, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, out
}

func TestOrganizationBranding(t *testing.T) {
	_, pool, _ := adminTestServer(t) // migrations and pool
	ownerEmail, subID, _ := seed(t, pool)
	var orgID string
	if err := pool.QueryRow(context.Background(), `SELECT organization_id FROM submissions WHERE id = $1`, subID).Scan(&orgID); err != nil {
		t.Fatal(err)
	}
	// One store shared by the admin API and the public (portal-facing) one.
	store, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer((&Server{DB: pool, OrgID: orgID, Uploads: store}).Routes())
	defer srv.Close()
	owner := newClient(t, srv.URL)
	if code, _ := owner.do("POST", "/api/v1/auth/login", map[string]string{"email": ownerEmail, "password": "secreto-123"}); code != http.StatusOK {
		t.Fatal("login")
	}
	rita, _ := addMember(t, pool, srv.URL, subID, "Rita", "reviewer")
	if code, _ := rita.do("PUT", "/api/v1/admin/organization", map[string]string{"name": "X"}); code != http.StatusForbidden {
		t.Fatalf("reviewer edits branding: %d", code)
	}

	var org brandingOut
	code, body := owner.do("GET", "/api/v1/admin/organization", nil)
	_ = json.Unmarshal(body, &org)
	if code != http.StatusOK || org.Name != "Acme" || org.PrimaryColor != "#18181b" || org.LogoURL != nil {
		t.Fatalf("defaults: %d %s", code, body)
	}
	for _, bad := range []map[string]string{{"primary_color": "#ffff00"}, {"primary_color": "azul"}, {"name": " "}, {"support_email": "no"}} {
		if code, body := owner.do("PUT", "/api/v1/admin/organization", bad); code != http.StatusBadRequest {
			t.Fatalf("%v accepted: %d %s", bad, code, body)
		}
	}
	code, body = owner.do("PUT", "/api/v1/admin/organization", map[string]string{
		"name": "Acme Finanzas", "primary_color": "#1D4ED8", "support_email": "Ayuda@Acme.cl"})
	_ = json.Unmarshal(body, &org)
	if code != http.StatusOK || org.PrimaryColor != "#1d4ed8" || org.SupportEmail != "ayuda@acme.cl" || org.Name != "Acme Finanzas" {
		t.Fatalf("update: %d %s", code, body)
	}

	// Logo: raster images only.
	if code, _ := owner.upload("/api/v1/admin/organization/logo", "logo.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)); code != http.StatusUnsupportedMediaType {
		t.Fatalf("svg logo: %d", code)
	}
	var pngBuf bytes.Buffer
	_ = png.Encode(&pngBuf, image.NewRGBA(image.Rect(0, 0, 4, 4)))
	code, body = owner.upload("/api/v1/admin/organization/logo", "logo.png", pngBuf.Bytes())
	_ = json.Unmarshal(body, &org)
	if code != http.StatusOK || org.LogoURL == nil || !strings.HasPrefix(*org.LogoURL, "/api/v1/branding/logo?v=") {
		t.Fatalf("upload logo: %d %s", code, body)
	}

	// The portal reads it without a session.
	anon := newClient(t, srv.URL)
	code, body = anon.do("GET", "/api/v1/branding", nil)
	if code != http.StatusOK || !strings.Contains(string(body), `"primary_color":"#1d4ed8"`) || !strings.Contains(string(body), `"name":"Acme Finanzas"`) {
		t.Fatalf("public branding: %d %s", code, body)
	}
	res, err := http.Get(srv.URL + *org.LogoURL)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "image/png" || !bytes.Equal(got, pngBuf.Bytes()) {
		t.Fatalf("logo: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}

	if code, body := owner.do("DELETE", "/api/v1/admin/organization/logo", nil); code != http.StatusOK || !strings.Contains(string(body), `"logo_url":null`) {
		t.Fatalf("delete logo: %d %s", code, body)
	}
	_, act := owner.do("GET", "/api/v1/admin/activity?scope=team", nil)
	if strings.Count(string(act), "organization.updated") != 3 {
		t.Fatalf("activity: %s", act)
	}
}
