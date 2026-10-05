package formschema

import (
	"encoding/json"
	"testing"
)

const planExample = `{
  "sections": [
    {
      "key": "empresa",
      "title": "Datos de la empresa",
      "fields": [
        { "key": "razon_social", "type": "text", "label": "Razón social", "required": true },
        { "key": "rut", "type": "id", "idKind": "rut", "label": "RUT", "required": true },
        { "key": "tipo", "type": "select", "label": "Tipo", "options": ["SpA", "SRL", "Persona natural"] },
        { "key": "estatutos", "type": "file", "label": "Estatutos", "required": true, "accept": ["application/pdf"], "maxMb": 10,
          "showIf": { "field": "tipo", "op": "neq", "value": "Persona natural" } },
        { "key": "socios", "type": "repeater", "label": "Socios", "min": 1,
          "fields": [
            { "key": "nombre", "type": "text", "label": "Nombre", "required": true },
            { "key": "participacion", "type": "number", "label": "% participación", "min": 0, "max": 100 }
          ] }
      ]
    }
  ]
}`

func mustSchema(t *testing.T) *Schema {
	t.Helper()
	s, err := Parse([]byte(planExample))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func data(t *testing.T, js string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(js), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestValidate(t *testing.T) {
	s := mustSchema(t)
	cases := []struct {
		name  string
		data  string
		files FileCounts
		want  []string
	}{
		{"empty", `{}`, nil, []string{"razon_social", "rut", "estatutos", "socios"}},
		{"ok", `{"razon_social":"Acme","rut":"76.086.428-5","tipo":"SpA","socios":[{"nombre":"Ana","participacion":50}]}`,
			FileCounts{"estatutos": 1}, nil},
		{"hidden file skipped", `{"razon_social":"Acme","rut":"76086428-5","tipo":"Persona natural","socios":[{"nombre":"Ana"}]}`,
			nil, nil},
		{"bad rut and row errors", `{"razon_social":"Acme","rut":"76.086.428-1","tipo":"SpA","socios":[{"participacion":150}]}`,
			FileCounts{"estatutos": 1}, []string{"rut", "socios.0.nombre", "socios.0.participacion"}},
		{"bad option", `{"razon_social":"Acme","rut":"76086428-5","tipo":"LLC","socios":[{"nombre":"Ana"}]}`,
			FileCounts{"estatutos": 1}, []string{"tipo"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			errs := s.Validate(data(t, c.data), c.files)
			if len(errs) != len(c.want) {
				t.Fatalf("got %v, want keys %v", errs, c.want)
			}
			for _, k := range c.want {
				if _, ok := errs[k]; !ok {
					t.Errorf("missing error for %s in %v", k, errs)
				}
			}
		})
	}
}

func TestValidRUT(t *testing.T) {
	for in, want := range map[string]bool{
		"76.086.428-5": true, "11.111.111-1": true, "12345678-5": true, "10000013-K": true,
		"12345678-9": false, "abc": false, "": false,
	} {
		if got := ValidRUT(in); got != want {
			t.Errorf("ValidRUT(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestCleanAndFind(t *testing.T) {
	s := mustSchema(t)
	out := s.Clean(data(t, `{"razon_social":"Acme","hack":1,"estatutos":"x","socios":[{"nombre":"Ana","x":1}]}`))
	if _, ok := out["hack"]; ok {
		t.Error("unknown key kept")
	}
	if _, ok := out["estatutos"]; ok {
		t.Error("file key kept in data")
	}
	row := out["socios"].([]any)[0].(map[string]any)
	if _, ok := row["x"]; ok {
		t.Error("unknown row key kept")
	}
	if f := s.Find("socios.3.nombre"); f == nil || f.Label != "Nombre" {
		t.Errorf("Find repeater path = %v", f)
	}
	if f := s.Find("nope"); f != nil {
		t.Error("Find unknown should be nil")
	}
}
