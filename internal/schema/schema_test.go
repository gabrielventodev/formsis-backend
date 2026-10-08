package schema

import (
	"strings"
	"testing"
)

const valid = `{
  "sections": [
    {
      "key": "empresa",
      "title": "Datos de la empresa",
      "fields": [
        { "key": "razon_social", "type": "text", "label": "Razón social", "required": true },
        { "key": "rut", "type": "id", "label": "RUT", "idKind": "rut" },
        { "key": "tipo", "type": "select", "label": "Tipo", "options": ["SpA", "SRL", "Persona natural"] },
        { "key": "estatutos", "type": "file", "label": "Estatutos", "accept": ["application/pdf"], "maxMb": 10,
          "showIf": { "field": "tipo", "op": "neq", "value": "Persona natural" } },
        { "key": "socios", "type": "repeater", "label": "Socios", "min": 1,
          "fields": [
            { "key": "nombre", "type": "text", "label": "Nombre", "required": true },
            { "key": "participacion", "type": "number", "label": "% participación", "min": 0, "max": 100 }
          ] }
      ]
    },
    {
      "key": "contacto", "title": "Contacto",
      "showIf": { "field": "tipo", "op": "eq", "value": "SpA" },
      "fields": [ { "key": "email", "type": "email", "label": "Email" } ]
    }
  ]
}`

func TestValidSchema(t *testing.T) {
	s, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if p := Validate(s); len(p) > 0 {
		t.Fatalf("expected no problems, got %+v", p)
	}
}

func TestParseRejectsUnknownProperties(t *testing.T) {
	if _, err := Parse([]byte(`{"sections":[{"key":"a","title":"A","fields":[],"colour":"red"}]}`)); err == nil {
		t.Fatal("expected error for unknown property")
	}
}

