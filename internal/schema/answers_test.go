package schema

import (
	"encoding/json"
	"testing"
)

const kyb = `{
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
    },
    {
      "key": "extra", "title": "Extra", "showIf": { "field": "tipo", "op": "eq", "value": "SRL" },
      "fields": [ { "key": "gerente", "type": "text", "label": "Gerente", "required": true } ]
    }
  ]
}`

func answers(t *testing.T, js string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(js), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestValidateAnswers(t *testing.T) {
	s, err := Parse([]byte(kyb))
	if err != nil {
		t.Fatal(err)
	}
	if p := Validate(s); len(p) > 0 {
		t.Fatalf("test schema is not publishable: %v", p)
	}
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
		{"conditional section", `{"razon_social":"Acme","rut":"76086428-5","tipo":"SRL","socios":[{"nombre":"Ana"}]}`,
			FileCounts{"estatutos": 1}, []string{"gerente"}},
		{"row errors", `{"razon_social":"Acme","rut":"76.086.428-1","tipo":"SpA","socios":[{"participacion":"150"}]}`,
			FileCounts{"estatutos": 1}, []string{"rut", "socios.0.nombre", "socios.0.participacion"}},
		{"bad option", `{"razon_social":"Acme","rut":"76086428-5","tipo":"LLC","socios":[{"nombre":"Ana"}]}`,
			FileCounts{"estatutos": 1}, []string{"tipo"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			errs := s.ValidateAnswers(answers(t, c.data), c.files)
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

func TestEvaluate(t *testing.T) {
	a := map[string]any{"tipo": "SpA", "productos": []any{"Crédito"}, "acepta": true, "monto": float64(10)}
	cases := []struct {
		c    Condition
		want bool
	}{
		{Condition{Field: "tipo", Op: "eq", Value: "SpA"}, true},
		{Condition{Field: "tipo", Op: "neq", Value: "SpA"}, false},
		{Condition{Field: "productos", Op: "eq", Value: "Crédito"}, true},
		{Condition{Field: "tipo", Op: "contains", Value: "sp"}, true},
		{Condition{Field: "acepta", Op: "eq", Value: true}, true},
		{Condition{Field: "nada", Op: "eq", Value: false}, true},
		{Condition{Field: "nada", Op: "empty"}, true},
		{Condition{Field: "monto", Op: "eq", Value: "10"}, true},
	}
	for _, c := range cases {
		if got := Evaluate(&c.c, a); got != c.want {
			t.Errorf("Evaluate(%+v) = %v", c.c, got)
		}
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
	s, _ := Parse([]byte(kyb))
	out := s.Clean(answers(t, `{"razon_social":"Acme","hack":1,"estatutos":"x","socios":[{"nombre":"Ana","x":1}]}`))
	if _, ok := out["hack"]; ok {
		t.Error("unknown key kept")
	}
	if _, ok := out["estatutos"]; ok {
		t.Error("file key kept in data")
	}
	if row := out["socios"].([]any)[0].(map[string]any); row["x"] != nil {
		t.Error("unknown row key kept")
	}
	if f := s.Find("socios.3.nombre"); f == nil || f.Label != "Nombre" {
		t.Errorf("Find repeater path = %v", f)
	}
	for _, p := range []string{"nope", "socios.x.nombre", "razon_social.0.x"} {
		if s.Find(p) != nil {
			t.Errorf("Find(%q) should be nil", p)
		}
	}
}
