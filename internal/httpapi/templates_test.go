package httpapi

import (
	"net/http"
	"testing"

	"github.com/gabrielventodev/formsis/api/internal/forms"
)

func TestFormTemplates(t *testing.T) {
	h := testServer(t)
	const base = "/api/v1/admin/forms"

	var list []forms.TemplateInfo
	call(t, h, http.MethodGet, base+"/templates", nil, http.StatusOK, &list)
	keys := map[string]forms.TemplateInfo{}
	for _, ti := range list {
		keys[ti.Key] = ti
	}
	for _, k := range []string{"kyb-empresa", "kyc-persona"} {
		if keys[k].Fields == 0 {
			t.Fatalf("template %s missing or empty: %+v", k, list)
		}
	}

	// Default title, then a custom one; both are drafts that publish as is.
	var f forms.Form
	call(t, h, http.MethodPost, base+"/templates/kyb-empresa", nil, http.StatusCreated, &f)
	if f.Status != "draft" || f.Title != keys["kyb-empresa"].Title || len(f.Schema) < 100 {
		t.Fatalf("unexpected form: %+v", f)
	}
	call(t, h, http.MethodPost, base+"/"+f.ID+"/publish", nil, http.StatusOK, nil)
	call(t, h, http.MethodDelete, base+"/"+f.ID, nil, http.StatusConflict, nil)

	var g forms.Form
	call(t, h, http.MethodPost, base+"/templates/kyc-persona", map[string]string{"title": "  Alta de clientes  "}, http.StatusCreated, &g)
	if g.Title != "Alta de clientes" {
		t.Fatalf("title = %q", g.Title)
	}
	call(t, h, http.MethodDelete, base+"/"+g.ID, nil, http.StatusNoContent, nil)

	call(t, h, http.MethodPost, base+"/templates/nope", nil, http.StatusNotFound, nil)
}