func TestValidateProblems(t *testing.T) {
	cases := []struct {
		name, schema, path, contains string
	}{
		{"no sections", `{"sections":[]}`, "sections", "al menos una sección"},
		{"empty section", `{"sections":[{"key":"a","title":"A","fields":[]}]}`, "sections[0].fields", "al menos un campo"},
		{"bad key", `{"sections":[{"key":"a","title":"A","fields":[{"key":"Nombre Completo","type":"text","label":"N"}]}]}`, "sections[0].fields[0].key", "Clave inválida"},
		{"duplicate key across sections", `{"sections":[
			{"key":"a","title":"A","fields":[{"key":"x","type":"text","label":"X"}]},
			{"key":"b","title":"B","fields":[{"key":"x","type":"text","label":"X"}]}]}`, "sections[1].fields[0].key", "repetida"},
		{"unknown type", `{"sections":[{"key":"a","title":"A","fields":[{"key":"x","type":"color","label":"X"}]}]}`, "sections[0].fields[0].type", "desconocido"},
		{"select without options", `{"sections":[{"key":"a","title":"A","fields":[{"key":"x","type":"select","label":"X"}]}]}`, "sections[0].fields[0].options", "al menos una opción"},
		{"bad regex", `{"sections":[{"key":"a","title":"A","fields":[{"key":"x","type":"text","label":"X","pattern":"(["}]}]}`, "sections[0].fields[0].pattern", "expresión regular"},
		{"min over max", `{"sections":[{"key":"a","title":"A","fields":[{"key":"x","type":"number","label":"X","min":5,"max":1}]}]}`, "sections[0].fields[0].min", "mínimo"},
		{"condition on later field", `{"sections":[{"key":"a","title":"A","fields":[
			{"key":"x","type":"text","label":"X","showIf":{"field":"y","op":"notEmpty"}},
			{"key":"y","type":"text","label":"Y"}]}]}`, "sections[0].fields[0].showIf.field", "campo anterior"},
		{"condition on itself", `{"sections":[{"key":"a","title":"A","fields":[
			{"key":"x","type":"text","label":"X","showIf":{"field":"x","op":"notEmpty"}}]}]}`, "sections[0].fields[0].showIf.field", "sí mismo"},
		{"condition without value", `{"sections":[{"key":"a","title":"A","fields":[
			{"key":"x","type":"text","label":"X"},
			{"key":"y","type":"text","label":"Y","showIf":{"field":"x","op":"eq"}}]}]}`, "sections[0].fields[1].showIf.value", "valor"},
		{"nested repeater", `{"sections":[{"key":"a","title":"A","fields":[{"key":"r","type":"repeater","label":"R","fields":[
			{"key":"q","type":"repeater","label":"Q","fields":[{"key":"z","type":"text","label":"Z"}]}]}]}]}`, "sections[0].fields[0].fields[0].type", "otro grupo"},
		{"currency without code", `{"sections":[{"key":"a","title":"A","fields":[{"key":"x","type":"currency","label":"X"}]}]}`, "sections[0].fields[0].currency", "moneda"},
		{"radio without options", `{"sections":[{"key":"a","title":"A","fields":[{"key":"x","type":"radio","label":"X"}]}]}`, "sections[0].fields[0].options", "al menos una opción"},
		{"scale too wide", `{"sections":[{"key":"a","title":"A","fields":[{"key":"x","type":"scale","label":"X","max":20}]}]}`, "sections[0].fields[0].min", "escala"},
		{"info in repeater", `{"sections":[{"key":"a","title":"A","fields":[{"key":"r","type":"repeater","label":"R","fields":[
			{"key":"i","type":"info","label":"I"}]}]}]}`, "sections[0].fields[0].fields[0].type", "separadores"},
		{"divider in repeater", `{"sections":[{"key":"a","title":"A","fields":[{"key":"r","type":"repeater","label":"R","fields":[
			{"key":"d","type":"divider"}]}]}]}`, "sections[0].fields[0].fields[0].type", "separadores"},
		{"heading without text", `{"sections":[{"key":"a","title":"A","fields":[{"key":"h","type":"heading"}]}]}`, "sections[0].fields[0].label", "etiqueta"},
		{"condition on heading", `{"sections":[{"key":"a","title":"A","fields":[
			{"key":"h","type":"heading","label":"H"},
			{"key":"y","type":"text","label":"Y","showIf":{"field":"h","op":"notEmpty"}}]}]}`, "sections[0].fields[1].showIf.field", "condición"},
		{"condition on address", `{"sections":[{"key":"a","title":"A","fields":[
			{"key":"x","type":"address","label":"X"},
			{"key":"y","type":"text","label":"Y","showIf":{"field":"x","op":"notEmpty"}}]}]}`, "sections[0].fields[1].showIf.field", "condición"},
		{"phone in a country without calling code", `{"sections":[{"key":"a","title":"A","fields":[{"key":"x","type":"phone","label":"X","defaultCountry":"AQ"}]}]}`, "sections[0].fields[0].defaultCountry", "país"},
		{"id without kind", `{"sections":[{"key":"a","title":"A","fields":[{"key":"x","type":"id","label":"X"}]}]}`, "sections[0].fields[0].idKind", "documento"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, err := Parse([]byte(c.schema))
			if err != nil {
				t.Fatal(err)
			}
			problems := Validate(s)
			for _, p := range problems {
				if p.Path == c.path && strings.Contains(p.Message, c.contains) {
					return
				}
			}
			t.Fatalf("expected problem at %s containing %q, got %+v", c.path, c.contains, problems)
		})
	}
}

func TestLivenessField(t *testing.T) {
	s, err := Parse([]byte(`{"sections":[{"key":"a","title":"A","fields":[
	  {"key":"vida","type":"liveness","label":"Verifica que eres tú","required":true},
	  {"key":"grupo","type":"repeater","label":"Grupo","fields":[{"key":"v","type":"liveness","label":"V"}]},
	  {"key":"x","type":"text","label":"X","showIf":{"field":"vida","op":"notEmpty"}}
	]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	probs := Validate(s)
	if len(probs) != 2 || probs[0].Path != "sections[0].fields[1].fields[0].type" || probs[1].Path != "sections[0].fields[2].showIf.field" {
		t.Fatalf("problems: %+v", probs)
	}

	s.Sections[0].Fields = s.Sections[0].Fields[:1]
	if errs := s.ValidateAnswers(map[string]any{}, FileCounts{}); errs["vida"] == "" {
		t.Fatal("a required liveness field needs a completed check")
	}
	if errs := s.ValidateAnswers(map[string]any{}, FileCounts{"vida": 1}); len(errs) != 0 {
		t.Fatalf("completed check: %v", errs)
	}
	// Whatever the browser sends for the field is not stored as an answer.
	if out := s.Clean(map[string]any{"vida": "ok"}); len(out) != 0 {
		t.Fatalf("clean: %v", out)
	}
}
